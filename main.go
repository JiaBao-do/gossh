package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

// Implement list.Item interface for Bubble Tea
func (e SSHEntry) Title() string { return e.Alias }
func (e SSHEntry) Description() string {
	desc := e.HostName
	if e.User != "" {
		desc += fmt.Sprintf(" • %s", e.User)
	}
	if e.Port != "" {
		desc += fmt.Sprintf(" :%s", e.Port)
	}
	return desc
}
func (e SSHEntry) FilterValue() string { return e.Alias + e.HostName + e.User }

// --- Main ---

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("gossh %s\n", version)
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	}

	configPath, err := getConfigPath()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	args := os.Args[1:]
	mode := "interactive"
	if len(args) > 0 {
		mode = args[0]
	}

	switch mode {
	case "list":
		listHostsCLI(configPath)
	case "add":
		addHost(configPath)
	case "edit":
		alias := ""
		if len(args) > 1 {
			alias = args[1]
		}
		editHost(configPath, alias)
	case "delete":
		deleteHost(configPath)
	case "keys", "key":
		keysCommand(configPath, args[1:])
	case "forward", "fwd":
		forwardCommand(configPath, args[1:])
	case "connect":
		if len(args) < 2 {
			fmt.Println("Usage: gossh connect <alias|user@host> [ssh args...]")
			os.Exit(1)
		}
		runSSH(args[1:]...)
	case "interactive":
		runTUI(configPath)
	default:
		// Anything else is handed to ssh: "gossh prod-db", "gossh user@host -p 2222".
		runSSH(args...)
	}
}

func printHelp() {
	fmt.Println("gossh - SSH host manager")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  gossh                       Launch interactive TUI")
	fmt.Println("  gossh <alias> [args]        Connect to a saved host (any ssh args work)")
	fmt.Println("  gossh user@host [args]      Plain ssh passthrough")
	fmt.Println("  gossh connect <alias>       Connect (use if an alias clashes with a command)")
	fmt.Println("  gossh list                  List all saved hosts")
	fmt.Println("  gossh add                   Add a new host")
	fmt.Println("  gossh edit [alias]          Edit a host (select or specify by alias)")
	fmt.Println("  gossh delete                Delete a host")
	fmt.Println()
	fmt.Println("Port forwarding:")
	fmt.Println("  gossh forward               Guided port forward (pick host and ports)")
	fmt.Println("  gossh forward <alias> SPEC...")
	fmt.Println("      8080                    localhost:8080 -> remote localhost:8080")
	fmt.Println("      8080:80                 localhost:8080 -> remote localhost:80")
	fmt.Println("      5433:db.internal:5432   localhost:5433 -> db.internal:5432 (seen from remote)")
	fmt.Println("      R:9000:3000             remote port 9000 -> your localhost:3000")
	fmt.Println("      D:1080                  SOCKS proxy on localhost:1080 through the host")
	fmt.Println()
	fmt.Println("Keys (stored in ~/.ssh/keys):")
	fmt.Println("  gossh keys                  List managed keys")
	fmt.Println("  gossh keys import <path>    Copy a key into ~/.ssh/keys")
	fmt.Println("  gossh keys migrate          Copy/move keys that hosts reference elsewhere")
	fmt.Println()
	fmt.Println("  gossh version               Show version")
	fmt.Println("  gossh help                  Show this help")
	fmt.Println()
	fmt.Println("Existing hosts are never changed automatically. When you set an IdentityFile")
	fmt.Println("outside ~/.ssh/keys you choose to copy it, move it, or keep it where it is.")
	fmt.Println("~/.ssh/config is backed up to ~/.ssh/config.bak before every change.")
	fmt.Println()
	fmt.Println("Interactive TUI keys:")
	fmt.Println("  enter                       Connect to selected host")
	fmt.Println("  f                           Port forward through selected host")
	fmt.Println("  e                           Edit selected host")
	fmt.Println("  a                           Add a new host")
	fmt.Println("  /                           Filter hosts")
	fmt.Println("  ctrl+c  q                   Quit")
}

