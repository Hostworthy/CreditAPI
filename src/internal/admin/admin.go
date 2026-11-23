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
	"github.com/atb00ker/CreditAPI/src/internal/forwarded"
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
	cfg        *config.Config
	keys       *keys.Store
	subs       *subscriptions.Manager
	slots      *agent.Slots
	keyMu      sync.RWMutex
	hmacKey    []byte
	secretPath string
	limiter    *limiter
	checkMu    sync.Mutex // one login check at a time
}

// New creates the admin handlers. The session signing key is derived from a
// random per-installation secret plus ADMIN_PASSWORD, so changing the
// password signs everyone out.
func New(cfg *config.Config, ks *keys.Store, subs *subscriptions.Manager, slots *agent.Slots) (*Admin, error) {
	a := &Admin{
		cfg: cfg, keys: ks, subs: subs, slots: slots,
		secretPath: filepath.Join(cfg.DataDir, "session-secret"),
		limiter:    &limiter{fails: map[string][]time.Time{}},
	}
	secret, err := os.ReadFile(a.secretPath)
	if err != nil || len(secret) < 32 {
		if err := a.rotateSecret(); err != nil {
			return nil, err
		}
		return a, nil
	}
	a.setKey(secret)
	return a, nil
}

// rotateSecret replaces the session secret, which signs every session out.
func (a *Admin) rotateSecret() error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if err := keys.WriteFileAtomic(a.secretPath, secret, 0o600); err != nil {
		return err
	}
	a.setKey(secret)
	return nil
}

func (a *Admin) setKey(secret []byte) {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(a.cfg.AdminPassword))
	a.keyMu.Lock()
	a.hmacKey = mac.Sum(nil)
	a.keyMu.Unlock()
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

// sign returns a session value "expiry.nonce.mac". The nonce makes every
// session unique.
func (a *Admin) sign(exp int64, nonce string) string {
	payload := strconv.FormatInt(exp, 10) + "." + nonce
	a.keyMu.RLock()
	mac := hmac.New(sha256.New, a.hmacKey)
	a.keyMu.RUnlock()
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *Admin) validCookie(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(a.sign(exp, parts[1])), []byte(c.Value))
}

// passwordOK compares digests so the comparison takes the same time
// whatever the length of the guess.
func (a *Admin) passwordOK(p string) bool {
	got := sha256.Sum256([]byte(p))
	want := sha256.Sum256([]byte(a.cfg.AdminPassword))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// Authorized reports whether the request carries a valid admin session or
// the admin password as a bearer token. It does not count failures.
func (a *Admin) Authorized(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return a.passwordOK(strings.TrimSpace(bearer))
	}
	return a.validCookie(r)
}

// secure reports whether cookies should be marked Secure: the request came
// over HTTPS (directly or through a trusted proxy), or PUBLIC_URL is HTTPS.
func (a *Admin) secure(r *http.Request) bool {
	return a.cfg.TrustedProxies.HTTPS(r) || strings.HasPrefix(a.cfg.PublicURL, "https://")
}

func (a *Admin) clientKey(r *http.Request) string {
	return forwarded.LimitKey(a.cfg.TrustedProxies.ClientIP(r))
}

// sameOrigin rejects cross-site requests that carry the session cookie.
func (a *Admin) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Host == r.Host || u.Host == a.cfg.TrustedProxies.Host(r) {
		return true
	}
	if pu, err := url.Parse(a.cfg.PublicURL); err == nil && pu.Host != "" && u.Host == pu.Host {
		return true
	}
	return false
}

func (a *Admin) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			ip := a.clientKey(r)
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
		if r.Method != http.MethodGet && !a.sameOrigin(r) {
			jsonError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next(w, r)
	}
}

func (a *Admin) createSession(w http.ResponseWriter, r *http.Request) {
	ip := a.clientKey(r)
	if !a.sameOrigin(r) {
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
	if !readJSON(w, r, &body) {
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
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.sign(exp, hex.EncodeToString(nonce)), Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteSession signs out. Sessions are not stored, so the only way to make
// a copied cookie useless is a new secret: this signs every device out.
func (a *Admin) deleteSession(w http.ResponseWriter, r *http.Request) {
	if a.validCookie(r) && a.sameOrigin(r) {
		if err := a.rotateSecret(); err != nil {
			logging.From(r.Context()).Error(logging.Event("admin", "signout", "failed"), logging.Err(err))
		}
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure(r), SameSite: http.SameSiteStrictMode})
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
	if !readJSON(w, r, &body) {
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
		_ = a.keys.Delete(k.ID) // do not leave a key without its grants
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
	if !readJSON(w, r, &body) {
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

// readJSON decodes a small JSON body (at most 64 KiB, read within 15
// seconds). On failure it writes the error response and returns false.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, http.StatusRequestEntityTooLarge, "request body is too large (max 64 KB)")
		} else {
			jsonError(w, http.StatusBadRequest, "could not read the request body")
		}
		return false
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return true
	}
	if err := json.Unmarshal(b, v); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
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

// maxTracked bounds the limiter's memory when many addresses fail.
const maxTracked = 10_000

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, known := l.fails[ip]; !known && len(l.fails) >= maxTracked {
		for k := range l.fails {
			l.prune(k)
		}
		for k := range l.fails {
			if len(l.fails) < maxTracked {
				break
			}
			delete(l.fails, k) // arbitrary eviction keeps memory bounded
		}
	}
	l.fails[ip] = append(l.prune(ip), time.Now())
}
