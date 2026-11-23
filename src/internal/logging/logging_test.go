package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestSchema(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewWithWriter(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	log.Info(Event("server", "start", "succeeded"), slog.String("path", "/v1"))
	log.Debug(Event("hidden", "debug", "line"))
	lines := decode(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (debug filtered), got %d", len(lines))
	}
	l := lines[0]
	for _, key := range []string{"timestamp", "level", "service", "message", "path"} {
		if _, ok := l[key]; !ok {
			t.Errorf("missing %q in %v", key, l)
		}
	}
	if l["level"] != "info" || l["service"] != Service || l["message"] != "creditapi.server.start.succeeded" {
		t.Fatalf("line %v", l)
	}
	if _, ok := l["msg"]; ok {
		t.Fatal("msg should be renamed to message")
	}
}

func TestEventIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+){2,4}$`)
	for _, id := range []string{
		Event("chat", "completion", "succeeded"),
		Event("http", "request", "completed"),
		Event("login", "setup-token", "started"),
	} {
		if !re.MatchString(id) {
			t.Errorf("bad event id %q", id)
		}
	}
}

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewWithWriter(&buf, "debug")
	log.Info(Event("test", "redact", "checked"),
		slog.String("token", "sk-ant-oat01-abcdefghijklmnop"),
		slog.String("Authorization", "Bearer x"),
		slog.String("prompt", "my private notes"),
		slog.String("note", "key sk-credit-ABCDEFGHIJKL leaked and sk-ant-api03-zzzzzzzzzzzz too, legacy sk-cmta-ABCDEFGHIJKL"),
		slog.Any("error", errors.New("bad token sk-ant-oat01-qqqqqqqqqqqqqqqq")),
		slog.Group("req", slog.String("code", "abc#def"), slog.Int("n", 3)),
	)
	out := buf.String()
	for _, secret := range []string{"sk-ant-oat01-abcdefghijklmnop", "Bearer x", "my private notes", "sk-credit-ABCDEFGHIJKL", "sk-cmta-ABCDEFGHIJKL", "sk-ant-api03-zzzzzzzzzzzz", "sk-ant-oat01-qqqqqqqqqqqqqqqq", "abc#def"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"n":3`) || !strings.Contains(out, "leaked and") {
		t.Errorf("non-secret data lost: %s", out)
	}
}

func TestWithAttrsRedacts(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewWithWriter(&buf, "info")
	log.With(slog.String("api_key", "sk-credit-123456789")).Info(Event("a", "b", "c"))
	if strings.Contains(buf.String(), "sk-credit-123456789") {
		t.Fatal("WithAttrs leaked secret")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("expected error")
	}
}

func TestRequestIDContext(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-1")
	if RequestIDFrom(ctx) != "req-1" || RequestIDFrom(context.Background()) != "" {
		t.Fatal("request id round trip")
	}
	var buf bytes.Buffer
	log, _ := NewWithWriter(&buf, "info")
	prev := slog.Default()
	slog.SetDefault(log)
	defer slog.SetDefault(prev)
	From(ctx).Info(Event("a", "b", "c"))
	if !strings.Contains(buf.String(), `"request_id":"req-1"`) {
		t.Fatalf("request id missing: %s", buf.String())
	}
}

func TestRegisteredSecretsAreRedacted(t *testing.T) {
	RegisterSecret("short")
	RegisterSecret("crsr_live_0123456789abcdef")
	got := RedactString("auth failed for key crsr_live_0123456789abcdef (short)")
	if strings.Contains(got, "crsr_live") || !strings.Contains(got, "short") {
		t.Fatalf("%q", got)
	}
}