// runSSH runs the system ssh client with the given arguments and exits with its status.
func runSSH(args ...string) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Println("Error: ssh client not found on PATH.")
		if runtime.GOOS == "windows" {
			fmt.Println("Install it with (admin): Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0")
		}
		os.Exit(1)
	}

	// ssh handles Ctrl+C itself (e.g. to stop a forward); don't let it kill gossh first.
	signal.Ignore(os.Interrupt)

	cmd := exec.Command(sshPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}

// --- Bubble Tea TUI (The Main Menu) ---

var docStyle = lipgloss.NewStyle().Margin(1, 2)

type model struct {
	list     list.Model
	selected *SSHEntry
	action   string // "connect", "edit", "add" or "forward"
	quitting bool
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		if msg.String() == "enter" {
			if i, ok := m.list.SelectedItem().(SSHEntry); ok {
				m.selected = &i
				m.action = "connect"
				return m, tea.Quit
			}
		}
		if !m.list.SettingFilter() {
			switch msg.String() {
			case "e", "f":
				if i, ok := m.list.SelectedItem().(SSHEntry); ok {
					m.selected = &i
					m.action = map[string]string{"e": "edit", "f": "forward"}[msg.String()]
					return m, tea.Quit
				}
			case "a":
				m.action = "add"
				return m, tea.Quit
			}
		}
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.action != "" || m.quitting {
		return ""
	}
	return docStyle.Render(m.list.View())
}

func runTUI(path string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}

	// Convert SSHEntry to []list.Item
	visible := hosts(entries)
	items := make([]list.Item, len(visible))
	for i, e := range visible {
		items[i] = e
	}

	// Setup List
	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "SSH Servers"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFF")).Background(lipgloss.Color("#7D56F4")).Padding(0, 1)

	connectKey := key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "connect"))
	forwardKey := key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "forward"))
	editKey := key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit"))
	addKey := key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add"))
	bindings := []key.Binding{connectKey, forwardKey, editKey, addKey}
	l.AdditionalShortHelpKeys = func() []key.Binding { return bindings }
	l.AdditionalFullHelpKeys = func() []key.Binding { return bindings }

	m := model{list: l}

	// Run Bubble Tea Program
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Println("Error running TUI:", err)
		os.Exit(1)
	}

	if finalM, ok := finalModel.(model); ok {
		switch finalM.action {
		case "connect":
			if finalM.selected == nil {
				return
			}
			fmt.Printf("Connecting to %s (%s)...\n", finalM.selected.Alias, finalM.selected.HostName)
			runSSH(finalM.selected.Alias)
		case "forward":
			if finalM.selected == nil {
				return
			}
			forwardInteractive(path, finalM.selected.Alias)
		case "edit":
			if finalM.selected == nil {
				return
			}
			editHost(path, finalM.selected.Alias)
		case "add":
			addHost(path)
		}
	}
}

// --- CRUD Operations (Using 'huh' for forms) ---

func required(name string) func(string) error {
	return func(s string) error {
		if s == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

// hostForm edits entry in place. originalKey is the IdentityFile before editing;
// it is accepted as-is even if the file no longer exists.
func hostForm(entry *SSHEntry, originalKey string) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Host Alias").
				Description("Short name (e.g. prod-db)").
				Value(&entry.Alias).
				Validate(required("alias")),
			huh.NewInput().
				Title("HostName").
				Description("IP address or Domain").
				Value(&entry.HostName).
				Validate(required("hostname")),
			huh.NewInput().
				Title("User").
				Description("Optional username").
				Value(&entry.User),
			huh.NewInput().
				Title("Port").
				Description("Optional (default 22)").
				Value(&entry.Port),
			huh.NewInput().
				Title("IdentityFile").
				Description("Optional key name in ~/.ssh/keys (tab completes) or a path to a key").
				Suggestions(keySuggestions()).
				Value(&entry.IdentityFile).
				Validate(validateIdentityFile(originalKey)),
		),
	)
}

