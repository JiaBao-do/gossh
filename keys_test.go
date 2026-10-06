package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeHome points the home directory at a temp dir with the gossh layout.
func fakeHome(t *testing.T) (home, keysDir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := ensureSSHLayout(); err != nil {
		t.Fatal(err)
	}
	return home, filepath.Join(home, ".ssh", "keys")
}

func writeKey(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0700)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestResolveIdentityFileModes(t *testing.T) {
	home, keysDir := fakeHome(t)
	src := filepath.Join(home, "old", "prod.pem")

	writeKey(t, src, "KEY")
	writeKey(t, src+".pub", "PUB")
	if !needsImport(src) {
		t.Fatal("external key should need import")
	}

	got, err := resolveIdentityFile(src, keyKeep)
	if err != nil || got != src {
		t.Fatalf("keep: got %q, %v", got, err)
	}

	got, err = resolveIdentityFile(src, keyCopy)
	if err != nil || got != "~/.ssh/keys/prod.pem" {
		t.Fatalf("copy: got %q, %v", got, err)
	}
	if !exists(src) || !exists(filepath.Join(keysDir, "prod.pem.pub")) {
		t.Fatal("copy should keep the original and bring the .pub along")
	}

	// Same content again: reused, not duplicated. Then move removes the original.
	got, err = resolveIdentityFile(src, keyMove)
	if err != nil || got != "~/.ssh/keys/prod.pem" {
		t.Fatalf("move: got %q, %v", got, err)
	}
	if exists(src) || exists(src+".pub") {
		t.Fatal("move should remove the original key and .pub")
	}
	if exists(filepath.Join(keysDir, "prod-1.pem")) {
		t.Fatal("identical key should not be duplicated")
	}

	// A bare name of a managed key resolves without asking.
	if needsImport("prod.pem") {
		t.Fatal("managed key name should not need import")
	}
	if got, _ := resolveIdentityFile("prod.pem", keyCopy); got != "~/.ssh/keys/prod.pem" {
		t.Fatalf("bare name: got %q", got)
	}
}

func TestImportNameCollision(t *testing.T) {
	home, keysDir := fakeHome(t)
	writeKey(t, filepath.Join(keysDir, "id_rsa"), "EXISTING")
	src := filepath.Join(home, "other", "id_rsa")
	writeKey(t, src, "DIFFERENT")

	got, err := resolveIdentityFile(src, keyCopy)
	if err != nil || got != "~/.ssh/keys/id_rsa-1" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestValidateIdentityFileAllowsUnchangedMissingKey(t *testing.T) {
	fakeHome(t)
	validate := validateIdentityFile("~/gone/old_key")
	if err := validate("~/gone/old_key"); err != nil {
		t.Errorf("unchanged missing key should be accepted: %v", err)
	}
	if err := validate("~/gone/other_key"); err == nil {
		t.Error("new missing key should be rejected")
	}
}

func TestKeyUsedBy(t *testing.T) {
	home, _ := fakeHome(t)
	shared := filepath.Join(home, "old", "shared")
	cfg := "IdentityFile ~/old/shared\n\nHost a\n  IdentityFile ~/old/shared\n\nHost b\n  IdentityFile ~/old/shared\n"
	path := filepath.Join(home, ".ssh", "config")
	writeKey(t, path, cfg)
	entries, _ := parseConfig(path)

	users := keyUsedBy(entries, shared, map[int]bool{findHost(entries, "a"): true})
	if len(users) != 2 || users[0] != "global settings" || users[1] != "b" {
		t.Errorf("users = %v", users)
	}
}
