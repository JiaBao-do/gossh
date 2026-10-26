package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// harness is a tiny Bubble Tea runtime: commands run in goroutines and
// their messages are fed back into the model on the test goroutine.
type harness struct {
	t    *testing.T
	m    *filesModel
	msgs chan tea.Msg
}

func newHarness(t *testing.T) (*harness, string, string) {
	left, right := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(left, "x.txt"), "x")
	writeFile(t, filepath.Join(left, "d", "inner.txt"), "inner")
	writeFile(t, filepath.Join(left, "y.txt"), "y")
	h := &harness{t: t, m: newFilesModel(localFS{}, left, localFS{}, right), msgs: make(chan tea.Msg, 100)}
	h.m.active = focusLeft
	h.m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	h.run(h.m.Init())
	h.waitFor("panes loaded", func() bool { return !h.m.panes[0].loading && !h.m.panes[1].loading })
	t.Cleanup(h.m.cancelAll)
	return h, left, right
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		switch msg := cmd().(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range msg {
				h.run(c)
			}
		default:
			h.msgs <- msg
		}
	}()
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	timeout := time.After(10 * time.Second)
	for !cond() {
		select {
		case msg := <-h.msgs:
			_, cmd := h.m.Update(msg)
			h.run(cmd)
		case <-timeout:
			h.t.Fatalf("timed out waiting for %s\n%s", what, h.m.View())
		}
	}
}

func (h *harness) press(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		_, cmd := h.m.Update(msg)
		h.run(cmd)
	}
}

func (h *harness) queueIdle() bool {
	if h.m.running != nil {
		return false
	}
	for _, t := range h.m.queue {
		if !t.finished() {
			return false
		}
	}
	return true
}

func TestFilesCopyMarkedThroughQueue(t *testing.T) {
	h, _, right := newHarness(t)
	// Entries: "..", "d", "x.txt", "y.txt". Mark d and x.txt, then copy.
	h.press("down", "space", "space", "c")
	if len(h.m.queue) != 2 {
		t.Fatalf("queue has %d tasks, want 2", len(h.m.queue))
	}
	h.waitFor("queue to finish", h.queueIdle)
	for _, task := range h.m.queue {
		if task.state != taskDone {
			t.Errorf("%s: state %d, err %v", task.name, task.state, task.err)
		}
	}
	for _, p := range []string{"x.txt", filepath.Join("d", "inner.txt")} {
		if _, err := os.Stat(filepath.Join(right, p)); err != nil {
			t.Errorf("%s not copied: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(right, "y.txt")); err == nil {
		t.Error("unmarked y.txt was copied")
	}
	h.waitFor("destination refresh", func() bool { return h.m.panes[1].has("x.txt") })

	v := h.m.View()
	for _, want := range []string{"THIS COMPUTER", "TRANSFERS", "2 done", "x.txt"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
}

func TestFilesConflictPrompt(t *testing.T) {
	h, _, right := newHarness(t)
	writeFile(t, filepath.Join(right, "x.txt"), "existing")
	h.run(h.m.reloadAll())

	// Cursor to x.txt and copy: the file exists on the right, so we get asked.
	h.press("down", "down", "c")
	h.waitFor("conflict prompt", func() bool { return h.m.pending != nil })
	if v := h.m.View(); !strings.Contains(v, "already exists") || !strings.Contains(v, "keep both") {
		t.Errorf("conflict prompt not shown:\n%s", v)
	}
	h.press("s") // skip
	h.waitFor("queue to finish", h.queueIdle)
	if got := readFile(t, filepath.Join(right, "x.txt")); got != "existing" {
		t.Errorf("skip overwrote the file: %q", got)
	}

	h.press("c")
	h.waitFor("conflict prompt", func() bool { return h.m.pending != nil })
	h.press("r") // keep both
	h.waitFor("queue to finish", h.queueIdle)
	if got := readFile(t, filepath.Join(right, "x (1).txt")); got != "x" {
		t.Errorf("keep both: copy = %q", got)
	}

	// "S" (skip for all) answers every conflict in the queue at once.
	writeFile(t, filepath.Join(right, "y.txt"), "existing y")
	h.run(h.m.reloadAll())
	h.press("a", "c")
	h.waitFor("conflict prompt", func() bool { return h.m.pending != nil })
	h.press("S")
	h.waitFor("queue to finish", h.queueIdle)
	if h.m.pending != nil {
		t.Error("asked again after answering for all")
	}
	if got := readFile(t, filepath.Join(right, "y.txt")); got != "existing y" {
		t.Errorf("skip-all overwrote y.txt: %q", got)
	}
	if got := readFile(t, filepath.Join(right, "x.txt")); got != "existing" {
		t.Errorf("skip-all overwrote x.txt: %q", got)
	}
	if h.m.conflictAll != conflictAsk {
		t.Error("answer for all should reset once the queue is done")
	}
}

func TestFilesQueuePanelKeys(t *testing.T) {
	h, _, _ := newHarness(t)
	h.press("down", "down", "c")
	h.waitFor("queue to finish", h.queueIdle)

	h.press("tab", "tab") // left -> right -> transfers
	if h.m.active != focusQueue {
		t.Fatalf("focus = %d, want transfers panel", h.m.active)
	}
	h.press("enter")
	if h.m.overlay == nil || !strings.Contains(strings.Join(h.m.overlay.lines, "\n"), "Copied:  1 file(s)") {
		t.Fatalf("details overlay = %+v", h.m.overlay)
	}
	h.press("esc", "c") // close details, clear finished
	if len(h.m.queue) != 0 {
		t.Errorf("clear finished left %d tasks", len(h.m.queue))
	}
}

func TestFilesHelpOverlay(t *testing.T) {
	h, _, _ := newHarness(t)
	h.press("?")
	if h.m.overlay == nil {
		t.Fatal("? should open help")
	}
	if v := h.m.View(); !strings.Contains(v, "HOW TO COPY") {
		t.Errorf("help not rendered:\n%s", v)
	}
	h.press("esc")
	if h.m.overlay != nil {
		t.Error("esc should close help")
	}
}

func TestFilesDeleteAndNavigate(t *testing.T) {
	h, left, _ := newHarness(t)
	h.press("down", "enter") // into d/
	h.waitFor("enter d", func() bool { return h.m.panes[0].dir == filepath.Join(left, "d") })
	h.press("h") // back up, cursor lands on d
	h.waitFor("back up", func() bool { return h.m.panes[0].dir == left })
	if e := h.m.panes[0].selected(); e == nil || e.name != "d" {
		t.Fatalf("after going up the cursor is on %+v", e)
	}
	h.press("d", "y")
	h.waitFor("delete", func() bool { return !h.m.panes[0].has("d") })
	if _, err := os.Stat(filepath.Join(left, "d")); !os.IsNotExist(err) {
		t.Errorf("d not deleted: %v", err)
	}
}
