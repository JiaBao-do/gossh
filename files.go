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

// gossh files: a two-pane (local | remote) file browser over SFTP.

// filesCommand implements "gossh files [--sudo] [alias [remote-dir]]".
func filesCommand(configPath string, args []string) {
	sudo := false
	var rest []string
	for _, a := range args {
		if a == "--sudo" {
			sudo = true
			continue
		}
		rest = append(rest, a)
	}
	host, remoteDir := "", ""
	if len(rest) > 0 {
		host = rest[0]
	}
	if len(rest) > 1 {
		remoteDir = rest[1]
	}
	if host == "" {
		var ok bool
		if host, ok = chooseHost(configPath, "Browse files on which host?"); !ok {
			return
		}
	}
	runFiles(host, remoteDir, sudo)
}

func runFiles(host, remoteDir string, sudo bool) {
	fmt.Printf("Connecting to %s...\n", host)
	sess, err := connectSFTP(host, connectOptions{sudo: sudo})
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
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

	m := &filesModel{
		panes: [2]*pane{
			{fs: localFS{}, dir: localDir, marked: map[string]bool{}},
			{fs: sess.fs, dir: remoteDir, marked: map[string]bool{}},
		},
		active: 1,
		sudo:   sudo,
	}
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Println("Error running file browser:", err)
		os.Exit(1)
	}
	if m.job != nil {
		m.job.cancel()
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

// targets returns the marked names, or the entry under the cursor.
func (p *pane) targets() []string {
	var names []string
	for _, e := range p.entries {
		if p.marked[e.name] {
			names = append(names, e.name)
		}
	}
	if len(names) > 0 {
		return names
	}
	if e := p.selected(); e != nil && !e.up {
		return []string{e.name}
	}
	return nil
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

func (p *pane) scrollTo(height int) {
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+height {
		p.offset = p.cursor - height + 1
	}
	p.offset = max(0, min(p.offset, len(p.entries)-height))
}

// --- Model ---

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

type jobTickMsg struct{}

type jobDoneMsg struct{ err error }

type filePrompt struct {
	question string
	isInput  bool
	input    textinput.Model
	onYes    func(value string) tea.Cmd
}

type transferJob struct {
	job     *copyJob
	cancel  context.CancelFunc
	start   time.Time
	dstSide int
	desc    string
}

type filesModel struct {
	panes      [2]*pane
	active     int
	sudo       bool
	showHidden bool
	width      int
	height     int
	status     string
	statusErr  bool
	prompt     *filePrompt
	job        *transferJob
}

func (m *filesModel) Init() tea.Cmd {
	return tea.Batch(m.load(0, m.panes[0].dir, ""), m.load(1, m.panes[1].dir, ""))
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

// listHeight is the number of entry rows that fit in a pane.
func (m *filesModel) listHeight() int {
	// border (2) + pane header (1) + footer (2)
	return max(1, m.height-5)
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
	case jobTickMsg:
		if m.job != nil {
			cmd = jobTick()
		}
	case jobDoneMsg:
		cmd = m.finishJob(msg.err)
	case tea.KeyMsg:
		if m.prompt != nil {
			cmd = m.updatePrompt(msg)
		} else {
			cmd = m.handleKey(msg)
		}
	}
	for _, p := range m.panes {
		p.scrollTo(m.listHeight())
	}
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

func (m *filesModel) updatePrompt(msg tea.KeyMsg) tea.Cmd {
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
	p := m.panes[m.active]
	side := m.active
	switch msg.String() {
	case "ctrl+c", "q":
		if m.job != nil {
			m.ask("A transfer is running. Cancel it and quit? (y/n)", func(string) tea.Cmd {
				if m.job != nil {
					m.job.cancel()
				}
				return tea.Quit
			})
			return nil
		}
		return tea.Quit
	case "tab", "shift+tab":
		m.active = 1 - m.active
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
	case "esc":
		if m.job != nil {
			m.job.cancel()
			m.setStatus("Cancelling transfer...")
			return nil
		}
		p.marked = map[string]bool{}
	case "c", "f5":
		return m.copySelection()
	case "d", "f8", "delete":
		m.confirmDelete(side)
	case "r", "f2":
		if e := p.selected(); e != nil && !e.up {
			name := e.name
			m.askInput("Rename to:", name, func(v string) tea.Cmd {
				if v == name {
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
		m.askInput("Go to path:", p.dir, func(v string) tea.Cmd {
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
	names := p.targets()
	if len(names) == 0 {
		return
	}
	m.ask(fmt.Sprintf("Delete %s from %s? This cannot be undone. (y/n)", describe(names), p.fs.Label()), func(string) tea.Cmd {
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

func (m *filesModel) copySelection() tea.Cmd {
	if m.job != nil {
		m.setStatus("A transfer is already running (esc cancels it)")
		return nil
	}
	srcSide := m.active
	src, dst := m.panes[srcSide], m.panes[1-srcSide]
	names := src.targets()
	if len(names) == 0 {
		return nil
	}
	var existing []string
	for _, n := range names {
		if dst.has(n) {
			existing = append(existing, n)
		}
	}
	start := func(string) tea.Cmd { return m.startTransfer(srcSide, names) }
	if len(existing) > 0 {
		m.ask(fmt.Sprintf("%s already exists on %s. Overwrite? (y/n)", describe(existing), dst.fs.Label()), start)
		return nil
	}
	return start("")
}

func jobTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return jobTickMsg{} })
}

func (m *filesModel) startTransfer(srcSide int, names []string) tea.Cmd {
	src, dst := m.panes[srcSide], m.panes[1-srcSide]
	job := newCopyJob(src.fs, dst.fs, copyOptions{recursive: true, preserveTimes: true})
	ctx, cancel := context.WithCancel(context.Background())
	verb := "Uploading"
	if src.fs.Remote() {
		verb = "Downloading"
	}
	m.job = &transferJob{
		job:     job,
		cancel:  cancel,
		start:   time.Now(),
		dstSide: 1 - srcSide,
		desc:    fmt.Sprintf("%s %s", verb, describe(names)),
	}
	src.marked = map[string]bool{}
	m.setStatus("")

	srcDir, dstDir := src.dir, dst.dir
	done := make(chan error, 1)
	go func() {
		for _, n := range names {
			if err := job.add(src.fs.Join(srcDir, n), dst.fs.Join(dstDir, n)); err != nil {
				done <- err
				return
			}
		}
		done <- job.run(ctx)
	}()
	return tea.Batch(func() tea.Msg { return jobDoneMsg{err: <-done} }, jobTick())
}

func (m *filesModel) finishJob(err error) tea.Cmd {
	j := m.job
	m.job = nil
	j.cancel()
	prog := j.job.prog
	switch {
	case errors.Is(err, context.Canceled):
		m.setStatus(fmt.Sprintf("Transfer cancelled after %d file(s)", prog.files.Load()))
	case err != nil:
		m.setError(err)
	default:
		elapsed := time.Since(j.start)
		s := fmt.Sprintf("%s done: %d file(s), %s in %s (%s)", j.desc, prog.files.Load(),
			formatSize(prog.done.Load()), elapsed.Round(100*time.Millisecond), formatRate(prog.done.Load(), elapsed))
		if n := len(j.job.skipped); n > 0 {
			s += fmt.Sprintf(", skipped %d special file(s)/dir link(s)", n)
		}
		m.setStatus(s)
	}
	return m.load(j.dstSide, m.panes[j.dstSide].dir, "")
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
	helpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

func (m *filesModel) View() string {
	if m.width == 0 {
		return ""
	}
	left := m.width / 2
	panes := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderPane(0, left),
		m.renderPane(1, m.width-left))
	return panes + "\n" + m.footer() + "\n" +
		helpStyle.Render(ansi.Truncate("tab switch  enter open  ⌫ up  space mark  a all  c copy  d delete  r rename  m mkdir  g goto  . hidden  esc cancel  q quit", m.width, "…"))
}

func (m *filesModel) footer() string {
	w := m.width
	switch {
	case m.prompt != nil && m.prompt.isInput:
		return ansi.Truncate(markStyle.Render(m.prompt.question)+" "+m.prompt.input.View(), w, "")
	case m.prompt != nil:
		return ansi.Truncate(markStyle.Render(m.prompt.question), w, "…")
	case m.job != nil:
		p := m.job.job.prog
		d, t := p.done.Load(), p.total.Load()
		pct := percent(d, t)
		line := fmt.Sprintf("%s %s %3d%%  %s / %s  %s  [%d/%d]  %s",
			m.job.desc, progressBar(pct, 20), pct, formatSize(d), formatSize(t),
			formatRate(d, time.Since(m.job.start)), p.files.Load(), p.fileCount.Load(), p.Current())
		return ansi.Truncate(line, w, "…")
	case m.statusErr:
		return errStyle.Render(ansi.Truncate("Error: "+m.status, w, "…"))
	}
	return okStyle.Render(ansi.Truncate(m.status, w, "…"))
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

	label := p.fs.Label()
	if p.fs.Remote() && m.sudo {
		label += " (sudo)"
	}
	label += ": "
	dir := p.dir
	if p.loading {
		dir += " …"
	}
	// Keep the label and the end of the path, which is the part that matters.
	if room := inner - lipgloss.Width(label); room > 1 && lipgloss.Width(dir) > room {
		runes := []rune(dir)
		dir = "…" + string(runes[len(runes)-room+1:])
	}
	header := ansi.Truncate(label+dir, inner, "…")
	hs := headerStyle
	if active {
		hs = activeHeader
	}
	lines := []string{hs.Render(header)}

	for i := p.offset; i < p.offset+height; i++ {
		if i >= len(p.entries) {
			lines = append(lines, "")
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
