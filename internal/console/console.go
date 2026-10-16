// Package console is the server-rendered admin console on :8200 (/console): sign-in with dev
// personas, audit log and event pages, approvals, policy tester and upstreams. Sessions are
// server-side (HttpOnly, SameSite=Strict cookie), every POST carries a per-session CSRF token,
// responses send a strict CSP, and at most 3 KB of optional JavaScript is used.
package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/admin"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/audit"
	"github.com/ROBERTG99/mcp-gateway/internal/authn"
	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/policy"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// TokenSource obtains a token for a persona (the dev issuer in development).
type TokenSource func(ctx context.Context, p devissuer.Persona) (string, error)

// Deps are the console's collaborators (the same services as the admin API).
type Deps struct {
	Verifier  *authn.Verifier
	Audit     audit.Reader
	Approvals *approvals.Service
	Policy    *policy.Engine
	Catalog   admin.CatalogStatus
	Flush     func(ctx context.Context) error
	Tokens    TokenSource
	Personas  []devissuer.Persona // default devissuer.Personas()
}

// Options tune the console.
type Options struct {
	SecureCookies bool
	Clock         clock.Clock
	Logger        *slog.Logger
}

// Console serves /console.
type Console struct {
	deps  Deps
	opts  Options
	api   *admin.API
	pages map[string]*template.Template

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	principal authn.Principal
	persona   devissuer.Persona
	csrf      string
	expires   time.Time
}

const (
	sessionCookie = "mcpgw_session"
	csrfCookie    = "mcpgw_csrf"
	cookiePath    = "/console"
)

// New builds the console.
func New(deps Deps, opts Options) (*Console, error) {
	if deps.Personas == nil {
		deps.Personas = devissuer.Personas()
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Console{deps: deps, opts: opts, sessions: map[string]*session{}, pages: map[string]*template.Template{},
		api: admin.New(admin.Deps{Audit: deps.Audit, Approvals: deps.Approvals, Policy: deps.Policy, Catalog: deps.Catalog, Flush: deps.Flush})}
	base := template.Must(template.New("layout").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("console templates: %w", err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t, err := template.Must(base.Clone()).ParseFS(templateFS, e)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e, err)
		}
		c.pages[name] = t
	}
	return c, nil
}

// Handler serves every /console route.
func (c *Console) Handler() http.Handler {
	r := chi.NewRouter()
	c.Mount(r)
	return r
}

// Mount registers the console routes on r.
func (c *Console) Mount(r chi.Router) {
	static, _ := fs.Sub(staticFS, "static")
	r.Route("/console", func(r chi.Router) {
		r.Use(securityHeaders)
		r.Handle("/static/*", http.StripPrefix("/console/static/", cacheStatic(http.FileServer(http.FS(static)))))
		r.Get("/signin", c.signinPage)
		r.Post("/signin", c.signin)
		r.Group(func(r chi.Router) {
			r.Use(c.requireSession)
			r.Get("/", c.home)
			r.Post("/signout", c.signout)
			c.mountPages(r)
		})
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func equal(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

type ctxKey struct{}

func (c *Console) lookup(r *http.Request) *session {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[ck.Value]
	if !ok {
		return nil
	}
	if !c.opts.Clock.Now().Before(s.expires) {
		delete(c.sessions, ck.Value)
		return nil
	}
	return s
}

func (c *Console) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := c.lookup(r)
		if s == nil {
			http.Redirect(w, r, "/console/signin", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || !equal(r.PostForm.Get("csrf"), s.csrf) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(authn.WithPrincipal(r.Context(), s.principal), ctxKey{}, s)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func current(r *http.Request) *session {
	s, _ := r.Context().Value(ctxKey{}).(*session)
	return s
}

func (c *Console) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	// Secure is MCPGW_CONSOLE_SECURE_COOKIES: on behind TLS, off for the local HTTP dev stack.
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: cookiePath, HttpOnly: true, Secure: c.opts.SecureCookies, //nolint:gosec // G124: see above
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (c *Console) signinPage(w http.ResponseWriter, r *http.Request) {
	if c.lookup(r) != nil {
		http.Redirect(w, r, "/console/audit", http.StatusSeeOther)
		return
	}
	token := randomToken()
	c.setCookie(w, csrfCookie, token, 900)
	c.render(w, r, http.StatusOK, "signin", view{Title: "Sign in", CSRF: token, Data: c.deps.Personas})
}

func (c *Console) signin(w http.ResponseWriter, r *http.Request) {
	ck, err := r.Cookie(csrfCookie)
	if err != nil || r.ParseForm() != nil || !equal(r.PostForm.Get("csrf"), ck.Value) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	var persona *devissuer.Persona
	for i := range c.deps.Personas {
		if c.deps.Personas[i].ID == r.PostForm.Get("persona") {
			persona = &c.deps.Personas[i]
		}
	}
	if persona == nil {
		http.Error(w, "unknown persona", http.StatusBadRequest)
		return
	}
	tok, err := c.deps.Tokens(r.Context(), *persona)
	var p authn.Principal
	if err == nil {
		p, err = c.deps.Verifier.Verify(r.Context(), tok)
	}
	if err != nil {
		c.opts.Logger.Warn("console sign-in failed", "persona", persona.ID, "error", err)
		c.render(w, r, http.StatusBadGateway, "signin", view{Title: "Sign in", CSRF: ck.Value, Data: c.deps.Personas,
			Error: "The token issuer is unavailable.", RetryURL: "/console/signin"})
		return
	}
	id := randomToken()
	c.mu.Lock()
	c.sessions[id] = &session{principal: p, persona: *persona, csrf: randomToken(), expires: p.ExpiresAt}
	c.mu.Unlock()
	c.setCookie(w, csrfCookie, "", -1)
	c.setCookie(w, sessionCookie, id, int(time.Until(p.ExpiresAt).Seconds()))
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func (c *Console) signout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		c.mu.Lock()
		delete(c.sessions, ck.Value)
		c.mu.Unlock()
	}
	c.setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/console/signin", http.StatusSeeOther)
}

func (c *Console) home(w http.ResponseWriter, r *http.Request) {
	if current(r).principal.HasScope(admin.ScopeApprove) {
		http.Redirect(w, r, "/console/approvals", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/console/audit", http.StatusSeeOther)
}

// view is the data every page template receives.
type view struct {
	Title        string
	Nav          string
	Principal    *authn.Principal
	PersonaTitle string
	CSRF         string
	Flash        string
	Error        string
	RetryURL     string
	Version      string
	Data         any
}

func (c *Console) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	if s := current(r); s != nil {
		p := s.principal
		v.Principal, v.PersonaTitle, v.CSRF = &p, s.persona.Title, s.csrf
	}
	v.Version = version.Version()
	t, ok := c.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", v); err != nil {
		c.opts.Logger.Error("render console page", "page", page, "error", err)
	}
}

var funcs = template.FuncMap{
	"ts": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05Z") },
	"tsp": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12] + "…"
		}
		return s
	},
	"has": func(p *authn.Principal, scope string) bool { return p != nil && p.HasScope(scope) },
}
