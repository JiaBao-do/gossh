package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// gossh files: an interactive two-pane (this computer | host) file manager
// over SFTP with a transfer queue, in the spirit of FileZilla.

// runFiles connects to host and runs the file manager until the user quits.
func runFiles(host, remoteDir string, sudo bool) error {
	fmt.Printf("Connecting to %s...\n", host)
	sess, err := connectSFTP(host, connectOptions{sudo: sudo})
	if err != nil {
		return err
	}
	defer sess.Close()

	localDir, err := os.Getwd()
	if err != nil {
		localDir, _ = localFS{}.Abs("~")
	}
	remoteDir, err = sess.fs.Abs(remoteDir)
	if err != nil {
		remoteDir = sess.fs.home
	}

	m := newFilesModel(localFS{}, localDir, sess.fs, remoteDir)
	m.sudo = sudo
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	m.cancelAll()
	return err
}

func newFilesModel(left fileSystem, leftDir string, right fileSystem, rightDir string) *filesModel {
	return &filesModel{
		panes: [2]*pane{
			{fs: left, dir: leftDir, marked: map[string]bool{}},
			{fs: right, dir: rightDir, marked: map[string]bool{}},
		},
		active:      1,
		conflicts:   make(chan *conflictReq),
		conflictAll: conflictAsk,
		status:      "Tip: put the cursor on a file (or mark several with space) and press c to copy it to the other side. Press ? for help.",
	}
}

// --- Directory listing ---

type fileEntry struct {
	name  string
	info  os.FileInfo // resolved through symlinks when possible
	isDir bool
	link  bool
	up    bool // the ".." entry
}

// listDir lists dir with ".." first, then directories, then files.
func listDir(fsys fileSystem, dir string, showHidden bool) ([]fileEntry, error) {
	infos, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]fileEntry, 0, len(infos)+1)
	for _, fi := range infos {
		name := fi.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		e := fileEntry{name: name, info: fi, isDir: fi.IsDir()}
		if fi.Mode()&os.ModeSymlink != 0 {
			e.link = true
			if target, err := fsys.Stat(fsys.Join(dir, name)); err == nil {
				e.info, e.isDir = target, target.IsDir()
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].isDir != entries[j].isDir {
			return entries[i].isDir
		}
		return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
	})
	if fsys.Dir(dir) != dir {
		entries = append([]fileEntry{{name: "..", isDir: true, up: true}}, entries...)
	}
	return entries, nil
}

type pane struct {
	fs      fileSystem
	dir     string
	entries []fileEntry
	cursor  int
	offset  int
	marked  map[string]bool
	loading bool
}

func (p *pane) selected() *fileEntry {
	if p.cursor < 0 || p.cursor >= len(p.entries) {
		return nil
	}
	return &p.entries[p.cursor]
}

