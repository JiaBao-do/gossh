package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeNeeded(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.1.0", "v1.1.0", false},
		{"v1.2.0", "v1.1.9", false},
		{"v1.9.9", "v1.10.0", true},
		{"dev", "v1.1.0", true},
		{"v1.1.0-3-gabc123-dirty", "v1.1.0", true},
	}
	for _, tt := range tests {
		if got, why := upgradeNeeded(tt.current, tt.latest); got != tt.want {
			t.Errorf("upgradeNeeded(%q, %q) = %v (%s), want %v", tt.current, tt.latest, got, why, tt.want)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("binary")
	sum := sha256.Sum256(data)
	sums := []byte("deadbeef  gossh-linux-amd64\n" + hex.EncodeToString(sum[:]) + " *gossh-windows-amd64.exe\n")
	if err := verifyChecksum(sums, "gossh-windows-amd64.exe", data); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := verifyChecksum(sums, "gossh-linux-amd64", data); err == nil {
		t.Error("wrong checksum accepted")
	}
	if err := verifyChecksum(sums, "gossh-darwin-arm64", data); err == nil {
		t.Error("missing checksum accepted")
	}
}

func TestReplaceExecutable(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "gossh.exe")
	writeFile(t, exe, "old")
	if err := replaceExecutable(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, exe); got != "new" {
		t.Errorf("exe = %q, want new", got)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Error("temporary .new file left behind")
	}
}

func TestDiscoverKeys(t *testing.T) {
	home, keysDir := fakeHome(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\nabc\n"
	writeKey(t, filepath.Join(home, ".ssh", "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\n")
	writeKey(t, filepath.Join(home, ".ssh", "id_ed25519.pub"), "ssh-ed25519 AAAA")
	writeKey(t, filepath.Join(home, ".ssh", "known_hosts"), "host ssh-ed25519 AAAA")
	writeKey(t, filepath.Join(home, "Downloads", "prod.pem"), pem)
	writeKey(t, filepath.Join(home, "Downloads", "notes.txt"), pem) // not a key file name
	writeKey(t, filepath.Join(keysDir, "managed.pem"), pem)         // already managed

	got := map[string]bool{}
	for _, p := range discoverKeys() {
		got[filepath.Base(p)] = true
	}
	if !got["id_ed25519"] || !got["prod.pem"] || len(got) != 2 {
		t.Errorf("discoverKeys = %v, want id_ed25519 and prod.pem", got)
	}
}

func TestChosenKey(t *testing.T) {
	for _, tt := range []struct{ choice, want string }{
		{keyChoiceKeep, "~/old"},
		{keyChoiceNone, ""},
		{keyChoiceBrowse, "/picked"},
		{keyChoiceType, "typed"},
		{"~/.ssh/keys/k", "~/.ssh/keys/k"},
	} {
		if got := chosenKey(tt.choice, "~/old", "/picked", ` "typed" `); got != tt.want {
			t.Errorf("chosenKey(%q) = %q, want %q", tt.choice, got, tt.want)
		}
	}
}
