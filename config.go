package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SSHEntry is one block of ~/.ssh/config. Only Host blocks with a single plain
// alias are shown and editable; everything else (the preamble before the first
// Host, Match blocks, wildcard hosts, comments, unknown directives) is kept
// verbatim so gossh never drops configuration it doesn't understand.
type SSHEntry struct {
	Alias        string
	HostName     string
	User         string
	Port         string
	IdentityFile string

	keyword   string // "host", "match", or "" for the preamble
	header    string // original header line
	origAlias string
	lines     []configLine      // body lines in original order
	orig      map[string]string // first parsed value of each managed key
}

type configLine struct {
	key string // lower-cased directive, "" for blank lines and comments
	raw string
}

var managedKeys = []string{"hostname", "user", "port", "identityfile"}

func isManaged(key string) bool {
	return slices.Contains(managedKeys, key)
}

func (e *SSHEntry) field(key string) *string {
	switch key {
	case "hostname":
		return &e.HostName
	case "user":
		return &e.User
	case "port":
		return &e.Port
	case "identityfile":
		return &e.IdentityFile
	}
	return nil
}

// Editable reports whether the entry is a plain "Host <alias>" block gossh manages.
func (e SSHEntry) Editable() bool {
	return e.keyword == "host" && e.Alias != "" && !strings.ContainsAny(e.Alias, "*?!, \t")
}

// hosts returns only the entries shown in lists and forms.
func hosts(entries []SSHEntry) []SSHEntry {
	var out []SSHEntry
	for _, e := range entries {
		if e.Editable() {
			out = append(out, e)
		}
	}
	return out
}

func findHost(entries []SSHEntry, alias string) int {
	for i, e := range entries {
		if e.Editable() && e.Alias == alias {
			return i
		}
	}
	return -1
}

func newHostEntry() SSHEntry {
	return SSHEntry{keyword: "host"}
}

// splitDirective parses "Key value", "Key=value" or `Key "quoted value"`.
func splitDirective(line string) (key, value string) {
	line = strings.TrimSpace(line)
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), ""
	}
	key = strings.ToLower(line[:i])
	value = strings.TrimSpace(line[i:])
	value = strings.TrimSpace(strings.TrimPrefix(value, "="))
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return key, value
}

func parseConfig(path string) ([]SSHEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHEntry{}, nil
		}
		return nil, err
	}
	defer file.Close()

	entries := []SSHEntry{{orig: map[string]string{}}} // preamble
	cur := &entries[0]

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			cur.lines = append(cur.lines, configLine{raw: raw})
			continue
		}
		key, value := splitDirective(trimmed)
		if key == "host" || key == "match" {
			entries = append(entries, SSHEntry{keyword: key, header: raw, orig: map[string]string{}})
			cur = &entries[len(entries)-1]
			if key == "host" {
				cur.Alias, cur.origAlias = value, value
			}
			continue
		}
		cur.lines = append(cur.lines, configLine{key: key, raw: raw})
		if cur.keyword == "host" && isManaged(key) {
			if _, seen := cur.orig[key]; !seen {
				cur.orig[key] = value
				*cur.field(key) = value
			}
		}
	}
	return entries, scanner.Err()
}

func formatDirective(indent, name, value string) string {
	if strings.ContainsAny(value, " \t") {
		value = `"` + value + `"`
	}
	return indent + name + " " + value
}

// blockIndent returns the indentation used by a block's directives ("  " if none).
func blockIndent(lines []configLine) string {
	for _, l := range lines {
		if l.key != "" {
			if n := len(l.raw) - len(strings.TrimLeft(l.raw, " \t")); n > 0 {
				return l.raw[:n]
			}
		}
	}
	return "  "
}

var directiveNames = map[string]string{
	"hostname": "HostName", "user": "User", "port": "Port", "identityfile": "IdentityFile",
}

func renderEntry(w *bufio.Writer, e SSHEntry) {
	if e.keyword == "" { // preamble
		for _, l := range e.lines {
			fmt.Fprintln(w, l.raw)
		}
		return
	}
	if e.keyword != "host" {
		fmt.Fprintln(w, e.header)
		for _, l := range e.lines {
			fmt.Fprintln(w, l.raw)
		}
		return
	}

	if e.header != "" && e.Alias == e.origAlias {
		fmt.Fprintln(w, e.header)
	} else {
		fmt.Fprintln(w, "Host "+e.Alias)
	}

	// Trailing blank lines and comments stay at the end, after any new directives.
	bodyEnd := len(e.lines)
	for bodyEnd > 0 && e.lines[bodyEnd-1].key == "" {
		bodyEnd--
	}

	indent := blockIndent(e.lines)
	done := map[string]bool{}
	for _, l := range e.lines[:bodyEnd] {
		if !isManaged(l.key) {
			fmt.Fprintln(w, l.raw)
			continue
		}
		val := *e.field(l.key)
		if val == e.orig[l.key] || done[l.key] {
			// Unchanged lines keep their exact formatting; later duplicates are
			// ignored by ssh but kept so nothing is lost.
			fmt.Fprintln(w, l.raw)
			done[l.key] = true
			continue
		}
		done[l.key] = true
		if val != "" {
			fmt.Fprintln(w, formatDirective(indent, directiveNames[l.key], val))
		}
	}
	for _, k := range managedKeys {
		if !done[k] && *e.field(k) != "" {
			fmt.Fprintln(w, formatDirective(indent, directiveNames[k], *e.field(k)))
		}
	}
	for _, l := range e.lines[bodyEnd:] {
		fmt.Fprintln(w, l.raw)
	}
}

// endsWithBlank reports whether an entry's output already ends in a blank line.
func endsWithBlank(e SSHEntry) bool {
	if len(e.lines) == 0 {
		return e.keyword == "" // an empty preamble needs no separator
	}
	return strings.TrimSpace(e.lines[len(e.lines)-1].raw) == ""
}

// writeConfig saves entries, keeping a backup of the previous file in config.bak.
func writeConfig(path string, entries []SSHEntry) error {
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		if err := os.WriteFile(path+".bak", old, 0600); err != nil {
			return fmt.Errorf("writing backup: %w", err)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	w := bufio.NewWriter(tmp)
	for i, e := range entries {
		if i > 0 && !endsWithBlank(entries[i-1]) {
			fmt.Fprintln(w)
		}
		renderEntry(w, e)
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0600)
	return os.Rename(tmp.Name(), path)
}
