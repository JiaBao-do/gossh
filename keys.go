package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
)

// keysDirRef is how keys inside the managed folder are written to ~/.ssh/config.
// OpenSSH expands "~" on Linux, macOS and Windows, so the config stays portable.
const keysDirRef = "~/.ssh/keys"

// keyMode is what to do with a key that lives outside ~/.ssh/keys.
type keyMode string

const (
	keyCopy keyMode = "copy" // copy into ~/.ssh/keys, leave the original
	keyMove keyMode = "move" // move into ~/.ssh/keys, remove the original
	keyKeep keyMode = "keep" // reference the key where it is
)

// sshDirs returns ~/.ssh and ~/.ssh/keys.
func sshDirs() (sshDir, keysDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	sshDir = filepath.Join(home, ".ssh")
	return sshDir, filepath.Join(sshDir, "keys"), nil
}

// ensureSSHLayout creates ~/.ssh, ~/.ssh/keys and an empty ~/.ssh/config if missing.
func ensureSSHLayout() error {
	sshDir, keysDir, err := sshDirs()
	if err != nil {
		return err
	}
	for _, dir := range []string{sshDir, keysDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	configPath := filepath.Join(sshDir, "config")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		f, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("creating %s: %w", configPath, err)
		}
		f.Close()
	}
	return nil
}

// listKeys returns the private key file names stored in ~/.ssh/keys.
func listKeys() []string {
	_, keysDir, err := sshDirs()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// keySuggestions returns completions for the IdentityFile input.
func keySuggestions() []string {
	var s []string
	for _, k := range listKeys() {
		s = append(s, k, keysDirRef+"/"+k)
	}
	return s
}

// expandHome turns a leading "~" into the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func cleanKeyInput(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"'`)
}

