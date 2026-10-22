// Package devissuer is a development-only JWT issuer: it mints RS256 tokens for any tenant,
// agent and scopes and publishes its JWKS. It refuses to start unless explicitly enabled
// (MCPGW_DEV_ISSUER_ENABLED=true) and must never run in production.
package devissuer

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// Token contract values.
const (
	Issuer     = "mcp-gateway dev-issuer"
	Audience   = "mcp-gateway"
	DefaultTTL = 30 * time.Minute
	MaxTTL     = time.Hour
)

// ErrDisabled is returned when the issuer was not explicitly enabled.
var ErrDisabled = errors.New("dev issuer is disabled (set MCPGW_DEV_ISSUER_ENABLED=true for development only)")

var (
	idPattern    = regexp.MustCompile(`^(\*|[A-Za-z0-9._@-]{1,64})$`)
	scopePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z:]+$`)
)

// Persona is a sign-in identity for the console.
type Persona struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tenant      string   `json:"tenant"`
	Subject     string   `json:"subject"`
	Scopes      []string `json:"scopes"`
}

// Personas are the console's dev sign-in identities (the user roles).
func Personas() []Persona {
	return []Persona{
		{ID: "platform-engineer", Title: "Platform engineer", Description: "Operates the gateway, reloads and tests policies",
			Tenant: "*", Subject: "alex.platform", Scopes: []string{"mcpgw:admin", "mcpgw:audit:read"}},
		{ID: "security-reviewer", Title: "Security reviewer", Description: "Reads and verifies the audit log",
			Tenant: "*", Subject: "sam.security", Scopes: []string{"mcpgw:audit:read"}},
		{ID: "approver", Title: "Approver (t_alpha)", Description: "Approves or rejects held calls for tenant t_alpha",
			Tenant: "t_alpha", Subject: "jo.approver", Scopes: []string{"mcpgw:approve", "mcpgw:audit:read"}},
	}
}

// Options configure the issuer.
type Options struct {
	Enabled bool
	KeyFile string // PEM RSA private key; generated when empty
	Clock   clock.Clock
}

// Server mints tokens and serves the JWKS.
type Server struct {
	key   *rsa.PrivateKey
	kid   string
	clock clock.Clock
}

// New creates an issuer, or returns ErrDisabled.
func New(opts Options) (*Server, error) {
	if !opts.Enabled {
		return nil, ErrDisabled
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	key, err := loadOrGenerate(opts.KeyFile)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return &Server{key: key, kid: hex.EncodeToString(sum[:8]), clock: opts.Clock}, nil
}

func loadOrGenerate(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		return key, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-provided key path
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("key file is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not RSA")
	}
	return rk, nil
}

// Mint signs a token for tenant, agent and scopes, valid for ttl (0 = DefaultTTL, at most MaxTTL).
func (i *Server) Mint(tenant, agent string, scopes []string, ttl time.Duration) (string, time.Duration, error) {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return "", 0, fmt.Errorf("ttl must be within (0, %s]", MaxTTL)
	}
	if !idPattern.MatchString(tenant) || !idPattern.MatchString(agent) || agent == "*" {
		return "", 0, errors.New("tenant and agent must match [A-Za-z0-9._@-]{1,64}")
	}
	for _, s := range scopes {
		if !scopePattern.MatchString(s) {
			return "", 0, fmt.Errorf("invalid scope %q", s)
		}
	}
	if scopes == nil {
		scopes = []string{}
	}
	now := i.clock.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": Issuer, "aud": Audience, "sub": agent, "tenant": tenant, "scopes": scopes,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(ttl).Unix(),
	})
	tok.Header["kid"] = i.kid
	s, err := tok.SignedString(i.key)
	if err != nil {
		return "", 0, fmt.Errorf("sign: %w", err)
	}
	return s, ttl, nil
}

// Handler serves /.well-known/jwks.json, POST /token, GET /personas and /healthz.
func (i *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": i.kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tenant     string   `json:"tenant"`
			Agent      string   `json:"agent"`
			Scopes     []string `json:"scopes"`
			TTLSeconds int      `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		tok, ttl, err := i.Mint(req.Tenant, req.Agent, req.Scopes, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": int(ttl.Seconds())})
	})
	mux.HandleFunc("GET /personas", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, Personas()) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
