package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/agent/claude"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
)

// checkReuse is how long a verified login is trusted without a new check.
const checkReuse = 10 * time.Minute

func (a *Admin) registerSubscriptions(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/subscriptions", a.require(a.listSubscriptions))
	mux.HandleFunc("POST /admin/api/subscriptions", a.require(a.createSubscription))
	mux.HandleFunc("GET /admin/api/subscriptions/{id}", a.require(a.getSubscription))
	mux.HandleFunc("PATCH /admin/api/subscriptions/{id}", a.require(a.updateSubscription))
	mux.HandleFunc("DELETE /admin/api/subscriptions/{id}", a.require(a.deleteSubscription))
	mux.HandleFunc("POST /admin/api/subscriptions/{id}/move", a.require(a.moveSubscription))
	mux.HandleFunc("POST /admin/api/subscriptions/{id}/check", a.require(a.checkSubscription))
	mux.HandleFunc("POST /admin/api/subscriptions/{id}/login", a.require(a.startLogin))
	mux.HandleFunc("POST /admin/api/subscriptions/{id}/login/code", a.require(a.submitCode))
	mux.HandleFunc("DELETE /admin/api/subscriptions/{id}/login", a.require(a.cancelLogin))
	mux.HandleFunc("PUT /admin/api/subscriptions/{id}/credential", a.require(a.setCredential))
	mux.HandleFunc("DELETE /admin/api/subscriptions/{id}/credential", a.require(a.signOut))
}

func (a *Admin) view(sub subscriptions.Subscription, priority int) map[string]any {
	out := map[string]any{
		"id": sub.ID, "name": sub.Name, "slug": sub.Slug(), "provider": sub.Provider,
		"keys": sub.Keys, "disabled": sub.Disabled, "priority": priority,
	}
	acc, b, err := a.subs.Account(sub)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	tok, source := acc.Tokens.Token()
	cred := map[string]any{"source": source, "hint": agent.Hint(tok), "name": b.CredentialEnv()}
	if t := acc.Tokens.SavedAt(); !t.IsZero() {
		cred["saved_at"] = t.UTC()
		if sub.Provider == claude.Provider && source == "login" {
			cred["expires_about"] = t.Add(claude.TokenLifetime).UTC()
		}
	}
	out["provider_label"] = b.Label()
	out["credential"] = cred
	out["auth"] = acc.Status.Auth(acc.Tokens.Version())
	out["runtime"] = acc.Status.Snapshot()
	out["login"] = a.subs.Login.Status(sub.ID)
	out["cli_version"] = a.subs.Version(sub.Provider)
	return out
}

func (a *Admin) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	providers := []map[string]any{}
	for _, b := range a.subs.Providers() {
		providers = append(providers, map[string]any{
			"id": b.Provider(), "label": b.Label(), "models": b.Models(), "default_model": b.DefaultModel(),
			"cli_version": a.subs.Version(b.Provider()), "credential_env": b.CredentialEnv(),
		})
	}
	list := []map[string]any{}
	for i, s := range a.subs.Store.List() {
		list = append(list, a.view(s, i))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": providers, "subscriptions": list, "in_flight": a.slots.InFlight(),
		"config": map[string]any{"max_concurrent": a.cfg.MaxConcurrent, "default_effort": a.cfg.DefaultEffort},
	})
}

func (a *Admin) lookup(w http.ResponseWriter, r *http.Request) (subscriptions.Subscription, *agent.Account, agent.Backend, bool) {
	sub, acc, b, err := a.subs.AccountByID(r.PathValue("id"))
	if errors.Is(err, subscriptions.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "subscription not found")
		return sub, nil, nil, false
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return sub, nil, nil, false
	}
	return sub, acc, b, true
}

func (a *Admin) priority(id string) int {
	for i, s := range a.subs.Store.List() {
		if s.ID == id {
			return i
		}
	}
	return -1
}

