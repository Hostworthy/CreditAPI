package integration_test

import (
	"strings"
	"testing"

	"github.com/atb00ker/CreditAPI/src/tests/support"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
)

const whoami = "[fake:whoami]"

// served asks who answered and returns the subscription ID from the reply.
func served(t *testing.T, s *support.TestServer, key, model string) (int, string) {
	t.Helper()
	code, reply, out := s.Ask(key, model, whoami)
	if code != 200 {
		return code, errorOf(t, out)["code"].(string)
	}
	return code, reply[strings.LastIndex(reply, " ")+1:]
}

// TestManySubscriptionsManyKeys is the motivating setup: five keys and four
// subscriptions with overlapping access.
//
//	Claude (Personal)  keys 1, 2, 3
//	Cursor (Personal)  keys 4, 5, 1
//	Claude (Company)   keys 4, 5
//	Cursor (Friend)    no keys
func TestManySubscriptionsManyKeys(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	// The first start created "Claude"; rename it to fit the example.
	claudePersonal := s.ClaudeID()
	if code, out := s.Admin("PATCH", "/admin/api/subscriptions/"+claudePersonal, map[string]any{"name": "Claude (Personal)", "keys": []string{}}); code != 200 {
		t.Fatalf("rename: %d %v", code, out)
	}
	ids := make([]string, 6)
	secrets := make([]string, 6)
	for i := 1; i <= 5; i++ {
		ids[i], secrets[i] = s.CreateKeyFor("key"+string(rune('0'+i)), []string{})
	}
	s.Admin("PATCH", "/admin/api/subscriptions/"+claudePersonal, map[string]any{"keys": []string{ids[1], ids[2], ids[3]}})
	cursorPersonal := s.CreateSubscription("Cursor (Personal)", "cursor", ids[4], ids[5], ids[1])
	claudeCompany := s.CreateSubscription("Claude (Company)", "claude", ids[4], ids[5])
	cursorFriend := s.CreateSubscription("Cursor (Friend)", "cursor")

	s.SignInClaude(claudePersonal)
	s.SignInClaude(claudeCompany)
	s.SignInCursor(cursorPersonal)
	if code, _ := s.Admin("PUT", "/admin/api/subscriptions/"+cursorFriend+"/credential", map[string]any{"credential": fakecursor.ValidKey}); code != 200 {
		t.Fatal("friend key")
	}

	cases := []struct {
		key   int
		model string
		want  string
	}{
		{1, "sonnet", claudePersonal},
		{1, "auto", cursorPersonal}, // only Cursor knows "auto"
		{1, "", claudePersonal},     // highest priority first
		{2, "auto", claudePersonal}, // no Cursor for key 2: Claude with its default model
		{3, "opus", claudePersonal},
		{4, "sonnet", claudeCompany},
		{4, "gpt-5", cursorPersonal},
		{4, "", cursorPersonal}, // Cursor (Personal) has a higher priority than Claude (Company)
		{5, "claude-company/haiku", claudeCompany},
		{1, "cursor-personal/auto", cursorPersonal},
	}
	for _, c := range cases {
		if code, got := served(t, s, secrets[c.key], c.model); code != 200 || got != c.want {
			t.Errorf("key %d model %q: %d %s, want %s", c.key, c.model, code, got, c.want)
		}
	}

	// Pinning a subscription the key may not use is refused.
	if code, got := served(t, s, secrets[2], "cursor-personal/auto"); code != 403 || got != "no_subscription" {
		t.Errorf("key 2 pinned to Cursor (Personal): %d %s", code, got)
	}
	if code, got := served(t, s, secrets[1], "cursor-friend/auto"); code != 403 {
		t.Errorf("key 1 pinned to Cursor (Friend): %d %s", code, got)
	}

	// /v1/models lists what each key can reach, including pinned names.
	_, out := s.Do("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + secrets[2]})
	var models []string
	for _, m := range out["data"].([]any) {
		models = append(models, m.(map[string]any)["id"].(string))
	}
	joined := "," + strings.Join(models, ",") + ","
	if !strings.Contains(joined, ",claude-personal/sonnet,") || strings.Contains(joined, "cursor") {
		t.Fatalf("key 2 models %v", models)
	}

	// Priority can be changed: move Claude (Company) to the top.
	for i := 0; i < 3; i++ {
		s.Admin("POST", "/admin/api/subscriptions/"+claudeCompany+"/move", map[string]any{"direction": "up"})
	}
	if code, got := served(t, s, secrets[4], ""); code != 200 || got != claudeCompany {
		t.Errorf("after move: %d %s", code, got)
	}

	// Disabling a subscription takes it out of rotation; a key left with no
	// usable subscription gets 403.
	s.Admin("PATCH", "/admin/api/subscriptions/"+claudePersonal, map[string]any{"disabled": true})
	if code, got := served(t, s, secrets[1], "sonnet"); code != 200 || got != cursorPersonal {
		t.Errorf("key 1 with Claude (Personal) disabled: %d %s", code, got)
	}
	if code, got := served(t, s, secrets[2], "sonnet"); code != 403 || got != "no_subscription" {
		t.Errorf("key 2 with its only subscription disabled: %d %s", code, got)
	}

	// The key list shows each key's subscriptions.
	_, out = s.Admin("GET", "/admin/api/keys", nil)
	for _, k := range out["keys"].([]any) {
		km := k.(map[string]any)
		if km["id"] == ids[4] && len(km["subscriptions"].([]any)) != 2 {
			t.Errorf("key 4 grants %v", km["subscriptions"])
		}
	}
}

// TestFallbackToNextSubscription: when the preferred subscription is signed
// out or rate limited, the next one answers.
func TestFallbackToNextSubscription(t *testing.T) {
	s := support.NewServer(t, support.Options{CursorKey: fakecursor.ValidKey})
	key := s.CreateKey("app")
	claudeID, cursorID := s.ClaudeID(), s.SubscriptionID("Cursor")

	// Claude is not signed in: Cursor answers, and says so in the logs only.
	if code, got := served(t, s, key, "sonnet"); code != 200 || got != cursorID {
		t.Fatalf("fallback while Claude is signed out: %d %s", code, got)
	}
	// A rate-limited reply also falls through to the next subscription.
	s.SignIn()
	code, reply, _ := s.Ask(key, "sonnet", "[fake:rate-limit] hi")
	if code != 429 {
		// Cursor also sees the directive, so both are limited.
		t.Fatalf("both rate limited: %d %s", code, reply)
	}
	if code, got := served(t, s, key, "sonnet"); code != 200 || got != claudeID {
		t.Fatalf("Claude back: %d %s", code, got)
	}

	// A pinned request never falls back.
	s.Admin("DELETE", "/admin/api/subscriptions/"+claudeID+"/credential", nil)
	if code, got := served(t, s, key, "claude/sonnet"); code != 401 || got != "subscription_login_required" {
		t.Fatalf("pinned to a signed-out subscription: %d %s", code, got)
	}

	// Both signed out (a saved key overrides CURSOR_API_KEY): the error
	// names both and links to the first.
	s.Admin("PUT", "/admin/api/subscriptions/"+cursorID+"/credential", map[string]any{"credential": fakecursor.ExpiredKey})
	_, out := s.Chat(key, hello)
	msg := errorOf(t, out)["message"].(string)
	if !strings.Contains(msg, "Claude, Cursor are signed out") || !strings.Contains(msg, "/login?subscription="+claudeID) {
		t.Fatalf("message %q", msg)
	}
}

func TestStrictModelsWithSubscriptions(t *testing.T) {
	s := support.NewServer(t, support.Options{StrictModels: true, EnvToken: "sk-ant-oat01-fake-0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"})
	key := s.CreateKey("app")
	if code, got := served(t, s, key, "auto"); code != 404 || got != "model_not_found" {
		t.Fatalf("strict unknown model: %d %s", code, got)
	}
	if code, _ := served(t, s, key, "haiku"); code != 200 {
		t.Fatalf("strict known model: %d", code)
	}
}

func TestKeyWithoutSubscriptions(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	_, key := s.CreateKeyFor("lonely", []string{})
	code, _, out := s.Ask(key, "sonnet", "hi")
	e := errorOf(t, out)
	if code != 403 || e["code"] != "no_subscription" || e["type"] != "permission_error" || !strings.Contains(e["message"].(string), "/admin") {
		t.Fatalf("%d %v", code, out)
	}
	resp, out := s.Do("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + key})
	if resp.StatusCode != 200 || len(out["data"].([]any)) != 0 {
		t.Fatalf("models %d %v", resp.StatusCode, out)
	}
}

func TestDeleteSubscriptionRemovesAccess(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	id, key := s.CreateKeyFor("app", []string{})
	cur := s.CreateSubscription("Cursor", "cursor", id)
	s.Admin("PUT", "/admin/api/subscriptions/"+cur+"/credential", map[string]any{"credential": fakecursor.ValidKey})
	if code, _, _ := s.Ask(key, "", "hi"); code != 200 {
		t.Fatalf("before delete: %d", code)
	}
	if code, _ := s.Admin("DELETE", "/admin/api/subscriptions/"+cur, nil); code != 200 {
		t.Fatal("delete")
	}
	if code, _, _ := s.Ask(key, "", "hi"); code != 403 {
		t.Fatalf("after delete: %d", code)
	}
}
