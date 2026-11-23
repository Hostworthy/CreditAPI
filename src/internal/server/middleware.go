package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/atb00ker/claude-max-to-api/src/internal/logging"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func resolveRequestID(header string) string {
	if requestIDPattern.MatchString(header) {
		return header
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusRecorder captures the response status while keeping streaming
// (Flush) and http.ResponseController working.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withRequestLogging assigns a request ID (echoed in X-Request-Id) and writes
// one access log line per request. Health checks and static assets log at
// debug level to keep logs quiet.
func withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := resolveRequestID(r.Header.Get("X-Request-Id"))
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(logging.WithRequestID(r.Context(), id)))

		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/assets/") {
			level = slog.LevelDebug
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Default().LogAttrs(r.Context(), level, logging.Event("http", "request", "completed"),
			logging.RequestID(id), slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", status), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	})
}
