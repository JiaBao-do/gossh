package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// --- Structs ---

type SSHEntry struct {
	Alias        string
	HostName     string
	User         string
	Port         string
	IdentityFile string
}

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
	case "delete":
		deleteHost(configPath)
	case "interactive":
		runTUI(configPath)
	default:
		runTUI(configPath)
	}
}

// --- Bubble Tea TUI (The Main Menu) ---

var docStyle = lipgloss.NewStyle().Margin(1, 2)

type model struct {
	list     list.Model
	selected *SSHEntry
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
			// Get the selected item
			if i, ok := m.list.SelectedItem().(SSHEntry); ok {
				m.selected = &i
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
	if m.selected != nil {
		return "" // Clear screen on exit to run SSH
	}
	return docStyle.Render(m.list.View())
}

func runTUI(path string) {
	entries, err := parseConfig(path)
	if err != nil {
		fmt.Println("Error parsing config:", err)
		return
	}

	if len(entries) == 0 {
		fmt.Println("No hosts found. Run 'gossh add' first.")
		return
	}

	// Convert SSHEntry to []list.Item
	items := make([]list.Item, len(entries))
	for i, e := range entries {
		items[i] = e
	}

	// Setup List
	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "SSH Servers"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFF")).Background(lipgloss.Color("#7D56F4")).Padding(0, 1)

	m := model{list: l}

	// Run Bubble Tea Program
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Println("Error running TUI:", err)
		os.Exit(1)
	}

	// Extract selection and Connect
	if finalM, ok := finalModel.(model); ok && finalM.selected != nil {
		fmt.Printf("Connecting to %s (%s)...\n", finalM.selected.Alias, finalM.selected.HostName)

		cmd := exec.Command("ssh", finalM.selected.Alias)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
	}
}

// --- CRUD Operations (Using 'huh' for forms) ---

func addHost(path string) {
	var entry SSHEntry

	// 'huh' replaces 'survey' for forms
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Host Alias").
				Description("Short name (e.g. prod-db)").
				Value(&entry.Alias).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("alias is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("HostName").
				Description("IP address or Domain").
				Value(&entry.HostName).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("hostname is required")
					}
					return nil
				}),
			huh.NewInput().
				Title("User").
				Description("Optional username").
				Value(&entry.User),
			huh.NewInput().
				Title("Port").
				Description("Optional (default 22)").
				Value(&entry.Port),
		),
	)

	err := form.Run()
	if err != nil {
		return // User cancelled
	}

	entries, _ := parseConfig(path)
	entries = append(entries, entry)

	if err := writeConfig(path, entries); err != nil {
		fmt.Println("Error saving:", err)
	} else {
		fmt.Println("Host added!")
	}
}

func deleteHost(path string) {
	entries, _ := parseConfig(path)
	if len(entries) == 0 {
		fmt.Println("No hosts to delete.")
		return
	}

	// Map entries to options for huh.Select
	options := make([]huh.Option[string], len(entries))
	for i, e := range entries {
		options[i] = huh.NewOption(fmt.Sprintf("%s (%s)", e.Alias, e.HostName), e.Alias)
	}

	var selectedAlias string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select Host to Delete").
				Options(options...).
				Value(&selectedAlias),
		),
	)

	err := form.Run()
	if err != nil {
		return
	}

	// Filter out the deleted one
	var newEntries []SSHEntry
	for _, e := range entries {
		if e.Alias != selectedAlias {
			newEntries = append(newEntries, e)
		}
	}

	writeConfig(path, newEntries)
	fmt.Printf("Deleted %s\n", selectedAlias)
}

func listHostsCLI(path string) {
	entries, _ := parseConfig(path)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tHOST\tUSER\tPORT")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Alias, e.HostName, e.User, e.Port)
	}
	w.Flush()
}

// --- Configuration Helpers (Standard Lib) ---

func getConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		os.Mkdir(configDir, 0700)
	}
	return filepath.Join(configDir, "config"), nil
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

	var entries []SSHEntry
	var current *SSHEntry

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		key := strings.ToLower(parts[0])
		value := parts[1]

		if key == "host" {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &SSHEntry{Alias: value}
		} else if current != nil {
			switch key {
			case "hostname":
				current.HostName = value
			case "user":
				current.User = value
			case "port":
				current.Port = value
			case "identityfile":
				current.IdentityFile = value
			}
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries, scanner.Err()
}

func writeConfig(path string, entries []SSHEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, e := range entries {
		fmt.Fprintf(w, "Host %s\n", e.Alias)
		if e.HostName != "" {
			fmt.Fprintf(w, "  HostName %s\n", e.HostName)
		}
		if e.User != "" {
			fmt.Fprintf(w, "  User %s\n", e.User)
		}
		if e.Port != "" {
			fmt.Fprintf(w, "  Port %s\n", e.Port)
		}
		if e.IdentityFile != "" {
			fmt.Fprintf(w, "  IdentityFile %s\n", e.IdentityFile)
		}
		fmt.Fprintln(w, "")
	}
	return w.Flush()
}
