package logging

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
)

const redacted = "[REDACTED]"

// secretPattern matches Claude OAuth tokens, Anthropic API keys and this
// server's virtual keys (current and legacy prefix) wherever they appear
// inside a string.
var secretPattern = regexp.MustCompile(`sk-(?:ant|credit|cmta)-[A-Za-z0-9_\-]{8,}`)

// sensitiveKeys are attribute keys whose values are never written.
var sensitiveKeys = map[string]struct{}{
	"password":       {},
	"admin_password": {},
	"token":          {},
	"oauth_token":    {},
	"access_token":   {},
	"refresh_token":  {},
	"api_key":        {},
	"credential":     {},
	"secret":         {},
	"authorization":  {},
	"cookie":         {},
	"code":           {},
	"prompt":         {},
	"messages":       {},
	"content":        {},
	"body":           {},
	"system_prompt":  {},
}

// RedactingHandler wraps a slog.Handler, dropping sensitive attributes and
// masking secrets embedded in other strings.
type RedactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler returns a handler that sanitizes records before writing.
func NewRedactingHandler(inner slog.Handler) *RedactingHandler {
	return &RedactingHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *RedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(sanitize(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

// WithAttrs implements slog.Handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = sanitize(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(clean)}
}

// WithGroup implements slog.Handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name)}
}

func sanitize(a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]any, len(group))
		for i, g := range group {
			clean[i] = sanitize(g)
		}
		return slog.Group(a.Key, clean...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && err != nil {
			return slog.String(a.Key, RedactString(err.Error()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

var (
	secretsMu sync.RWMutex
	secrets   = map[string]struct{}{}
)

// RegisterSecret adds a value (a password, API key or token) that must never
// appear in logs or error messages. Values shorter than 8 characters are
// ignored, so common words are not masked everywhere.
func RegisterSecret(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 8 {
		return
	}
	secretsMu.Lock()
	secrets[v] = struct{}{}
	secretsMu.Unlock()
}

// RedactString masks secrets embedded in s: anything shaped like an
// Anthropic or CreditAPI key, and every registered secret.
func RedactString(s string) string {
	s = secretPattern.ReplaceAllString(s, redacted)
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for v := range secrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}
