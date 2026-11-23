package keys

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create("  "); err == nil {
		t.Fatal("empty name should fail")
	}
	k, secret, err := s.Create("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, KeyPrefix) || len(secret) < 40 {
		t.Fatalf("secret %q", secret)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), secret) {
		t.Fatal("plaintext secret must not be stored")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode().Perm())
	}
	if got := s.Authenticate(secret); got == nil || got.ID != k.ID {
		t.Fatal("authenticate failed")
	}
	if s.Authenticate(secret+"x") != nil || s.Authenticate("") != nil {
		t.Fatal("wrong secret accepted")
	}

	s.RecordUsage(k.ID, 100, 20)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].Requests != 1 || list[0].InputTokens != 100 || list[0].Hash != "" {
		t.Fatalf("list %+v", list)
	}
	if s2.Authenticate(secret) == nil {
		t.Fatal("key not persisted")
	}

	off := true
	if _, err := s2.Update(k.ID, nil, &off); err != nil {
		t.Fatal(err)
	}
	if s2.Authenticate(secret) != nil {
		t.Fatal("disabled key accepted")
	}
	if err := s2.Delete(k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(k.ID); err == nil {
		t.Fatal("double delete should fail")
	}
}

func TestLegacyPrefixStillAuthenticates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := LegacyKeyPrefix + "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefg"
	k := &Key{ID: "legacy01", Name: "old", Hash: hashKey(legacy)}
	s.keys[k.ID] = k
	s.byHash[k.Hash] = k
	if got := s.Authenticate(legacy); got == nil || got.ID != "legacy01" {
		t.Fatal("legacy sk-cmta- key should still authenticate")
	}
	if s.Authenticate("sk-other-"+legacy) != nil {
		t.Fatal("unknown prefix accepted")
	}
}

func TestConcurrentUsageAndFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, _ := Open(path)
	k, _, err := s.Create("busy")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.RecordUsage(k.ID, 3, 2) }()
		go func() { defer wg.Done(); _ = s.Flush() }()
	}
	wg.Wait()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, _ := Open(path)
	got := again.List()[0]
	if got.Requests != 20 || got.InputTokens != 60 || got.OutputTokens != 40 {
		t.Fatalf("usage not saved: %+v", got)
	}
	if s.Authenticate("") != nil || s.Authenticate("sk-other-x") != nil {
		t.Fatal("foreign prefixes must not authenticate")
	}
	if _, err := s.Update("missing", nil, nil); err == nil {
		t.Fatal("updating a missing key should fail")
	}
}

func TestSaveFailureKeepsStoreConsistent(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "sub", "keys.json"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, _, err := s.Create("x"); err == nil {
		t.Fatal("expected a save error")
	}
	if len(s.List()) != 0 {
		t.Fatal("a key that could not be saved must not be kept")
	}
}
