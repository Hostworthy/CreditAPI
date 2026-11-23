// Package admin serves the /admin dashboard, the /login Claude sign-in flow
// and their JSON APIs.
package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/claude-max-to-api/src/internal/claude"
	"github.com/atb00ker/claude-max-to-api/src/internal/config"
	"github.com/atb00ker/claude-max-to-api/src/internal/keys"
	"github.com/atb00ker/claude-max-to-api/src/internal/logging"
)

//go:embed web
var webFS embed.FS

const (
	cookieName = "cmta_admin"
	sessionTTL = 7 * 24 * time.Hour
)

// Admin wires the admin UI and API.
type Admin struct {
	cfg     *config.Config
	keys    *keys.Store
	tokens  *claude.TokenStore
	login   *claude.LoginManager
	runner  *claude.Runner
	status  *claude.Status
	hmacKey []byte
	limiter *limiter
	checkMu sync.Mutex // one login check at a time
}

// checkReuse is how long a verified login is trusted without a new check.
const checkReuse = 10 * time.Minute

// New creates the admin handlers. The session signing key is derived from a
// random per-installation secret plus ADMIN_PASSWORD, so changing the
// password signs everyone out.
func New(cfg *config.Config, ks *keys.Store, tokens *claude.TokenStore, login *claude.LoginManager, runner *claude.Runner, status *claude.Status) (*Admin, error) {
	secretPath := filepath.Join(cfg.DataDir, "session-secret")
	secret, err := os.ReadFile(secretPath)
	if err != nil || len(secret) < 32 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := keys.WriteFileAtomic(secretPath, secret, 0o600); err != nil {
			return nil, err
		}
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(cfg.AdminPassword))
	return &Admin{
		cfg: cfg, keys: ks, tokens: tokens, login: login, runner: runner, status: status,
		hmacKey: mac.Sum(nil),
		limiter: &limiter{fails: map[string][]time.Time{}},
	}, nil
}

// Register adds all admin routes.
func (a *Admin) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(webFS, "web")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusFound)
	})
	mux.HandleFunc("GET /admin", a.page(static, "admin.html"))
	mux.HandleFunc("GET /login", a.page(static, "login.html"))
	mux.HandleFunc("GET /assets/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if strings.Contains(name, "/") || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFileFS(w, r, static, name)
	})

	mux.HandleFunc("POST /admin/api/session", a.createSession)
	mux.HandleFunc("DELETE /admin/api/session", a.deleteSession)
	mux.HandleFunc("GET /admin/api/me", a.me)

	mux.HandleFunc("GET /admin/api/keys", a.require(a.listKeys))
	mux.HandleFunc("POST /admin/api/keys", a.require(a.createKey))
	mux.HandleFunc("PATCH /admin/api/keys/{id}", a.require(a.updateKey))
	mux.HandleFunc("DELETE /admin/api/keys/{id}", a.require(a.deleteKey))

	mux.HandleFunc("GET /admin/api/claude", a.require(a.claudeStatus))
	mux.HandleFunc("POST /admin/api/claude/login", a.require(a.startLogin))
	mux.HandleFunc("POST /admin/api/claude/login/code", a.require(a.submitCode))
	mux.HandleFunc("DELETE /admin/api/claude/login", a.require(a.cancelLogin))
	mux.HandleFunc("PUT /admin/api/claude/token", a.require(a.setToken))
	mux.HandleFunc("DELETE /admin/api/claude/token", a.require(a.clearToken))
	mux.HandleFunc("POST /admin/api/claude/check", a.require(a.checkClaude))
}

func (a *Admin) page(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(static, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		_, _ = w.Write(b)
	}
}

// --- sessions ----------------------------------------------------------------

func (a *Admin) sign(exp int64) string {
	mac := hmac.New(sha256.New, a.hmacKey)
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *Admin) validCookie(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(a.sign(exp)), []byte(c.Value))
}

func (a *Admin) passwordOK(p string) bool {
	return subtle.ConstantTimeCompare([]byte(p), []byte(a.cfg.AdminPassword)) == 1
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOrigin rejects cross-site requests that carry the session cookie.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host || (r.Header.Get("X-Forwarded-Host") != "" && u.Host == r.Header.Get("X-Forwarded-Host"))
}

func (a *Admin) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			ip := clientIP(r)
			if a.limiter.blocked(ip) {
				jsonError(w, http.StatusTooManyRequests, "too many failed attempts, wait a few minutes")
				return
			}
			if !a.passwordOK(strings.TrimSpace(bearer)) {
				a.limiter.fail(ip)
				jsonError(w, http.StatusUnauthorized, "invalid admin password")
				return
			}
			next(w, r)
			return
		}
		if !a.validCookie(r) {
			jsonError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			jsonError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next(w, r)
	}
}

