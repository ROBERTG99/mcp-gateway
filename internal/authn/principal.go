// Package authn verifies agent and admin JWTs and carries the authenticated principal.
package authn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

// Principal is an authenticated caller: an agent (or admin user) acting for a tenant.
type Principal struct {
	Tenant    string
	Agent     string
	Scopes    []string
	Issuer    string
	ExpiresAt time.Time
}

// HasScope reports whether the principal holds scope.
func (p Principal) HasScope(scope string) bool { return slices.Contains(p.Scopes, scope) }

// Key identifies the principal's authorization context (tenant, agent and scope set).
// Two tokens with the same key see exactly the same tools.
func (p Principal) Key() string {
	scopes := slices.Clone(p.Scopes)
	slices.Sort(scopes)
	sum := sha256.Sum256([]byte(p.Tenant + "\x00" + p.Agent + "\x00" + strings.Join(scopes, " ")))
	return hex.EncodeToString(sum[:16])
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
