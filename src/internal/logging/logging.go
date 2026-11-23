// Package logging provides JSON structured logging to stdout with redaction
// of secrets and user content.
//
// Messages are stable event IDs of the form
// creditapi.<operation>.<action>.<status> (see docs/DEV.md).
//
// Safe fields: request_id, method, path, status, duration_ms, key, key_id,
// model, stream, images, tools, finish, tool_calls, prompt_tokens,
// completion_tokens, error (sanitized). Never log tokens, API keys, the admin
// password, authentication codes, prompts or responses.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Service is the value of the "service" field and the first part of every
// event ID.
const Service = "creditapi"

// New builds a JSON logger writing to os.Stdout with redaction.
func New(level string) (*slog.Logger, error) {
	return NewWithWriter(os.Stdout, level)
}

// NewWithWriter is New with a custom destination.
func NewWithWriter(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			switch attr.Key {
			case slog.TimeKey:
				return slog.Attr{Key: "timestamp", Value: slog.TimeValue(attr.Value.Time().UTC())}
			case slog.MessageKey:
				return slog.Attr{Key: "message", Value: attr.Value}
			case slog.LevelKey:
				if l, ok := attr.Value.Any().(slog.Level); ok {
					return slog.String(slog.LevelKey, strings.ToLower(l.String()))
				}
			}
			return attr
		},
	}
	base := slog.NewJSONHandler(w, opts)
	return slog.New(NewRedactingHandler(base)).With(slog.String("service", Service)), nil
}

// ParseLevel maps LOG_LEVEL strings to slog levels.
func ParseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}
}

// Event builds an event ID: Event("chat", "completion", "succeeded") returns
// "creditapi.chat.completion.succeeded".
func Event(parts ...string) string {
	return Service + "." + strings.Join(parts, ".")
}

type requestIDKey struct{}

// WithRequestID stores a request ID in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request ID stored in ctx, if any.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// From returns the default logger, annotated with the request ID in ctx.
func From(ctx context.Context) *slog.Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return slog.Default().With(RequestID(id))
	}
	return slog.Default()
}

// RequestID logs a per-request correlation id.
func RequestID(id string) slog.Attr { return slog.String("request_id", id) }

// Err logs an error value; the redacting handler sanitizes its message.
func Err(err error) slog.Attr { return slog.Any("error", err) }
