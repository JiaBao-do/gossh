package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// drive runs cmd and feeds the resulting messages back into the model,
// the way the Bubble Tea runtime would. Ticks and quit are dropped.
func drive(m *filesModel, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil, jobTickMsg, tea.QuitMsg:
	case tea.BatchMsg:
		for _, c := range msg {
			drive(m, c)
		}
	default:
		_, next := m.Update(msg)
		drive(m, next)
	}
}

func press(m *filesModel, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := m.Update(msg)
		drive(m, cmd)
	}
}

func newTestBrowser(t *testing.T) (*filesModel, string, string) {
	left, right := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(left, "x.txt"), "x")
	writeFile(t, filepath.Join(left, "d", "inner.txt"), "inner")
	writeFile(t, filepath.Join(left, "y.txt"), "y")
	m := &filesModel{panes: [2]*pane{
		{fs: localFS{}, dir: left, marked: map[string]bool{}},
		{fs: localFS{}, dir: right, marked: map[string]bool{}},
	}}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	drive(m, m.Init())
	return m, left, right
}

func TestBrowserCopyMarked(t *testing.T) {
	m, _, right := newTestBrowser(t)
	// Entries: "..", "d", "x.txt", "y.txt". Mark d and x.txt, then copy.
	press(m, "down", "space", "space", "c")
	if m.job != nil {
		t.Fatal("job should have finished")
	}
	if m.statusErr {
		t.Fatalf("status error: %s", m.status)
	}
	for _, p := range []string{"x.txt", filepath.Join("d", "inner.txt")} {
		if _, err := os.Stat(filepath.Join(right, p)); err != nil {
			t.Errorf("%s not copied: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(right, "y.txt")); err == nil {
		t.Error("unmarked y.txt was copied")
	}
	if !m.panes[1].has("x.txt") {
		t.Error("destination pane was not refreshed")
	}

	// Copying again asks before overwriting; "n" leaves things alone.
	press(m, "k", "c") // cursor sat on y.txt; x.txt now exists on the right
	if m.prompt == nil || !strings.Contains(m.prompt.question, "Overwrite") {
		t.Fatalf("expected overwrite prompt, got %+v", m.prompt)
	}
	press(m, "n")
	if m.prompt != nil || m.job != nil {
		t.Error("prompt should be gone with no transfer started")
	}

	if v := m.View(); !strings.Contains(v, "x.txt") || !strings.Contains(v, "local: ") {
		t.Errorf("view missing content:\n%s", v)
	}
}

func TestBrowserDeleteAndNavigate(t *testing.T) {
	m, left, _ := newTestBrowser(t)
	press(m, "down", "enter") // into d/
	if m.panes[0].dir != filepath.Join(left, "d") {
		t.Fatalf("dir = %s", m.panes[0].dir)
	}
	press(m, "h") // back up, cursor lands on d
	if e := m.panes[0].selected(); m.panes[0].dir != left || e == nil || e.name != "d" {
		t.Fatalf("after going up: dir=%s selected=%+v", m.panes[0].dir, e)
	}
	press(m, "d", "y")
	if _, err := os.Stat(filepath.Join(left, "d")); !os.IsNotExist(err) {
		t.Errorf("d not deleted: %v", err)
	}
	if m.panes[0].has("d") {
		t.Error("pane still lists d")
	}
}