// targets returns the marked entries, or the entry under the cursor.
func (p *pane) targets() []fileEntry {
	var out []fileEntry
	for _, e := range p.entries {
		if p.marked[e.name] {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		return out
	}
	if e := p.selected(); e != nil && !e.up {
		return []fileEntry{*e}
	}
	return nil
}

func entryNames(entries []fileEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

func (p *pane) has(name string) bool {
	for _, e := range p.entries {
		if !e.up && e.name == name {
			return true
		}
	}
	return false
}

func (p *pane) move(delta int) {
	p.cursor += delta
	p.cursor = max(0, min(p.cursor, len(p.entries)-1))
}

func scrollWindow(cursor, offset, height, n int) int {
	if cursor < offset {
		offset = cursor
	}
	if cursor >= offset+height {
		offset = cursor - height + 1
	}
	return max(0, min(offset, n-height))
}

// paneLabel is how a side is named on screen.
func paneLabel(fsys fileSystem) string {
	if fsys.Remote() {
		return fsys.Label()
	}
	return "this computer"
}

// --- Transfer queue ---

type taskState int

const (
	taskQueued taskState = iota
	taskRunning
	taskDone
	taskFailed
	taskCancelled
)

// queueTask is one item the user asked to copy: a file or a whole folder.
type queueTask struct {
	srcSide int
	name    string
	isDir   bool
	srcPath string
	dstPath string
	state   taskState
	job     *copyJob
	err     error // why the whole task stopped, if it did
	cancel  context.CancelFunc
	start   time.Time
	end     time.Time
}

func (t *queueTask) finished() bool { return t.state >= taskDone }

type conflictReq struct {
	task  *queueTask
	c     conflict
	reply chan conflictAction
}

// --- Model ---

const (
	focusLeft = iota
	focusRight
	focusQueue
)

type dirLoadedMsg struct {
	side    int
	dir     string
	entries []fileEntry
	err     error
	focus   string // entry to put the cursor on
}

type opDoneMsg struct {
	side   int
	status string
	focus  string
	err    error
}

type tickMsg struct{}

type taskDoneMsg struct {
	task *queueTask
	err  error
}

type conflictMsg struct{ req *conflictReq }

type filePrompt struct {
	question string
	isInput  bool
	input    textinput.Model
	onYes    func(value string) tea.Cmd
}

// overlay is a full-screen scrollable text page (help, transfer details).
type overlay struct {
	title  string
	lines  []string
	offset int
}

type filesModel struct {
	panes      [2]*pane
	active     int // focusLeft, focusRight or focusQueue
	sudo       bool
	showHidden bool
	width      int
	height     int
	status     string
	statusErr  bool
	prompt     *filePrompt
	overlay    *overlay

	queue   []*queueTask
	qCursor int
	qOffset int
	running *queueTask

	conflicts   chan *conflictReq
	pending     *conflictReq   // conflict waiting for an answer
	conflictAll conflictAction // "for all" answer, until the queue empties
}

func (m *filesModel) Init() tea.Cmd {
	return tea.Batch(m.load(0, m.panes[0].dir, ""), m.load(1, m.panes[1].dir, ""), m.listenConflicts())
}

func (m *filesModel) listenConflicts() tea.Cmd {
	ch := m.conflicts
	return func() tea.Msg { return conflictMsg{<-ch} }
}

func (m *filesModel) load(side int, dir, focus string) tea.Cmd {
	p := m.panes[side]
	p.loading = true
	fsys, hidden := p.fs, m.showHidden
	return func() tea.Msg {
		entries, err := listDir(fsys, dir, hidden)
		return dirLoadedMsg{side: side, dir: dir, entries: entries, err: err, focus: focus}
	}
}

func (m *filesModel) reloadAll() tea.Cmd {
	return tea.Batch(m.load(0, m.panes[0].dir, ""), m.load(1, m.panes[1].dir, ""))
}

func (m *filesModel) setStatus(s string) { m.status, m.statusErr = s, false }
func (m *filesModel) setError(err error) { m.status, m.statusErr = err.Error(), true }

func (m *filesModel) ask(question string, onYes func(string) tea.Cmd) {
	m.prompt = &filePrompt{question: question, onYes: onYes}
}

func (m *filesModel) askInput(question, value string, onYes func(string) tea.Cmd) {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetValue(value)
	ti.CursorEnd()
	ti.Focus()
	m.prompt = &filePrompt{question: question, isInput: true, input: ti, onYes: onYes}
}

// Layout: two panes, the transfers panel under them, then two footer lines.
func (m *filesModel) queueRows() int {
	if m.height < 24 {
		return 2
	}
	return 5
}

func (m *filesModel) listHeight() int {
	// pane border + header (3), transfers panel border + header + rows, footer (2)
	return max(1, m.height-3-(3+m.queueRows())-2)
}

func (m *filesModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case dirLoadedMsg:
		m.applyDir(msg)
	case opDoneMsg:
		if msg.err != nil {
			m.setError(msg.err)
		} else {
			m.setStatus(msg.status)
		}
		cmd = m.load(msg.side, m.panes[msg.side].dir, msg.focus)
	case tickMsg:
		if m.running != nil {
			cmd = tick()
		}
	case taskDoneMsg:
		cmd = m.finishTask(msg.task, msg.err)
	case conflictMsg:
		if m.conflictAll != conflictAsk {
			msg.req.reply <- m.conflictAll
		} else {
			m.pending = msg.req
		}
		cmd = m.listenConflicts()
	case tea.KeyMsg:
		switch {
		case m.overlay != nil:
			m.overlayKey(msg)
		case m.pending != nil:
			m.conflictKey(msg)
		case m.prompt != nil:
			cmd = m.promptKey(msg)
		default:
			cmd = m.handleKey(msg)
		}
	}
	for _, p := range m.panes {
		p.offset = scrollWindow(p.cursor, p.offset, m.listHeight(), len(p.entries))
	}
	m.qCursor = max(0, min(m.qCursor, len(m.queue)-1))
	m.qOffset = scrollWindow(m.qCursor, m.qOffset, m.queueRows(), len(m.queue))
	return m, cmd
}

func (m *filesModel) applyDir(msg dirLoadedMsg) {
	p := m.panes[msg.side]
	p.loading = false
	if msg.err != nil {
		m.setError(msg.err)
		return
	}
	focus := msg.focus
	if msg.dir == p.dir {
		// Refresh: keep the cursor on the same entry and keep marks that still exist.
		if e := p.selected(); focus == "" && e != nil {
			focus = e.name
		}
		for name := range p.marked {
			if !containsEntry(msg.entries, name) {
				delete(p.marked, name)
			}
		}
	} else {
		p.marked = map[string]bool{}
		p.offset = 0
	}
	p.dir, p.entries, p.cursor = msg.dir, msg.entries, 0
	for i, e := range p.entries {
		if e.name == focus {
			p.cursor = i
			break
		}
	}
}

func containsEntry(entries []fileEntry, name string) bool {
	for _, e := range entries {
		if e.name == name {
			return true
		}
	}
	return false
}

func (m *filesModel) overlayKey(msg tea.KeyMsg) {
	o := m.overlay
	page := max(1, m.height-4)
	switch msg.String() {
	case "esc", "q", "?", "enter", "ctrl+c":
		m.overlay = nil
		return
	case "up", "k":
		o.offset--
	case "down", "j":
		o.offset++
	case "pgup":
		o.offset -= page
	case "pgdown", " ":
		o.offset += page
	case "home":
		o.offset = 0
	case "end":
		o.offset = len(o.lines)
	}
	o.offset = max(0, min(o.offset, len(o.lines)-page))
}

func (m *filesModel) conflictKey(msg tea.KeyMsg) {
	actions := map[string]conflictAction{
		"o": conflictOverwrite, "n": conflictNewer, "s": conflictSkip, "r": conflictRename,
	}
	k := msg.String()
	if a, ok := actions[k]; ok {
		m.answer(a, false)
		return
	}
	if a, ok := actions[strings.ToLower(k)]; ok && k != strings.ToLower(k) {
		m.answer(a, true)
		return
	}
	if k == "esc" || k == "ctrl+c" {
		m.pending.task.cancel()
		m.answer(conflictSkip, false)
		m.setStatus("Transfer cancelled")
	}
}

func (m *filesModel) answer(a conflictAction, forAll bool) {
	if forAll {
		m.conflictAll = a
	}
	m.pending.reply <- a
	m.pending = nil
}

func (m *filesModel) promptKey(msg tea.KeyMsg) tea.Cmd {
	p := m.prompt
	switch msg.String() {
	case "esc", "ctrl+c":
		m.prompt = nil
		return nil
	}
	if !p.isInput {
		switch msg.String() {
		case "y", "Y":
			m.prompt = nil
			return p.onYes("")
		case "n", "N", "enter":
			m.prompt = nil
		}
		return nil
	}
	if msg.String() == "enter" {
		m.prompt = nil
		value := strings.TrimSpace(p.input.Value())
		if value == "" {
			return nil
		}
		return p.onYes(value)
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return cmd
}

func (m *filesModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c", "q":
		if m.running != nil || m.countState(taskQueued) > 0 {
			m.ask("Transfers are still running or queued. Cancel them and quit? (y/n)", func(string) tea.Cmd {
				m.cancelAll()
				return tea.Quit
			})
			return nil
		}
		return tea.Quit
	case "?", "f1":
		m.showHelp()
		return nil
	case "tab":
		m.active = (m.active + 1) % 3
		return nil
	case "shift+tab":
		m.active = (m.active + 2) % 3
		return nil
	case "esc":
		if m.running != nil {
			m.running.cancel()
			m.setStatus("Cancelling the current transfer...")
			return nil
		}
		if m.active != focusQueue {
			m.panes[m.active].marked = map[string]bool{}
		}
		return nil
	}
	if m.active == focusQueue {
		return m.queueKey(msg)
	}
	return m.paneKey(msg)
}

func (m *filesModel) paneKey(msg tea.KeyMsg) tea.Cmd {
	p := m.panes[m.active]
	side := m.active
	switch msg.String() {
	case "up", "k":
		p.move(-1)
	case "down", "j":
		p.move(1)
	case "pgup":
		p.move(-m.listHeight())
	case "pgdown":
		p.move(m.listHeight())
	case "home":
		p.cursor = 0
	case "end":
		p.cursor = len(p.entries) - 1
	case "enter", "right", "l":
		if e := p.selected(); e != nil && e.isDir {
			if e.up {
				return m.goUp(side)
			}
			return m.load(side, p.fs.Join(p.dir, e.name), "")
		}
	case "backspace", "left", "h":
		return m.goUp(side)
	case "~":
		if home, err := p.fs.Abs("~"); err == nil {
			return m.load(side, home, "")
		}
	case " ", "insert":
		if e := p.selected(); e != nil && !e.up {
			if p.marked[e.name] {
				delete(p.marked, e.name)
			} else {
				p.marked[e.name] = true
			}
		}
		p.move(1)
	case "a":
		m.toggleAll(p)
	case "c", "f5":
		return m.enqueue(side)
	case "d", "f8", "delete":
		m.confirmDelete(side)
	case "r", "f2":
		if e := p.selected(); e != nil && !e.up {
			name := e.name
			m.askInput("Rename to:", name, func(v string) tea.Cmd {
				if v == name {
					return nil
				}
				if p.has(v) {
					m.setError(fmt.Errorf("%q already exists", v))
					return nil
				}
				return m.fsOp(side, "Renamed "+name+" to "+v, v, func() error {
					return p.fs.Rename(p.fs.Join(p.dir, name), p.fs.Join(p.dir, v))
				})
			})
		}
	case "m", "f7":
		m.askInput("New folder name:", "", func(v string) tea.Cmd {
			return m.fsOp(side, "Created "+v, v, func() error {
				return p.fs.MkdirAll(p.fs.Join(p.dir, v))
			})
		})
	case "g":
		m.askInput("Go to folder:", p.dir, func(v string) tea.Cmd {
			dir, err := p.fs.Abs(v)
			if err != nil {
				m.setError(err)
				return nil
			}
			return m.load(side, dir, "")
		})
	case ".":
		m.showHidden = !m.showHidden
		return m.reloadAll()
	case "ctrl+r":
		return m.reloadAll()
	}
	return nil
}

func (m *filesModel) queueKey(msg tea.KeyMsg) tea.Cmd {
	var t *queueTask
	if m.qCursor < len(m.queue) {
		t = m.queue[m.qCursor]
	}
	switch msg.String() {
	case "up", "k":
		m.qCursor--
	case "down", "j":
		m.qCursor++
	case "home":
		m.qCursor = 0
	case "end":
		m.qCursor = len(m.queue) - 1
	case "enter":
		if t != nil {
			m.showDetails(t)
		}
	case "d", "delete", "x":
		switch {
		case t == nil:
		case t.state == taskRunning:
			t.cancel()
			m.setStatus("Cancelling " + t.name + "...")
		default:
			m.queue = append(m.queue[:m.qCursor], m.queue[m.qCursor+1:]...)
		}
	case "r":
		if t != nil && (t.state == taskFailed || t.state == taskCancelled) {
			t.state, t.job, t.err = taskQueued, nil, nil
			return m.startNext()
		}
	case "c":
		kept := m.queue[:0]
		for _, q := range m.queue {
			if !q.finished() {
				kept = append(kept, q)
			}
		}
		m.queue = kept
	}
	return nil
}

func (m *filesModel) goUp(side int) tea.Cmd {
	p := m.panes[side]
	parent := p.fs.Dir(p.dir)
	if parent == p.dir {
		return nil
	}
	return m.load(side, parent, p.fs.Base(p.dir))
}

func (m *filesModel) toggleAll(p *pane) {
	all := true
	for _, e := range p.entries {
		if !e.up && !p.marked[e.name] {
			all = false
			break
		}
	}
	p.marked = map[string]bool{}
	if all {
		return
	}
	for _, e := range p.entries {
		if !e.up {
			p.marked[e.name] = true
		}
	}
}

// fsOp runs a file operation off the UI goroutine, then reloads the pane.
func (m *filesModel) fsOp(side int, okStatus, focus string, fn func() error) tea.Cmd {
	m.setStatus("Working...")
	return func() tea.Msg {
		return opDoneMsg{side: side, status: okStatus, focus: focus, err: fn()}
	}
}

func describe(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("%q", names[0])
	}
	return fmt.Sprintf("%d items", len(names))
}

