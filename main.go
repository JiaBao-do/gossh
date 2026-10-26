package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

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
	cleanupOldBinary()
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Printf("gossh %s\n", version)
			return
		case "help", "--help", "-h":
			printHelp()
			return
		case "upgrade", "update":
			if err := upgradeCLI(); err != nil {
				fmt.Println("Error:", err)
				os.Exit(1)
			}
			return
		}
	}

	configPath, err := getConfigPath()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	if len(args) == 0 {
		runMenu(configPath)
		return
	}
	// "gossh prod-db", "gossh user@host -p 2222": straight to ssh.
	os.Exit(runSSH(args...))
}

func printHelp() {
	fmt.Println("gossh - SSH host manager")
	fmt.Println()
	fmt.Println("  gossh                   Open gossh. Everything is done from there: connect,")
	fmt.Println("                          transfer files, port forwarding, add/edit/delete hosts, keys")
	fmt.Println("  gossh <alias> [args]    Connect straight to a saved host (any ssh args work)")
	fmt.Println("  gossh user@host [args]  Plain ssh")
	fmt.Println("  gossh upgrade           Install the latest gossh release")
	fmt.Println("  gossh version           Show version")
	fmt.Println()
	fmt.Println("Keys in gossh:")
	fmt.Println("  enter  connect           t  transfer files       s  transfer files as root (sudo)")
	fmt.Println("  f      port forwarding   a  add host             e  edit host")
	fmt.Println("  d      delete host       k  manage keys          u  upgrade gossh")
	fmt.Println("  /      filter            ?  more keys            q  quit")
	fmt.Println()
	fmt.Println("Existing hosts are never changed automatically, and ~/.ssh/config is backed")
	fmt.Println("up to ~/.ssh/config.bak before every change.")
}

