// Package admin serves the /admin dashboard, the /login Claude sign-in flow
// and their JSON APIs.
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
)

//go:embed web
var webFS embed.FS

const (
	cookieName = "creditapi_admin"
	sessionTTL = 7 * 24 * time.Hour
)

// Admin wires the admin UI and API.
type Admin struct {
	cfg     *config.Config
	keys    *keys.Store
	subs    *subscriptions.Manager
	slots   *agent.Slots
	hmacKey []byte
	limiter *limiter
	checkMu sync.Mutex // one login check at a time
}

// New creates the admin handlers. The session signing key is derived from a
// random per-installation secret plus ADMIN_PASSWORD, so changing the
// password signs everyone out.
func New(cfg *config.Config, ks *keys.Store, subs *subscriptions.Manager, slots *agent.Slots) (*Admin, error) {
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
		cfg: cfg, keys: ks, subs: subs, slots: slots,
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

	a.registerSubscriptions(mux)
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

// keyView adds the subscriptions a key may use.
func (a *Admin) keyView(k keys.Key) map[string]any {
	return map[string]any{
		"id": k.ID, "name": k.Name, "hint": k.Hint, "created_at": k.CreatedAt, "last_used_at": k.LastUsedAt,
		"disabled": k.Disabled, "requests": k.Requests, "input_tokens": k.InputTokens, "output_tokens": k.OutputTokens,
		"subscriptions": nonNil(a.subs.Store.GrantsOf(k.ID)),
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, k := range a.keys.List() {
		list = append(list, a.keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": list})
}

func (a *Admin) checkSubscriptionIDs(ids []string) error {
	for _, id := range ids {
		if _, ok := a.subs.Store.Get(id); !ok {
			return fmt.Errorf("unknown subscription %q", id)
		}
	}
	return nil
}

func (a *Admin) checkKeyIDs(ids []string) error {
	known := map[string]bool{}
	for _, k := range a.keys.List() {
		known[k.ID] = true
	}
	for _, id := range ids {
		if !known[strings.TrimSpace(id)] {
			return fmt.Errorf("unknown key %q", id)
		}
	}
	return nil
}

// createKey creates a key. Without "subscriptions" in the body the key may use
// every existing subscription; pass a list (possibly empty) to choose.
func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string    `json:"name"`
		Subscriptions *[]string `json:"subscriptions"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	var grants []string
	if body.Subscriptions == nil {
		for _, s := range a.subs.Store.List() {
			grants = append(grants, s.ID)
		}
	} else {
		grants = *body.Subscriptions
		if err := a.checkSubscriptionIDs(grants); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	k, secret, err := a.keys.Create(body.Name)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.subs.Store.SetKeyGrants(k.ID, grants); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("key", "create", "succeeded"), slog.String("key", k.Name), slog.String("key_id", k.ID))
	writeJSON(w, http.StatusCreated, map[string]any{"key": a.keyView(*k), "secret": secret})
}

func (a *Admin) updateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          *string   `json:"name"`
		Disabled      *bool     `json:"disabled"`
		Subscriptions *[]string `json:"subscriptions"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if body.Subscriptions != nil {
		if err := a.checkSubscriptionIDs(*body.Subscriptions); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	k, err := a.keys.Update(id, body.Name, body.Disabled)
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Subscriptions != nil {
		if err := a.subs.Store.SetKeyGrants(id, *body.Subscriptions); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": a.keyView(*k)})
}

func (a *Admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.keys.Delete(id)
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.subs.Store.SetKeyGrants(id, nil); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("key", "delete", "succeeded"), slog.String("key_id", id))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