func (m *filesModel) confirmDelete(side int) {
	p := m.panes[side]
	names := entryNames(p.targets())
	if len(names) == 0 {
		return
	}
	m.ask(fmt.Sprintf("Delete %s from %s? This cannot be undone. (y/n)", describe(names), paneLabel(p.fs)), func(string) tea.Cmd {
		return m.fsOp(side, "Deleted "+describe(names), "", func() error {
			for _, n := range names {
				if err := p.fs.RemoveAll(p.fs.Join(p.dir, n)); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// --- Running the queue ---

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// enqueue adds the marked entries (or the one under the cursor) of a pane
// to the queue, to be copied into the folder open in the other pane.
func (m *filesModel) enqueue(srcSide int) tea.Cmd {
	src, dst := m.panes[srcSide], m.panes[1-srcSide]
	entries := src.targets()
	if len(entries) == 0 {
		m.setStatus("Nothing to copy: move the cursor onto a file or folder first")
		return nil
	}
	for _, e := range entries {
		m.queue = append(m.queue, &queueTask{
			srcSide: srcSide,
			name:    e.name,
			isDir:   e.isDir,
			srcPath: src.fs.Join(src.dir, e.name),
			dstPath: dst.fs.Join(dst.dir, e.name),
		})
	}
	src.marked = map[string]bool{}
	m.setStatus(fmt.Sprintf("Added %s to the queue → %s: %s", describe(entryNames(entries)), paneLabel(dst.fs), dst.dir))
	return m.startNext()
}

func (m *filesModel) countState(s taskState) int {
	n := 0
	for _, t := range m.queue {
		if t.state == s {
			n++
		}
	}
	return n
}

func (m *filesModel) startNext() tea.Cmd {
	if m.running != nil {
		return nil
	}
	for _, t := range m.queue {
		if t.state == taskQueued {
			return m.startTask(t)
		}
	}
	m.conflictAll = conflictAsk
	return nil
}

func (m *filesModel) startTask(t *queueTask) tea.Cmd {
	src, dst := m.panes[t.srcSide].fs, m.panes[1-t.srcSide].fs
	ctx, cancel := context.WithCancel(context.Background())
	conflicts := m.conflicts
	ask := func(c conflict) conflictAction {
		req := &conflictReq{task: t, c: c, reply: make(chan conflictAction, 1)}
		select {
		case conflicts <- req:
		case <-ctx.Done():
			return conflictSkip
		}
		select {
		case a := <-req.reply:
			return a
		case <-ctx.Done():
			return conflictSkip
		}
	}
	job := newCopyJob(src, dst, copyOptions{recursive: true, preserveTimes: true, onConflict: conflictAsk, ask: ask})
	t.state, t.job, t.cancel, t.start, t.err = taskRunning, job, cancel, time.Now(), nil
	m.running = t

	done := make(chan error, 1)
	go func() {
		if err := job.add(t.srcPath, t.dstPath); err != nil {
			done <- err
			return
		}
		done <- job.run(ctx)
	}()
	return tea.Batch(func() tea.Msg { return taskDoneMsg{task: t, err: <-done} }, tick())
}

func (m *filesModel) finishTask(t *queueTask, err error) tea.Cmd {
	t.end = time.Now()
	t.cancel()
	if m.running == t {
		m.running = nil
	}
	if m.pending != nil && m.pending.task == t {
		m.pending = nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		t.state = taskCancelled
	case err != nil:
		t.state, t.err = taskFailed, err
	case len(t.job.Failures()) > 0:
		t.state = taskFailed
	default:
		t.state = taskDone
	}

	cmds := []tea.Cmd{m.load(1-t.srcSide, m.panes[1-t.srcSide].dir, "")}
	if next := m.startNext(); next != nil {
		cmds = append(cmds, next)
	} else if m.running == nil {
		done, failed := m.countState(taskDone), m.countState(taskFailed)
		if failed > 0 {
			m.setError(fmt.Errorf("transfers finished: %d done, %d failed (tab to the Transfers panel, enter shows why)", done, failed))
		} else {
			m.setStatus(fmt.Sprintf("All transfers finished (%d done)", done))
		}
	}
	return tea.Batch(cmds...)
}

func (m *filesModel) cancelAll() {
	for _, t := range m.queue {
		if t.state == taskQueued {
			t.state = taskCancelled
		}
	}
	if m.running != nil {
		m.running.cancel()
	}
}

// --- Help and details pages ---

func (m *filesModel) showHelp() {
	remote := paneLabel(m.panes[1].fs)
	lines := []string{
		"LEFT pane is this computer, RIGHT pane is " + remote + ".",
		"The active pane has the purple border. tab moves between left, right and the Transfers panel.",
		"",
		"HOW TO COPY",
		"  1. In one pane, open the folder that has the files     (enter opens a folder, backspace goes back)",
		"  2. In the other pane, open the folder to copy them into",
		"  3. Back in the first pane, put the cursor on a file or folder and press c",
		"",
		"  To copy several at once, mark them first: space marks/unmarks the item under",
		"  the cursor (marked items show a * and turn yellow), a marks everything.",
		"  c then copies all marked items. Without marks, c copies just the cursor item.",
		"",
		"  A WHOLE FOLDER: put the cursor on the folder and press c. The folder and",
		"  everything inside it is copied into the folder open on the other side.",
		"  ONLY WHAT IS INSIDE A FOLDER: open the folder (enter), press a to mark",
		"  everything, then c. The contents land directly in the other side's folder.",
		"",
		"  Each copy goes into the Transfers panel at the bottom and runs in order;",
		"  you can keep browsing and add more while it works.",
		"",
		"WHEN A FILE ALREADY EXISTS you are asked what to do:",
		"  o  overwrite          n  overwrite only if the new one is newer",
		"  s  skip               r  keep both (the new copy gets a name like file (1).txt)",
		"  O N S R (with shift)  same, and use that answer for the rest of the queue",
		"  esc                   cancel that transfer",
		"  Files are written under a temporary name and swapped in at the end, so a",
		"  cancelled or broken copy never damages the file that was already there.",
		"",
		"KEYS IN THE FILE PANES",
		"  ↑ ↓  j k         move                    pgup pgdown home end   jump",
		"  enter → l        open folder             backspace ← h          parent folder",
		"  space            mark / unmark           a                      mark all / none",
		"  c  F5            copy to the other pane  esc                    clear marks",
		"  d  F8  delete    delete                  r  F2                  rename",
		"  m  F7            new folder              g                      go to a folder by path",
		"  ~                home folder             .                      show / hide hidden files",
		"  ctrl+r           refresh",
		"",
		"KEYS IN THE TRANSFERS PANEL (tab to it)",
		"  ↑ ↓              select                  enter                  details and errors",
		"  d                remove, or cancel if running",
		"  r                retry a failed or cancelled transfer",
		"  c                clear finished transfers",
		"  esc              cancel the running transfer (works anywhere)",
		"",
		"  ?  F1            this help               q                      quit",
	}
	if m.sudo {
		lines = append([]string{"Connected with --sudo: everything on " + remote + " is done as root.", ""}, lines...)
	}
	m.overlay = &overlay{title: "Help: copying files between this computer and " + remote, lines: lines}
}

func (m *filesModel) showDetails(t *queueTask) {
	src, dst := m.panes[t.srcSide].fs, m.panes[1-t.srcSide].fs
	lines := []string{
		"From:   " + paneLabel(src) + ": " + t.srcPath,
		"To:     " + paneLabel(dst) + ": " + t.dstPath,
		"Status: " + m.taskStatus(t, 200),
	}
	if t.err != nil {
		lines = append(lines, "", "Stopped because: "+t.err.Error())
	}
	if t.job != nil {
		p := t.job.prog
		lines = append(lines, "",
			fmt.Sprintf("Copied:  %d file(s), %s", p.files.Load(), formatSize(p.done.Load())),
			fmt.Sprintf("Skipped: %d existing file(s) left as they were", p.kept.Load()),
			fmt.Sprintf("Renamed: %d copy(ies) saved under a new name (keep both)", p.renamed.Load()))
		if fails := t.job.Failures(); len(fails) > 0 {
			lines = append(lines, "", fmt.Sprintf("FAILED (%d):", len(fails)))
			for _, f := range fails {
				lines = append(lines, "  "+f.path, "      "+f.err.Error())
			}
		}
		if skipped := t.job.Skipped(); len(skipped) > 0 {
			lines = append(lines, "", "NOT COPIED (links to folders, broken links, devices or sockets):")
			for _, s := range skipped {
				lines = append(lines, "  "+s)
			}
		}
	}
	m.overlay = &overlay{title: "Transfer: " + t.name, lines: lines}
}

// --- View ---

var (
	paneStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	activePane   = paneStyle.BorderForeground(lipgloss.Color("#7D56F4"))
	headerStyle  = lipgloss.NewStyle().Bold(true)
	activeHeader = headerStyle.Foreground(lipgloss.Color("#7D56F4"))
	cursorStyle  = lipgloss.NewStyle().Background(lipgloss.Color("#7D56F4")).Foreground(lipgloss.Color("#FFFFFF"))
	idleCursor   = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	dirStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FAFFF")).Bold(true)
	markStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD75F")).Bold(true)
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F5F"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#87D787"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	runStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#AF87FF"))
)

func (m *filesModel) View() string {
	if m.width == 0 {
		return ""
	}
	if m.overlay != nil {
		return m.renderOverlay()
	}
	left := m.width / 2
	panes := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderPane(0, left),
		m.renderPane(1, m.width-left))
	return panes + "\n" + m.renderQueue() + "\n" + m.footer()
}

// keyHelp renders "key desc" pairs, dropping the ones that don't fit.
func keyHelp(width int, pairs ...[2]string) string {
	var b strings.Builder
	used := 0
	for _, p := range pairs {
		w := lipgloss.Width(p[0]) + 1 + lipgloss.Width(p[1]) + 3
		if used+w > width {
			break
		}
		b.WriteString(keyStyle.Render(p[0]) + " " + dimStyle.Render(p[1]) + "   ")
		used += w
	}
	return b.String()
}

func shortPath(p string, width int) string {
	r := []rune(p)
	if len(r) <= width || width < 2 {
		return p
	}
	return "…" + string(r[len(r)-width+1:])
}

func (m *filesModel) footer() string {
	w := m.width
	if c := m.pending; c != nil {
		dst := m.panes[1-c.task.srcSide].fs
		newer := ""
		if newerThan(c.c.srcInfo.ModTime(), c.c.dstInfo.ModTime()) {
			newer = " (newer)"
		}
		q := fmt.Sprintf("%q already exists on %s.  Existing: %s, %s   New: %s, %s%s",
			dst.Base(c.c.dst), paneLabel(dst),
			formatSize(c.c.dstInfo.Size()), shortTime(c.c.dstInfo.ModTime()),
			formatSize(c.c.srcInfo.Size()), shortTime(c.c.srcInfo.ModTime()), newer)
		return markStyle.Render(ansi.Truncate(q, w, "…")) + "\n" +
			keyHelp(w, [2]string{"o", "overwrite"}, [2]string{"n", "only if newer"}, [2]string{"s", "skip"},
				[2]string{"r", "keep both"}, [2]string{"O N S R", "= same for all"}, [2]string{"esc", "cancel transfer"})
	}

	var status string
	switch {
	case m.prompt != nil && m.prompt.isInput:
		status = ansi.Truncate(markStyle.Render(m.prompt.question)+" "+m.prompt.input.View(), w, "")
	case m.prompt != nil:
		status = markStyle.Render(ansi.Truncate(m.prompt.question, w, "…"))
	case m.statusErr:
		status = errStyle.Render(ansi.Truncate("Error: "+m.status, w, "…"))
	default:
		status = okStyle.Render(ansi.Truncate(m.status, w, "…"))
	}
	return status + "\n" + m.contextHelp()
}

// contextHelp is the bottom line: the keys that matter where the focus is.
func (m *filesModel) contextHelp() string {
	w := m.width
	if m.prompt != nil {
		if m.prompt.isInput {
			return keyHelp(w, [2]string{"enter", "ok"}, [2]string{"esc", "cancel"})
		}
		return keyHelp(w, [2]string{"y", "yes"}, [2]string{"n", "no"})
	}
	if m.active == focusQueue {
		return keyHelp(w, [2]string{"enter", "details"}, [2]string{"d", "remove/cancel"}, [2]string{"r", "retry"},
			[2]string{"c", "clear finished"}, [2]string{"tab", "back to files"}, [2]string{"?", "help"}, [2]string{"q", "quit"})
	}
	src, dst := m.panes[m.active], m.panes[1-m.active]
	what := ""
	if n := len(src.marked); n > 0 {
		what = fmt.Sprintf("%d marked", n)
	} else if e := src.selected(); e != nil && !e.up {
		what = ansi.Truncate(e.name, 24, "…")
	}
	copyDesc := "copy to " + paneLabel(dst.fs)
	if what != "" {
		copyDesc = fmt.Sprintf("copy %s → %s: %s", what, paneLabel(dst.fs), shortPath(dst.dir, 28))
	}
	return keyHelp(w, [2]string{"c", copyDesc}, [2]string{"space", "mark"}, [2]string{"enter", "open"},
		[2]string{"⌫", "back"}, [2]string{"tab", "switch"}, [2]string{"d", "delete"}, [2]string{"?", "help"}, [2]string{"q", "quit"})
}

func progressBar(pct, width int) string {
	n := max(0, min(width, pct*width/100))
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

func (m *filesModel) renderPane(side, width int) string {
	p := m.panes[side]
	active := side == m.active
	inner := max(10, width-2)
	height := m.listHeight()

	label := strings.ToUpper(paneLabel(p.fs))
	if p.fs.Remote() && m.sudo {
		label += " (sudo)"
	}
	label += ": "
	suffix := ""
	if n := len(p.marked); n > 0 {
		suffix = fmt.Sprintf("  [%d marked]", n)
	}
	dir := p.dir
	if p.loading {
		dir += " …"
	}
	// Keep the label and the end of the path, which is the part that matters.
	dir = shortPath(dir, inner-lipgloss.Width(label)-lipgloss.Width(suffix))
	hs := headerStyle
	if active {
		hs = activeHeader
	}
	lines := []string{hs.Render(ansi.Truncate(label+dir, inner-lipgloss.Width(suffix), "…")) + markStyle.Render(suffix)}

	empty := true
	for _, e := range p.entries {
		if !e.up {
			empty = false
			break
		}
	}
	for i := p.offset; i < p.offset+height; i++ {
		if i >= len(p.entries) {
			if empty && !p.loading && i == len(p.entries) {
				lines = append(lines, dimStyle.Render("  (empty folder)"))
			} else {
				lines = append(lines, "")
			}
			continue
		}
		e := p.entries[i]
		row := ansi.Truncate(formatRow(e, p.marked[e.name], inner), inner, "…")
		var style lipgloss.Style
		switch {
		case i == p.cursor && active:
			style = cursorStyle
		case i == p.cursor:
			style = idleCursor
		case p.marked[e.name]:
			style = markStyle
		case e.isDir:
			style = dirStyle
		default:
			style = lipgloss.NewStyle()
		}
		lines = append(lines, style.Width(inner).Render(row))
	}

	ps := paneStyle
	if active {
		ps = activePane
	}
	return ps.Width(inner).Render(strings.Join(lines, "\n"))
}

// formatRow lays out "* name     size  date" to fit width.
func formatRow(e fileEntry, marked bool, width int) string {
	mark := "  "
	if marked {
		mark = "* "
	}
	name := e.name
	switch {
	case e.up:
		name = ".. (parent folder)"
	case e.isDir:
		name += "/"
	case e.link:
		name += "@"
	}

	size, date := "", ""
	if !e.up && e.info != nil {
		if !e.isDir {
			size = formatSize(e.info.Size())
		}
		date = shortTime(e.info.ModTime())
	}
	meta := fmt.Sprintf(" %9s  %12s", size, date)
	if width < 44 {
		meta = fmt.Sprintf(" %9s", size)
	}
	nameWidth := width - len(mark) - len(meta)
	if nameWidth < 4 {
		return mark + name
	}
	name = ansi.Truncate(name, nameWidth, "…")
	return mark + name + strings.Repeat(" ", nameWidth-ansi.StringWidth(name)) + meta
}

// shortTime formats like ls: time for recent files, year for old ones.
func shortTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if time.Since(t) > 180*24*time.Hour || time.Until(t) > 24*time.Hour {
		return t.Format("Jan _2  2006")
	}
	return t.Format("Jan _2 15:04")
}

// taskStatus is the right-hand side of a queue row.
func (m *filesModel) taskStatus(t *queueTask, width int) string {
	switch t.state {
	case taskQueued:
		return "waiting"
	case taskCancelled:
		return "cancelled"
	case taskRunning:
		p := t.job.prog
		if cur := p.Current(); strings.HasPrefix(cur, "scanning ") {
			return "scanning..."
		}
		if m.pending != nil && m.pending.task == t {
			return "waiting for your answer below"
		}
		d, total := p.done.Load(), p.total.Load()
		pct := percent(d, total)
		s := fmt.Sprintf("%3d%% %s / %s  %s", pct, formatSize(d), formatSize(total), formatRate(d, time.Since(t.start)))
		if n := p.fileCount.Load(); n > 1 {
			s += fmt.Sprintf("  [%d/%d files]", p.files.Load(), n)
		}
		if width > 70 {
			s = progressBar(pct, 14) + " " + s
		}
		return s
	}

	p := t.job.prog
	s := fmt.Sprintf("%d file(s), %s in %s", p.files.Load(), formatSize(p.done.Load()), t.end.Sub(t.start).Round(100*time.Millisecond))
	if n := p.kept.Load(); n > 0 {
		s += fmt.Sprintf(", %d skipped", n)
	}
	if n := p.renamed.Load(); n > 0 {
		s += fmt.Sprintf(", %d renamed", n)
	}
	if t.state == taskFailed {
		if t.err != nil {
			return "failed: " + t.err.Error()
		}
		return fmt.Sprintf("%d failed, %s (enter = why)", len(t.job.Failures()), s)
	}
	return "done: " + s
}

func (m *filesModel) renderQueue() string {
	inner := max(10, m.width-2)
	active := m.active == focusQueue
	rows := m.queueRows()

	title := "TRANSFERS"
	var counts []string
	for _, c := range []struct {
		state taskState
		name  string
	}{{taskRunning, "running"}, {taskQueued, "waiting"}, {taskDone, "done"}, {taskFailed, "failed"}, {taskCancelled, "cancelled"}} {
		if n := m.countState(c.state); n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, c.name))
		}
	}
	if len(counts) > 0 {
		title += "  " + strings.Join(counts, " · ")
	}
	hs := headerStyle
	if active {
		hs = activeHeader
	}
	lines := []string{hs.Render(ansi.Truncate(title, inner, "…"))}

	if len(m.queue) == 0 {
		hint := []string{
			"Nothing copied yet. Put the cursor on a file or folder (or mark several with space) and press c:",
			"it is copied into the folder open in the other pane, and its progress shows here.",
		}
		for i := 0; i < rows; i++ {
			line := ""
			if i < len(hint) {
				line = dimStyle.Render(ansi.Truncate(hint[i], inner, "…"))
			}
			lines = append(lines, line)
		}
	}
	for i := m.qOffset; len(m.queue) > 0 && i < m.qOffset+rows; i++ {
		if i >= len(m.queue) {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, m.queueRow(m.queue[i], i == m.qCursor && active, inner))
	}

	ps := paneStyle
	if active {
		ps = activePane
	}
	return ps.Width(inner).Render(strings.Join(lines, "\n"))
}

func (m *filesModel) queueRow(t *queueTask, selected bool, width int) string {
	icon, style := "·", dimStyle
	switch t.state {
	case taskRunning:
		icon, style = "▶", runStyle
	case taskDone:
		icon, style = "✓", okStyle
	case taskFailed:
		icon, style = "✗", errStyle
	case taskCancelled:
		icon = "–"
	}
	dst := m.panes[1-t.srcSide].fs
	arrow := "↑"
	if !dst.Remote() {
		arrow = "↓"
	}
	name := t.name
	if t.isDir {
		name += "/"
	}
	status := m.taskStatus(t, width)
	statusWidth := min(lipgloss.Width(status), width*3/5)
	status = ansi.Truncate(status, statusWidth, "…")

	leftWidth := width - statusWidth - 2
	left := fmt.Sprintf("%s %s %s → %s: %s", icon, arrow, name, paneLabel(dst), dst.Dir(t.dstPath))
	left = ansi.Truncate(left, leftWidth, "…")
	row := left + strings.Repeat(" ", max(1, width-lipgloss.Width(left)-statusWidth)) + status
	if selected {
		return cursorStyle.Width(width).Render(row)
	}
	return style.Width(width).Render(row)
}

func (m *filesModel) renderOverlay() string {
	o := m.overlay
	inner := max(10, m.width-2)
	page := max(1, m.height-4) // border (2) + title (1) + hint (1)
	lines := []string{activeHeader.Render(ansi.Truncate(o.title, inner, "…"))}
	for i := o.offset; i < o.offset+page; i++ {
		line := ""
		if i < len(o.lines) {
			line = ansi.Truncate(o.lines[i], inner, "…")
		}
		lines = append(lines, line)
	}
	hint := "esc close"
	if len(o.lines) > page {
		hint = "↑ ↓ pgup pgdown scroll   " + hint
	}
	lines = append(lines, dimStyle.Render(hint))
	return activePane.Width(inner).Render(strings.Join(lines, "\n"))
}
