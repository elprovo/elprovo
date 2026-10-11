package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "secrets.json"), filepath.Join(dir, "secrets.key"))
}

func TestStoredSecretsAreSealedAndReadBack(t *testing.T) {
	s := newStore(t)
	if err := s.Set("db_password", "hunter2-very-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("the store file holds the value in plain text")
	}
	values, err := s.Values()
	if err != nil || values["db_password"] != "hunter2-very-secret" {
		t.Fatalf("Values = %v, %v", values, err)
	}
	if info, _ := os.Stat(s.keyPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestStoreWithAnotherKeyCannotBeOpened(t *testing.T) {
	s := newStore(t)
	if err := s.Set("a", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := os.WriteFile(s.keyPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Values(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Values err = %v, want a key mismatch", err)
	}
}

func TestDeleteRemovesOneKey(t *testing.T) {
	s := newStore(t)
	for _, k := range []string{"b", "a"} {
		if err := s.Set(k, "v"); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := s.Delete("b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if keys, err := s.Keys(); err != nil || !reflect.DeepEqual(keys, []string{"a"}) {
		t.Fatalf("Keys = %v, %v; want [a]", keys, err)
	}
}

func TestInvalidKeyIsRejected(t *testing.T) {
	if err := newStore(t).Set("has space", "v"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey", err)
	}
}

func TestResolverPrefersThePipelineFile(t *testing.T) {
	s := newStore(t)
	if err := s.Set("api_token", "from-store"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("db", "store-db"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "secrets.env")
	content := "# pipeline secrets\nexport api_token='from-file'\n\nother=\"x y\"\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Resolver{Store: s, File: file}.Resolve([]string{"api_token", "db", "other"})
	want := map[string]string{"api_token": "from-file", "db": "store-db", "other": "x y"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve = %v, %v; want %v", got, err, want)
	}
}

func TestMissingSecretErrorNamesTheKeyOnly(t *testing.T) {
	s := newStore(t)
	if err := s.Set("present", "s3cr3t-value"); err != nil {
		t.Fatal(err)
	}
	_, err := Resolver{Store: s}.Resolve([]string{"present", "absent"})
	if err == nil || !strings.Contains(err.Error(), `"absent"`) || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v, want it to name \"absent\" and no value", err)
	}
}
