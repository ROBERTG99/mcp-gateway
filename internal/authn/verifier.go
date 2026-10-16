package authn

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// Contract limits (spec §4.4).
const (
	MaxLifetime = time.Hour
	ClockSkew   = 30 * time.Second
)

// ErrInvalidToken is wrapped by every verification failure.
var ErrInvalidToken = errors.New("invalid token")

// Options configure a Verifier.
type Options struct {
	JWKSURL    string
	Issuer     string
	Audience   string
	CacheTTL   time.Duration // how long fetched keys are considered fresh (default 5 min)
	StaleLimit time.Duration // how long cached keys are used while the issuer is unreachable (default 1 h)
	HTTPClient *http.Client
	Clock      clock.Clock
}

// Verifier validates RS256 JWTs against a JWKS.
type Verifier struct {
	opts Options

	flight singleflight.Group

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	backoff     time.Duration
	refreshing  bool
}

// NewVerifier returns a verifier; keys are fetched lazily.
func NewVerifier(opts Options) *Verifier {
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	if opts.StaleLimit == 0 {
		opts.StaleLimit = time.Hour
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	return &Verifier{opts: opts}
}

type claims struct {
	jwt.RegisteredClaims
	Tenant string   `json:"tenant"`
	Scopes []string `json:"scopes"`
}

// Verify parses and validates a token and returns its principal.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	var c claims
	parsed, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("kid header required")
		}
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(v.opts.Issuer),
		jwt.WithAudience(v.opts.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(ClockSkew),
		jwt.WithTimeFunc(v.opts.Clock.Now),
	)
	if err != nil || !parsed.Valid {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	now := v.opts.Clock.Now()
	exp := c.ExpiresAt.Time
	if exp.Sub(now) > MaxLifetime+ClockSkew {
		return Principal{}, fmt.Errorf("%w: lifetime exceeds %s", ErrInvalidToken, MaxLifetime)
	}
	if c.IssuedAt != nil && exp.Sub(c.IssuedAt.Time) > MaxLifetime {
		return Principal{}, fmt.Errorf("%w: lifetime exceeds %s", ErrInvalidToken, MaxLifetime)
	}
	if c.Subject == "" || c.Tenant == "" {
		return Principal{}, fmt.Errorf("%w: sub and tenant claims are required", ErrInvalidToken)
	}
	return Principal{Tenant: c.Tenant, Agent: c.Subject, Scopes: c.Scopes, Issuer: c.Issuer, ExpiresAt: exp}, nil
}

// key returns the public key for kid. Known keys are served from the cache, without waiting on
// the issuer, until StaleLimit; once older than CacheTTL a single background refresh is started.
// Unknown kids (or keys past StaleLimit) need a synchronous fetch, which is single-flight and
// throttled after failures, so a hung issuer never serialises verification.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now := v.opts.Clock.Now()
	v.mu.Lock()
	k, known := v.keys[kid]
	age := now.Sub(v.fetchedAt)
	switch {
	case known && age < v.opts.CacheTTL:
		v.mu.Unlock()
		return k, nil
	case known && age < v.opts.StaleLimit:
		v.mu.Unlock()
		v.refreshAsync()
		return k, nil
	}
	throttled := now.Sub(v.lastAttempt) < v.backoff
	v.mu.Unlock()
	if throttled {
		return nil, fmt.Errorf("unknown kid %q (jwks refresh throttled)", kid)
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// refresh fetches the JWKS once for all concurrent callers.
func (v *Verifier) refresh(ctx context.Context) error {
	ch := v.flight.DoChan("jwks", func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.opts.HTTPClient.Timeout+time.Second)
		defer cancel()
		keys, err := v.fetch(fctx)
		now := v.opts.Clock.Now()
		v.mu.Lock()
		defer v.mu.Unlock()
		v.lastAttempt = now
		if err != nil {
			v.backoff = min(max(2*v.backoff, time.Second), 30*time.Second)
			return nil, err
		}
		v.keys, v.fetchedAt, v.backoff = keys, now, 10*time.Second // unknown kids: at most one fetch per 10 s
		return nil, nil
	})
	select {
	case r := <-ch:
		return r.Err
	case <-ctx.Done():
		return fmt.Errorf("jwks refresh: %w", ctx.Err())
	}
}

func (v *Verifier) refreshAsync() {
	v.mu.Lock()
	if v.refreshing || v.opts.Clock.Now().Sub(v.lastAttempt) < v.backoff {
		v.mu.Unlock()
		return
	}
	v.refreshing = true
	v.mu.Unlock()
	go func() {
		_ = v.refresh(context.Background())
		v.mu.Lock()
		v.refreshing = false
		v.mu.Unlock()
	}()
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.opts.JWKSURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	resp, err := v.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Alg != "" && k.Alg != "RS256") || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

// Middleware authenticates requests with a bearer token and stores the principal in the
// context. Failures get 401 with `WWW-Authenticate: Bearer error="invalid_token"`; the token is
// never echoed or logged.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := BearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-gateway"`)
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		p, err := v.Verify(r.Context(), token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// BearerToken extracts the token from an Authorization: Bearer header.
func BearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
