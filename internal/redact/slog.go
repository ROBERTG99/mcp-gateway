package redact

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys are attribute keys whose values are always replaced, whatever they contain.
var sensitiveKeys = map[string]bool{
	"authorization": true, "token": true, "access_token": true, "password": true, "secret": true,
	"api_key": true, "cookie": true, "set-cookie": true, "signature": true,
}

// Handler is a slog.Handler that redacts every string attribute (and the message) before
// passing the record on, so personal data and tokens never reach the logs.
type Handler struct {
	next slog.Handler
	r    *Redactor
}

// NewHandler wraps next.
func NewHandler(next slog.Handler, r *Redactor) *Handler { return &Handler{next: next, r: r} }

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out) //nolint:wrapcheck // pass-through handler
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.attr(a)
	}
	return &Handler{next: h.next.WithAttrs(red), r: h.r}
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), r: h.r}
}

func (h *Handler) attr(a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, Marker(Secret))
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.String(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		red := make([]slog.Attr, len(attrs))
		for i, g := range attrs {
			red[i] = h.attr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, h.r.String(err.Error()))
		}
		return slog.Any(a.Key, h.r.Value(normalize(v.Any())))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// normalize turns fmt.Stringer and other values into strings the redactor can inspect.
func normalize(v any) any {
	switch x := v.(type) {
	case string, map[string]any, []any:
		return x
	case interface{ String() string }:
		return x.String()
	default:
		return v
	}
}
