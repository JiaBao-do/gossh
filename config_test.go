package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `# my global settings
Include ~/.ssh/config.d/*
ServerAliveInterval 60

Host *.corp
    ProxyJump bastion
    User admin

Host prod
    HostName 10.0.0.5
    User deploy
    IdentityFile ~/old/prod_key
    ProxyJump bastion
    # keep this comment
    LocalForward 5432 localhost:5432

Host bastion
  HostName=bastion.example.com
  Port 2222

Match host *.internal
    ForwardAgent yes
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRoundTripUnchanged(t *testing.T) {
	path := writeTemp(t, sampleConfig)
	entries, err := parseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, entries); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != sampleConfig {
		t.Errorf("round trip changed the file:\n%s", got)
	}
	if got := readFile(t, path+".bak"); got != sampleConfig {
		t.Errorf("backup does not match original")
	}
}

func TestOnlyPlainHostsAreEditable(t *testing.T) {
	entries, _ := parseConfig(writeTemp(t, sampleConfig))
	var aliases []string
	for _, e := range hosts(entries) {
		aliases = append(aliases, e.Alias)
	}
	if strings.Join(aliases, ",") != "prod,bastion" {
		t.Errorf("editable hosts = %v", aliases)
	}
	b := entries[findHost(entries, "bastion")]
	if b.HostName != "bastion.example.com" || b.Port != "2222" {
		t.Errorf("bastion parsed as %+v", b)
	}
}

func TestEditKeepsUnknownDirectives(t *testing.T) {
	path := writeTemp(t, sampleConfig)
	entries, _ := parseConfig(path)
	i := findHost(entries, "prod")
	entries[i].HostName = "10.0.0.9"
	entries[i].Port = "2200"
	entries[i].IdentityFile = ""
	if err := writeConfig(path, entries); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, want := range []string{
		"Include ~/.ssh/config.d/*",
		"Host *.corp\n    ProxyJump bastion",
		"    HostName 10.0.0.9\n",
		"    ProxyJump bastion\n    # keep this comment\n    LocalForward 5432 localhost:5432\n",
		"Match host *.internal\n    ForwardAgent yes",
		"  Port 2200\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "prod_key") {
		t.Errorf("cleared IdentityFile still present:\n%s", got)
	}
	// New directive goes before the trailing comment/blank lines of the block.
	if !strings.Contains(got, "  Port 2200\n\nHost bastion") {
		t.Errorf("new Port not placed at end of prod block:\n%s", got)
	}
}

func TestAddAndDelete(t *testing.T) {
	path := writeTemp(t, "Host a\n  HostName a.example\n")
	entries, _ := parseConfig(path)
	e := newHostEntry()
	e.Alias, e.HostName, e.User = "b", "b.example", "me"
	entries = append(entries, e)
	if err := writeConfig(path, entries); err != nil {
		t.Fatal(err)
	}
	want := "Host a\n  HostName a.example\n\nHost b\n  HostName b.example\n  User me\n"
	if got := readFile(t, path); got != want {
		t.Errorf("after add:\n%q\nwant\n%q", got, want)
	}

	entries, _ = parseConfig(path)
	i := findHost(entries, "a")
	entries = append(entries[:i], entries[i+1:]...)
	writeConfig(path, entries)
	if got := readFile(t, path); got != "Host b\n  HostName b.example\n  User me\n" {
		t.Errorf("after delete:\n%q", got)
	}
}

func TestEmptyConfig(t *testing.T) {
	path := writeTemp(t, "")
	entries, _ := parseConfig(path)
	e := newHostEntry()
	e.Alias, e.HostName = "x", "1.2.3.4"
	writeConfig(path, append(entries, e))
	if got := readFile(t, path); got != "Host x\n  HostName 1.2.3.4\n" {
		t.Errorf("got %q", got)
	}
}
