package keys

import (
	"os"
	"path/filepath"
	"strings"
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