// applyKeyChoice resolves a changed IdentityFile, asking the user what to do with
// keys stored outside ~/.ssh/keys. Unchanged values are left exactly as they were.
// entries/self are used to avoid moving a key another host still needs.
func applyKeyChoice(entry *SSHEntry, originalKey string, entries []SSHEntry, self int) error {
	if entry.IdentityFile == originalKey {
		return nil
	}
	mode := keyKeep
	if needsImport(entry.IdentityFile) {
		var err error
		if mode, err = askKeyMode(entry.IdentityFile); err != nil {
			return err
		}
		if mode == keyMove {
			if users := keyUsedBy(entries, entry.IdentityFile, map[int]bool{self: true}); len(users) > 0 {
				fmt.Printf("Copying instead of moving: the key is also used by %s\n", strings.Join(users, ", "))
				mode = keyCopy
			}
		}
	}
	resolved, err := resolveIdentityFile(entry.IdentityFile, mode)
	if err != nil {
		return err
	}
	entry.IdentityFile = resolved
	return nil
}

func addHost(path string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}

	entry := newHostEntry()
	if err := hostForm(&entry, "").Run(); err != nil {
		return // User cancelled
	}
	if findHost(entries, entry.Alias) != -1 {
		fmt.Printf("Host '%s' already exists; use: gossh edit %s\n", entry.Alias, entry.Alias)
		return
	}
	if err := applyKeyChoice(&entry, "", entries, -1); err != nil {
		fmt.Println("Error:", err)
		return
	}

	entries = append(entries, entry)
	if err := writeConfig(path, entries); err != nil {
		fmt.Println("Error saving:", err)
	} else {
		fmt.Println("Host added!")
	}
}

// selectHost asks the user to pick one of the editable hosts.
func selectHost(entries []SSHEntry, title string) (string, bool) {
	visible := hosts(entries)
	if len(visible) == 0 {
		fmt.Println("No hosts saved yet. Add one with: gossh add")
		return "", false
	}
	options := make([]huh.Option[string], len(visible))
	for i, e := range visible {
		options[i] = huh.NewOption(fmt.Sprintf("%s (%s)", e.Alias, e.HostName), e.Alias)
	}
	var alias string
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(options...).Value(&alias),
	)).Run()
	return alias, err == nil
}

func deleteHost(path string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}
	alias, ok := selectHost(entries, "Select Host to Delete")
	if !ok {
		return
	}

	idx := findHost(entries, alias)
	entries = append(entries[:idx], entries[idx+1:]...)
	if err := writeConfig(path, entries); err != nil {
		fmt.Println("Error saving:", err)
		return
	}
	fmt.Printf("Deleted %s\n", alias)
}

func editHost(path string, alias string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}

	if alias == "" {
		var ok bool
		if alias, ok = selectHost(entries, "Select Host to Edit"); !ok {
			return
		}
	}

	idx := findHost(entries, alias)
	if idx == -1 {
		fmt.Printf("Host '%s' not found.\n", alias)
		return
	}

	entry := entries[idx]
	originalKey := entry.IdentityFile
	if err := hostForm(&entry, originalKey).Run(); err != nil {
		return
	}
	if entry.Alias != alias && findHost(entries, entry.Alias) != -1 {
		fmt.Printf("Host '%s' already exists.\n", entry.Alias)
		return
	}
	if err := applyKeyChoice(&entry, originalKey, entries, idx); err != nil {
		fmt.Println("Error:", err)
		return
	}

	entries[idx] = entry
	if err := writeConfig(path, entries); err != nil {
		fmt.Println("Error saving:", err)
	} else {
		fmt.Println("Host updated!")
	}
}

func listHostsCLI(path string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tHOST\tUSER\tPORT\tKEY")
	for _, e := range hosts(entries) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Alias, e.HostName, e.User, e.Port, e.IdentityFile)
	}
	w.Flush()
}

// --- Configuration Helpers (Standard Lib) ---

func getConfigPath() (string, error) {
	if err := ensureSSHLayout(); err != nil {
		return "", err
	}
	sshDir, _, err := sshDirs()
	if err != nil {
		return "", err
	}
	return filepath.Join(sshDir, "config"), nil
}