func (a *Admin) createSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string   `json:"name"`
		Provider string   `json:"provider"`
		Keys     []string `json:"keys"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.checkKeyIDs(body.Keys); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := a.subs.Create(body.Name, strings.ToLower(strings.TrimSpace(body.Provider)), body.Keys)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.view(sub, a.priority(sub.ID)))
}

func (a *Admin) getSubscription(w http.ResponseWriter, r *http.Request) {
	sub, _, _, ok := a.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.view(sub, a.priority(sub.ID)))
}

func (a *Admin) updateSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     *string   `json:"name"`
		Disabled *bool     `json:"disabled"`
		Keys     *[]string `json:"keys"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Keys != nil {
		if err := a.checkKeyIDs(*body.Keys); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	sub, err := a.subs.Store.Update(r.PathValue("id"), body.Name, body.Disabled, body.Keys)
	if errors.Is(err, subscriptions.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "subscription not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.view(sub, a.priority(sub.ID)))
}

func (a *Admin) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	err := a.subs.Delete(r.PathValue("id"))
	if errors.Is(err, subscriptions.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "subscription not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) moveSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Direction string `json:"direction"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	delta := 0
	switch body.Direction {
	case "up":
		delta = -1
	case "down":
		delta = 1
	default:
		jsonError(w, http.StatusBadRequest, `direction must be "up" or "down"`)
		return
	}
	if err := a.subs.Store.Move(r.PathValue("id"), delta); err != nil {
		jsonError(w, http.StatusNotFound, "subscription not found")
		return
	}
	a.listSubscriptions(w, r)
}

// checkSubscription answers "is this subscription's login working?". A recent
// successful request or check is reused; otherwise a one-word prompt is sent.
// state is "valid", "login_required" or "error" (could not tell).
func (a *Admin) checkSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	_ = readJSON(r, &body)
	sub, acc, b, ok := a.lookup(w, r)
	if !ok {
		return
	}

	a.checkMu.Lock()
	defer a.checkMu.Unlock()

	out := map[string]any{"subscription": a.view(sub, a.priority(sub.ID))}
	if info := acc.Status.Auth(acc.Tokens.Version()); !body.Force && info.State == "valid" &&
		info.CheckedAt != nil && time.Since(*info.CheckedAt) < checkReuse {
		out["state"], out["checked_at"], out["source"], out["cached"] = "valid", info.CheckedAt, info.Source, true
		writeJSON(w, http.StatusOK, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	release, err := a.slots.Acquire(ctx, 30*time.Second)
	if err != nil {
		out["state"], out["reason"] = "error", err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	defer release()
	start := time.Now()
	res, err := agent.Execute(ctx, b, acc, agent.RunOptions{
		Model:        a.subs.ProbeModel(b),
		SystemPrompt: "You are a connectivity check. Follow the instruction exactly.",
		Content:      []agent.ContentBlock{{Type: "text", Text: "Reply with exactly the word OK and nothing else."}},
		Probe:        true,
	}, agent.Callbacks{})
	out["checked_at"], out["source"], out["cached"] = time.Now().UTC(), "check", false
	out["duration_ms"] = time.Since(start).Milliseconds()
	var ce *agent.Error
	switch {
	case err == nil:
		out["state"], out["reply"], out["model"] = "valid", strings.TrimSpace(res.Text), res.Model
	case errors.As(err, &ce) && ce.Kind == "auth":
		out["state"], out["reason"] = "login_required", ce.Message
	default:
		out["state"], out["reason"] = "error", err.Error()
	}
	out["subscription"] = a.view(sub, a.priority(sub.ID))
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) startLogin(w http.ResponseWriter, r *http.Request) {
	sub, acc, b, ok := a.lookup(w, r)
	if !ok {
		return
	}
	st, err := a.subs.Login.Start(sub.ID, b.Login(acc))
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
	id := r.PathValue("id")
	if err := a.subs.Login.SubmitCode(id, body.Code); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.subs.Login.Status(id))
}

func (a *Admin) cancelLogin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.subs.Login.Cancel(id)
	writeJSON(w, http.StatusOK, a.subs.Login.Status(id))
}

func (a *Admin) setCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Credential string `json:"credential"`
		Token      string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, acc, _, ok := a.lookup(w, r)
	if !ok {
		return
	}
	cred := body.Credential
	if cred == "" {
		cred = body.Token
	}
	if err := acc.Tokens.Save(cred); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	logging.From(r.Context()).Info(logging.Event("login", "credential", "saved"), slog.String("subscription", sub.ID))
	writeJSON(w, http.StatusOK, a.view(sub, a.priority(sub.ID)))
}

func (a *Admin) signOut(w http.ResponseWriter, r *http.Request) {
	sub, _, _, ok := a.lookup(w, r)
	if !ok {
		return
	}
	if err := a.subs.SignOut(r.Context(), sub.ID); err != nil {
		logging.From(r.Context()).Warn(logging.Event("login", "credential", "remove-failed"), slog.String("subscription", sub.ID), logging.Err(err))
	}
	logging.From(r.Context()).Info(logging.Event("login", "credential", "removed"), slog.String("subscription", sub.ID))
	writeJSON(w, http.StatusOK, a.view(sub, a.priority(sub.ID)))
}