// managedKeyName returns the file name if input refers to a key inside ~/.ssh/keys,
// either as a bare name or as a path into that folder.
func managedKeyName(input string) (string, bool) {
	input = cleanKeyInput(input)
	_, keysDir, err := sshDirs()
	if err != nil || input == "" {
		return "", false
	}
	if !strings.ContainsAny(input, `/\`) {
		if _, err := os.Stat(filepath.Join(keysDir, input)); err == nil {
			return input, true
		}
	}
	if abs, err := filepath.Abs(expandHome(input)); err == nil && strings.EqualFold(filepath.Dir(abs), keysDir) {
		return filepath.Base(abs), true
	}
	return "", false
}

// needsImport reports whether input is an existing key file outside ~/.ssh/keys.
func needsImport(input string) bool {
	input = cleanKeyInput(input)
	if input == "" {
		return false
	}
	if _, ok := managedKeyName(input); ok {
		return false
	}
	info, err := os.Stat(expandHome(input))
	return err == nil && !info.IsDir()
}

// validateIdentityFile accepts empty input, the unchanged original value (even if
// that file is gone), a key name in ~/.ssh/keys, or an existing file.
func validateIdentityFile(original string) func(string) error {
	return func(s string) error {
		s = cleanKeyInput(s)
		if s == "" || s == original {
			return nil
		}
		if _, ok := managedKeyName(s); ok {
			return nil
		}
		info, err := os.Stat(expandHome(s))
		if err != nil {
			return fmt.Errorf("key file not found: %s", s)
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory, not a key file", s)
		}
		return nil
	}
}

// askKeyMode asks what to do with a key outside ~/.ssh/keys.
func askKeyMode(path string) (keyMode, error) {
	mode := keyCopy
	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[keyMode]().
			Title("This key is outside ~/.ssh/keys").
			Description(path).
			Options(
				huh.NewOption("Copy into ~/.ssh/keys (keep the original)", keyCopy),
				huh.NewOption("Move into ~/.ssh/keys (remove the original)", keyMove),
				huh.NewOption("Keep it where it is", keyKeep),
			).
			Value(&mode),
	)).Run()
	return mode, err
}

// resolveIdentityFile turns the IdentityFile the user entered into the value
// written to ~/.ssh/config, importing the key according to mode.
func resolveIdentityFile(input string, mode keyMode) (string, error) {
	input = cleanKeyInput(input)
	if input == "" {
		return "", nil
	}
	if name, ok := managedKeyName(input); ok {
		return keysDirRef + "/" + name, nil
	}
	if mode == keyKeep {
		return input, nil
	}
	_, keysDir, err := sshDirs()
	if err != nil {
		return "", err
	}
	src, err := filepath.Abs(expandHome(input))
	if err != nil {
		return "", err
	}
	name, err := importKey(src, keysDir, mode == keyMove)
	if err != nil {
		return "", err
	}
	return keysDirRef + "/" + name, nil
}

// importKey copies (or moves) src and src.pub into keysDir and returns the stored name.
// An identical key already present is reused; a different key with the same name gets a suffix.
func importKey(src, keysDir string, move bool) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading key: %w", err)
	}

	base := filepath.Base(src)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	name := base
	reused := false
	for i := 1; ; i++ {
		existing, err := os.ReadFile(filepath.Join(keysDir, name))
		if os.IsNotExist(err) {
			break
		}
		if err == nil && bytes.Equal(existing, data) {
			reused = true
			break
		}
		name = fmt.Sprintf("%s-%d%s", stem, i, ext)
	}

	dst := filepath.Join(keysDir, name)
	if !reused {
		if err := writePrivateFile(dst, data); err != nil {
			return "", err
		}
	}
	pub, pubErr := os.ReadFile(src + ".pub")
	if pubErr == nil {
		if _, err := os.Stat(dst + ".pub"); os.IsNotExist(err) {
			if err := os.WriteFile(dst+".pub", pub, 0644); err != nil {
				return "", err
			}
		}
	}

	if move {
		if err := os.Remove(src); err != nil {
			return "", fmt.Errorf("key copied to %s but the original could not be removed: %w", dst, err)
		}
		if pubErr == nil {
			os.Remove(src + ".pub")
		}
		fmt.Printf("Moved key to %s\n", dst)
	} else if reused {
		fmt.Printf("Key already in %s\n", dst)
	} else {
		fmt.Printf("Copied key to %s\n", dst)
	}
	return name, nil
}

// writePrivateFile writes a file readable only by the current user.
// On Windows the ACL is locked down too, otherwise OpenSSH rejects the key.
func writePrivateFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if u, err := user.Current(); err == nil {
			exec.Command("icacls", path, "/inheritance:r", "/grant:r", u.Username+":F").Run()
		}
	}
	return nil
}

// samePath compares two key paths after "~" expansion.
func samePath(a, b string) bool {
	pa, errA := filepath.Abs(expandHome(cleanKeyInput(a)))
	pb, errB := filepath.Abs(expandHome(cleanKeyInput(b)))
	return errA == nil && errB == nil && strings.EqualFold(pa, pb)
}

// keyUsedBy returns the aliases (or block headers) still referencing src,
// ignoring the entries whose index is in skip.
func keyUsedBy(entries []SSHEntry, src string, skip map[int]bool) []string {
	var users []string
	for i, e := range entries {
		if skip[i] {
			continue
		}
		if e.Editable() {
			if e.IdentityFile != "" && samePath(e.IdentityFile, src) {
				users = append(users, e.Alias)
			}
			continue
		}
		for _, l := range e.lines {
			if l.key != "identityfile" {
				continue
			}
			if _, v := splitDirective(l.raw); samePath(v, src) {
				name := strings.TrimSpace(e.header)
				if name == "" {
					name = "global settings"
				}
				users = append(users, name)
			}
		}
	}
	return users
}

// --- CLI -----------------------------------------------------------------

// keysCommand implements "gossh keys [import <path> | migrate]".
// keysMenu is the "k" screen: see the managed keys, import one, or bring
// the keys hosts use from other folders into ~/.ssh/keys.
func keysMenu(configPath string) (string, error) {
	desc := "No keys in ~/.ssh/keys yet."
	if keys := listKeys(); len(keys) > 0 {
		desc = "In ~/.ssh/keys: " + strings.Join(keys, ", ")
	}
	action := "import"
	if err := huh.NewSelect[string]().
		Title("Keys").
		Description(desc).
		Options(
			huh.NewOption("Import a key file (.pem, id_rsa, ...) into ~/.ssh/keys", "import"),
			huh.NewOption("Bring keys my hosts use from other folders into ~/.ssh/keys", "migrate"),
			huh.NewOption("Back", ""),
		).
		Value(&action).
		Run(); err != nil {
		return "", err
	}
	switch action {
	case "import":
		return importKeyInteractive(configPath)
	case "migrate":
		return migrateKeys(configPath)
	}
	return "", nil
}

// importKeyInteractive picks a key file (found ones first, or browse) and
// copies or moves it into ~/.ssh/keys.
func importKeyInteractive(configPath string) (string, error) {
	_, keysDir, err := sshDirs()
	if err != nil {
		return "", err
	}
	var opts []huh.Option[string]
	for _, p := range discoverKeys() {
		opts = append(opts, huh.NewOption(tildePath(p), p))
	}
	opts = append(opts, huh.NewOption("Browse for a key file...", keyChoiceBrowse))

	choice, browsed, move := keyChoiceBrowse, "", false
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which key?").
				Description("Private keys found in ~/.ssh, Downloads and Desktop").
				Options(opts...).
				Value(&choice),
		),
		huh.NewGroup(
			keyFilePicker("Pick the private key file", &browsed, validateIdentityFile("")),
		).WithHideFunc(func() bool { return choice != keyChoiceBrowse }),
		huh.NewGroup(
			huh.NewSelect[bool]().
				Title("Copy or move it into ~/.ssh/keys?").
				Options(
					huh.NewOption("Copy (keep the original)", false),
					huh.NewOption("Move (remove the original)", true),
				).
				Value(&move),
		),
	).Run(); err != nil {
		return "", err
	}
	if choice == keyChoiceBrowse {
		choice = browsed
	}
	src, err := filepath.Abs(expandHome(choice))
	if err != nil {
		return "", err
	}

	note := ""
	if move {
		if entries, err := parseConfig(configPath); err == nil {
			if users := keyUsedBy(entries, src, nil); len(users) > 0 {
				note = fmt.Sprintf(" (copied, not moved: %s still use the original)", strings.Join(users, ", "))
				move = false
			}
		}
	}
	name, err := importKey(src, keysDir, move)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Imported %s as %s/%s%s", filepath.Base(src), keysDirRef, name, note), nil
}

// migrateKeys moves or copies keys referenced from outside ~/.ssh/keys, for the
// hosts the user picks, and rewrites those hosts' IdentityFile.
func migrateKeys(configPath string) (string, error) {
	entries, err := parseConfig(configPath)
	if err != nil {
		return "", fmt.Errorf("parsing config: %w", err)
	}

	var options []huh.Option[string]
	var selected []string
	var missing []string
	for _, e := range hosts(entries) {
		if e.IdentityFile == "" {
			continue
		}
		if _, ok := managedKeyName(e.IdentityFile); ok {
			continue
		}
		if !needsImport(e.IdentityFile) {
			missing = append(missing, fmt.Sprintf("%s -> %s", e.Alias, e.IdentityFile))
			continue
		}
		options = append(options, huh.NewOption(fmt.Sprintf("%s  (%s)", e.Alias, e.IdentityFile), e.Alias).Selected(true))
		selected = append(selected, e.Alias)
	}

	var notes []string
	if len(missing) > 0 {
		notes = append(notes, "skipped "+strings.Join(missing, ", ")+" (file not found or uses ssh tokens)")
	}
	if len(options) == 0 {
		return strings.Join(append([]string{"All host keys are already in ~/.ssh/keys"}, notes...), "; "), nil
	}

	mode := keyCopy
	err = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Hosts with keys outside ~/.ssh/keys").
			Description("space to toggle, enter to confirm").
			Options(options...).
			Value(&selected),
		huh.NewSelect[keyMode]().
			Title("What should happen to these keys?").
			Options(
				huh.NewOption("Copy into ~/.ssh/keys (keep originals)", keyCopy),
				huh.NewOption("Move into ~/.ssh/keys (remove originals)", keyMove),
				huh.NewOption("Keep them where they are (no changes)", keyKeep),
			).
			Value(&mode),
	)).Run()
	if err != nil {
		return "", err
	}
	if mode == keyKeep || len(selected) == 0 {
		return "No changes made", nil
	}

	// Several hosts can share one key; import each source file once.
	imported := map[string]string{}
	changed := 0
	for _, alias := range selected {
		i := findHost(entries, alias)
		src := expandHome(cleanKeyInput(entries[i].IdentityFile))
		ref, ok := imported[src]
		if !ok {
			// Copy first; moving happens only after every host is updated.
			if ref, err = resolveIdentityFile(src, keyCopy); err != nil {
				notes = append(notes, fmt.Sprintf("%s: %v", alias, err))
				continue
			}
			imported[src] = ref
		}
		entries[i].IdentityFile = ref
		changed++
	}
	if changed == 0 {
		return strings.Join(append([]string{"No hosts updated"}, notes...), "; "), nil
	}
	if err := writeConfig(configPath, entries); err != nil {
		return "", fmt.Errorf("saving: %w", err)
	}
	if mode == keyMove {
		skip := map[int]bool{}
		for _, alias := range selected {
			skip[findHost(entries, alias)] = true
		}
		for src := range imported {
			if users := keyUsedBy(entries, src, skip); len(users) > 0 {
				notes = append(notes, fmt.Sprintf("kept %s (still used by %s)", src, strings.Join(users, ", ")))
				continue
			}
			if err := os.Remove(src); err != nil {
				notes = append(notes, fmt.Sprintf("could not remove %s: %v", src, err))
				continue
			}
			os.Remove(src + ".pub")
		}
	}
	summary := fmt.Sprintf("Updated %d host(s) to use ~/.ssh/keys (old config in config.bak)", changed)
	return strings.Join(append([]string{summary}, notes...), "; "), nil
}

// isPrivateKey reports whether path looks like a PEM/OpenSSH private key.
func isPrivateKey(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 128)
	n, _ := f.Read(head)
	return bytes.Contains(head[:n], []byte("PRIVATE KEY-----"))
}

// discoverKeys finds private keys outside ~/.ssh/keys that the user probably
// means: loose keys in ~/.ssh and .pem/.key files in Downloads and Desktop.
func discoverKeys() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var found []string
	for _, dir := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, "Downloads"), filepath.Join(home, "Desktop")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasSuffix(name, ".pub") {
				continue
			}
			ext := strings.ToLower(filepath.Ext(name))
			if filepath.Base(dir) != ".ssh" && ext != ".pem" && ext != ".key" {
				continue // outside ~/.ssh only look at obvious key files
			}
			if p := filepath.Join(dir, name); isPrivateKey(p) {
				found = append(found, p)
			}
		}
	}
	return found
}

// tildePath shows a path under the home directory as ~/...
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}