func (a *Admin) createSession(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if a.limiter.blocked(ip) {
		jsonError(w, http.StatusTooManyRequests, "too many failed attempts, wait a few minutes")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !a.passwordOK(body.Password) {
		a.limiter.fail(ip)
		time.Sleep(400 * time.Millisecond)
		logging.From(r.Context()).Warn(logging.Event("admin", "signin", "failed"), slog.String("ip", ip))
		jsonError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.sign(exp), Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) deleteSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": a.validCookie(r)})
}

// --- keys --------------------------------------------------------------------

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": a.keys.List()})
}

func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	k, secret, err := a.keys.Create(body.Name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	k.Hash = ""
	logging.From(r.Context()).Info(logging.Event("key", "create", "succeeded"), slog.String("key", k.Name), slog.String("key_id", k.ID))
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

func (a *Admin) updateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     *string `json:"name"`
		Disabled *bool   `json:"disabled"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	k, err := a.keys.Update(r.PathValue("id"), body.Name, body.Disabled)
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

func (a *Admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	err := a.keys.Delete(r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("key", "delete", "succeeded"), slog.String("key_id", r.PathValue("id")))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- claude auth ---------------------------------------------------------------

func (a *Admin) claudeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"token":   a.tokenInfo(),
		"auth":    a.status.Auth(a.tokens.Version()),
		"login":   a.login.Status(),
		"runtime": a.status.Snapshot(),
		"config": map[string]any{
			"default_model":  a.cfg.DefaultModel,
			"models":         a.cfg.Models,
			"max_concurrent": a.cfg.MaxConcurrent,
			"default_effort": a.cfg.DefaultEffort,
			"env_token_set":  a.cfg.EnvToken != "",
		},
	})
}

func (a *Admin) tokenInfo() map[string]any {
	tok, source := a.tokens.Token()
	token := map[string]any{"source": source, "hint": claude.Hint(tok)}
	if t := a.tokens.SavedAt(); source == "login" && !t.IsZero() {
		token["saved_at"] = t.UTC()
		token["expires_about"] = t.Add(claude.TokenLifetime).UTC()
	}
	return token
}

func (a *Admin) startLogin(w http.ResponseWriter, r *http.Request) {
	st, err := a.login.Start()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *Admin) submitCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.login.SubmitCode(body.Code); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.login.Status())
}

func (a *Admin) cancelLogin(w http.ResponseWriter, r *http.Request) {
	a.login.Cancel()
	writeJSON(w, http.StatusOK, a.login.Status())
}

func (a *Admin) setToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.tokens.Save(body.Token); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("login", "token", "saved"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) clearToken(w http.ResponseWriter, r *http.Request) {
	if err := a.tokens.Clear(); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("login", "token", "removed"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// checkClaude answers "is the Claude login working?". A recent successful
// request or check is reused; otherwise a one-word prompt is sent to Claude.
// state is "valid", "login_required" or "error" (could not tell, e.g. network).
func (a *Admin) checkClaude(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	_ = readJSON(r, &body)

	a.checkMu.Lock()
	defer a.checkMu.Unlock()

	out := map[string]any{"token": a.tokenInfo()}
	if info := a.status.Auth(a.tokens.Version()); !body.Force && info.State == "valid" &&
		info.CheckedAt != nil && time.Since(*info.CheckedAt) < checkReuse {
		out["state"], out["checked_at"], out["source"], out["cached"] = "valid", info.CheckedAt, info.Source, true
		writeJSON(w, http.StatusOK, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	release, err := a.runner.Acquire(ctx, 30*time.Second)
	if err != nil {
		out["state"], out["reason"] = "error", err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	defer release()
	start := time.Now()
	res, err := a.runner.Run(ctx, claude.RunOptions{
		Model:        "haiku",
		SystemPrompt: "You are a connectivity check. Follow the instruction exactly.",
		Content:      []claude.ContentBlock{{Type: "text", Text: "Reply with exactly the word OK and nothing else."}},
		Probe:        true,
	}, claude.Callbacks{})
	now := time.Now().UTC()
	out["checked_at"], out["source"], out["cached"] = now, "check", false
	out["duration_ms"] = time.Since(start).Milliseconds()
	var ce *claude.Error
	switch {
	case err == nil:
		out["state"], out["reply"], out["model"] = "valid", strings.TrimSpace(res.Text), res.Model
	case errors.As(err, &ce) && ce.Kind == "auth":
		out["state"], out["reason"] = "login_required", ce.Message
	default:
		out["state"], out["reason"] = "error", err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers -----------------------------------------------------------------

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// limiter blocks an address after repeated failed password attempts.
type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	maxFails   = 10
	failWindow = 10 * time.Minute
)

func (l *limiter) prune(ip string) []time.Time {
	cut := time.Now().Add(-failWindow)
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip)) >= maxFails
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.prune(ip), time.Now())
}