// runSSH runs the system ssh client with the given arguments and returns its exit code.
func runSSH(args ...string) int {
	sshPath := sshBinary()

	// ssh handles Ctrl+C itself (e.g. to stop a forward); don't let it kill gossh first.
	signal.Ignore(os.Interrupt)

	cmd := exec.Command(sshPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}

// waitForEnter keeps output (like an ssh error) on screen before the menu
// takes the terminal back.
func waitForEnter() {
	fmt.Print("\nPress enter to go back to gossh...")
	fmt.Scanln()
}

// --- Bubble Tea TUI (The Main Menu) ---

var (
	docStyle       = lipgloss.NewStyle().Margin(1, 2)
	menuOKStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#87D787"))
	menuErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F5F"))
)

type model struct {
	list      list.Model
	selected  string // alias the action applies to
	action    string // "" means quit
	status    string
	statusErr bool
}

// menuActions maps keys to actions; the bool says whether a host must be selected.
var menuActions = map[string]struct {
	action    string
	needsHost bool
}{
	"enter": {"connect", true},
	"t":     {"files", true},
	"s":     {"files-sudo", true},
	"f":     {"forward", true},
	"e":     {"edit", true},
	"d":     {"delete", true},
	"a":     {"add", false},
	"k":     {"keys", false},
	"u":     {"upgrade", false},
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if a, ok := menuActions[msg.String()]; ok && !m.list.SettingFilter() {
			if a.needsHost {
				i, ok := m.list.SelectedItem().(SSHEntry)
				if !ok {
					m.status, m.statusErr = "No host selected. Press a to add one.", true
					return m, nil
				}
				m.selected = i.Alias
			}
			m.action = a.action
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v-1)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.action != "" {
		return ""
	}
	status := menuOKStyle.Render(m.status)
	if m.statusErr {
		status = menuErrorStyle.Render(m.status)
	}
	return docStyle.Render(m.list.View() + "\n" + status)
}

// pickHost shows the host list until the user picks an action.
func pickHost(entries []SSHEntry, status string, statusErr bool) (action, alias string, err error) {
	visible := hosts(entries)
	items := make([]list.Item, len(visible))
	for i, e := range visible {
		items[i] = e
	}

	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "SSH Servers"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.SetStatusBarItemName("host", "hosts")
	l.Styles.Title = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFF")).Background(lipgloss.Color("#7D56F4")).Padding(0, 1)

	// Free up k/d/u/f/b (the list's vim-style defaults) for gossh actions.
	l.KeyMap.CursorUp = key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up"))
	l.KeyMap.CursorDown = key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down"))
	l.KeyMap.PrevPage = key.NewBinding(key.WithKeys("left", "pgup"), key.WithHelp("←/pgup", "prev page"))
	l.KeyMap.NextPage = key.NewBinding(key.WithKeys("right", "pgdown"), key.WithHelp("→/pgdn", "next page"))

	bind := func(k, help string) key.Binding {
		return key.NewBinding(key.WithKeys(k), key.WithHelp(k, help))
	}
	short := []key.Binding{bind("enter", "connect"), bind("t", "files"), bind("f", "forward"), bind("a", "add"), bind("e", "edit")}
	full := append(short, bind("s", "files as root"), bind("d", "delete"), bind("k", "keys"), bind("u", "upgrade gossh"))
	l.AdditionalShortHelpKeys = func() []key.Binding { return short }
	l.AdditionalFullHelpKeys = func() []key.Binding { return full }

	if status == "" && len(visible) == 0 {
		status = "No hosts yet. Press a to add one."
	}
	final, err := tea.NewProgram(model{list: l, status: status, statusErr: statusErr}, tea.WithAltScreen()).Run()
	if err != nil {
		return "", "", err
	}
	m := final.(model)
	return m.action, m.selected, nil
}

// runMenu is gossh's home screen: pick a host and an action, do it, come back.
func runMenu(path string) {
	status, statusErr := "", false
	for {
		entries, err := parseConfig(path)
		if err != nil {
			fmt.Println("Error parsing config:", err)
			os.Exit(1)
		}
		action, alias, err := pickHost(entries, status, statusErr)
		if err != nil {
			fmt.Println("Error running TUI:", err)
			os.Exit(1)
		}
		if action == "" {
			return
		}

		var msg string
		switch action {
		case "connect":
			fmt.Printf("Connecting to %s...\n", alias)
			code := runSSH(alias)
			msg = "Disconnected from " + alias
			if code == 255 {
				// ssh's own failure (unreachable, auth...); keep its message visible.
				waitForEnter()
				err = fmt.Errorf("could not connect to %s", alias)
			}
		case "files", "files-sudo":
			if err = runFiles(alias, "", action == "files-sudo"); err != nil {
				waitForEnter()
			}
		case "forward":
			msg, err = forwardInteractive(alias)
		case "add":
			msg, err = addHost(path)
		case "edit":
			msg, err = editHost(path, alias)
		case "delete":
			msg, err = deleteHost(path, alias)
		case "keys":
			msg, err = keysMenu(path)
		case "upgrade":
			var upgraded bool
			if upgraded, msg, err = upgradeInteractive(); upgraded {
				fmt.Println(msg)
				return // the binary on disk is the new version now
			}
		}

		status, statusErr = msg, false
		switch {
		case errors.Is(err, huh.ErrUserAborted):
			status = "Cancelled"
		case err != nil:
			status, statusErr = err.Error(), true
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

// Key picker choices that are not a key path ('<' can't start a usable path).
const (
	keyChoiceKeep   = "<keep>"
	keyChoiceNone   = "<none>"
	keyChoiceBrowse = "<browse>"
	keyChoiceType   = "<type>"
)

// keyOptions lists what the IdentityFile picker offers: the current key,
// no key, the managed keys, keys found elsewhere, then browse / type a path.
func keyOptions(current string) []huh.Option[string] {
	var opts []huh.Option[string]
	if current != "" {
		opts = append(opts, huh.NewOption("Keep current: "+current, keyChoiceKeep))
	}
	opts = append(opts, huh.NewOption("No key (ssh agent, default keys or password)", keyChoiceNone))
	for _, k := range listKeys() {
		ref := keysDirRef + "/" + k
		if ref != current {
			opts = append(opts, huh.NewOption(k+"   (~/.ssh/keys)", ref))
		}
	}
	for _, p := range discoverKeys() {
		if p != expandHome(current) {
			opts = append(opts, huh.NewOption(filepath.Base(p)+"   ("+tildePath(p)+", will offer to copy into ~/.ssh/keys)", p))
		}
	}
	return append(opts,
		huh.NewOption("Browse for a key file...", keyChoiceBrowse),
		huh.NewOption("Type a path...", keyChoiceType))
}

// keyFilePicker is a file browser for choosing a private key, starting at home.
func keyFilePicker(title string, value *string, validate func(string) error) *huh.FilePicker {
	home, _ := os.UserHomeDir()
	return huh.NewFilePicker().
		Title(title).
		Description("enter opens a folder or picks the file, backspace goes up, esc cancels").
		CurrentDirectory(home).
		ShowHidden(true).
		ShowSize(true).
		Height(15).
		Value(value).
		Validate(validate)
}

// runHostForm edits entry in place. originalKey is the IdentityFile before
// editing; it is accepted as-is even if the file no longer exists.
func runHostForm(entry *SSHEntry, originalKey string) error {
	choice := keyChoiceNone
	if originalKey != "" {
		choice = keyChoiceKeep
	}
	browsed, typed := "", entry.IdentityFile

	err := huh.NewForm(
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
		),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Key (IdentityFile)").
				Description("The private key / .pem used to log in. Keys in ~/.ssh/keys are listed first.").
				Options(keyOptions(originalKey)...).
				Value(&choice),
		),
		huh.NewGroup(
			keyFilePicker("Pick the private key file", &browsed, validateIdentityFile(originalKey)),
		).WithHideFunc(func() bool { return choice != keyChoiceBrowse }),
		huh.NewGroup(
			huh.NewInput().
				Title("Key path").
				Description("A key name in ~/.ssh/keys (tab completes) or a path to a key").
				Suggestions(keySuggestions()).
				Value(&typed).
				Validate(validateIdentityFile(originalKey)),
		).WithHideFunc(func() bool { return choice != keyChoiceType }),
	).Run()
	if err != nil {
		return err
	}
	entry.IdentityFile = chosenKey(choice, originalKey, browsed, typed)
	return nil
}

// chosenKey turns the picker's answers into the IdentityFile value.
func chosenKey(choice, original, browsed, typed string) string {
	switch choice {
	case keyChoiceKeep:
		return original
	case keyChoiceNone:
		return ""
	case keyChoiceBrowse:
		return browsed
	case keyChoiceType:
		return cleanKeyInput(typed)
	}
	return choice
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

func addHost(path string) (string, error) {
	entries, err := parseConfig(path)
	if err != nil {
		return "", fmt.Errorf("parsing config: %w", err)
	}

	entry := newHostEntry()
	if err := runHostForm(&entry, ""); err != nil {
		return "", err
	}
	if findHost(entries, entry.Alias) != -1 {
		return "", fmt.Errorf("host %q already exists; select it and press e to edit it", entry.Alias)
	}
	if err := applyKeyChoice(&entry, "", entries, -1); err != nil {
		return "", err
	}

	entries = append(entries, entry)
	if err := writeConfig(path, entries); err != nil {
		return "", fmt.Errorf("saving: %w", err)
	}
	return "Added " + entry.Alias, nil
}

func deleteHost(path, alias string) (string, error) {
	entries, err := parseConfig(path)
	if err != nil {
		return "", fmt.Errorf("parsing config: %w", err)
	}
	idx := findHost(entries, alias)
	if idx == -1 {
		return "", fmt.Errorf("host %q not found", alias)
	}

	confirmed := false
	if err := huh.NewConfirm().
		Title(fmt.Sprintf("Delete %s?", alias)).
		Description("Removes its Host block from ~/.ssh/config (the old file is kept as config.bak).\nKey files are not deleted.").
		Affirmative("Delete").
		Negative("Cancel").
		Value(&confirmed).
		Run(); err != nil || !confirmed {
		return "", err
	}

	entries = append(entries[:idx], entries[idx+1:]...)
	if err := writeConfig(path, entries); err != nil {
		return "", fmt.Errorf("saving: %w", err)
	}
	return "Deleted " + alias, nil
}

func editHost(path string, alias string) (string, error) {
	entries, err := parseConfig(path)
	if err != nil {
		return "", fmt.Errorf("parsing config: %w", err)
	}
	idx := findHost(entries, alias)
	if idx == -1 {
		return "", fmt.Errorf("host %q not found", alias)
	}

	entry := entries[idx]
	originalKey := entry.IdentityFile
	if err := runHostForm(&entry, originalKey); err != nil {
		return "", err
	}
	if entry.Alias != alias && findHost(entries, entry.Alias) != -1 {
		return "", fmt.Errorf("host %q already exists", entry.Alias)
	}
	if err := applyKeyChoice(&entry, originalKey, entries, idx); err != nil {
		return "", err
	}

	entries[idx] = entry
	if err := writeConfig(path, entries); err != nil {
		return "", fmt.Errorf("saving: %w", err)
	}
	return "Updated " + entry.Alias, nil
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
