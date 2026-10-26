package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/pkg/sftp"
)

// fileSystem is the small set of operations transfers and the file browser
// need. localFS is this machine; remoteFS is a host reached over SFTP.
type fileSystem interface {
	Label() string // "local" or the host alias
	Remote() bool
	ReadDir(dir string) ([]os.FileInfo, error) // entries are lstat'ed
	Stat(p string) (os.FileInfo, error)
	Open(p string) (io.ReadCloser, error)
	Create(p string) (io.WriteCloser, error)
	MkdirAll(p string) error
	RemoveAll(p string) error
	Rename(oldPath, newPath string) error
	Chmod(p string, mode os.FileMode) error
	Chtimes(p string, mtime time.Time) error
	Glob(pattern string) ([]string, error)
	Join(elem ...string) string
	Dir(p string) string
	Base(p string) string
	Abs(p string) (string, error) // resolves ~ and relative paths
	// ModesMeaningful reports whether permission bits read from this file
	// system are worth copying (they are not on Windows).
	ModesMeaningful() bool
}

// --- Local ---

type localFS struct{}

func (localFS) Label() string { return "local" }
func (localFS) Remote() bool  { return false }

func (localFS) ReadDir(dir string) ([]os.FileInfo, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdir(-1)
}

func (localFS) Stat(p string) (os.FileInfo, error)      { return os.Stat(p) }
func (localFS) Open(p string) (io.ReadCloser, error)    { return os.Open(p) }
func (localFS) Create(p string) (io.WriteCloser, error) { return os.Create(p) }
func (localFS) MkdirAll(p string) error                 { return os.MkdirAll(p, 0755) }
func (localFS) RemoveAll(p string) error                { return os.RemoveAll(p) }
func (localFS) Rename(o, n string) error                { return os.Rename(o, n) }
func (localFS) Chmod(p string, m os.FileMode) error     { return os.Chmod(p, m) }
func (localFS) Chtimes(p string, t time.Time) error     { return os.Chtimes(p, t, t) }
func (localFS) Glob(pattern string) ([]string, error)   { return filepath.Glob(pattern) }
func (localFS) Join(elem ...string) string              { return filepath.Join(elem...) }
func (localFS) Dir(p string) string                     { return filepath.Dir(p) }
func (localFS) Base(p string) string                    { return filepath.Base(p) }
func (localFS) ModesMeaningful() bool                   { return runtime.GOOS != "windows" }

func (localFS) Abs(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

// --- Remote (SFTP) ---

type remoteFS struct {
	c    *sftp.Client
	host string
	home string // working directory the server started in, normally $HOME
}

func (r *remoteFS) Label() string { return r.host }
func (r *remoteFS) Remote() bool  { return true }

func (r *remoteFS) ReadDir(dir string) ([]os.FileInfo, error) { return r.c.ReadDir(dir) }
func (r *remoteFS) Stat(p string) (os.FileInfo, error)        { return r.c.Stat(p) }
func (r *remoteFS) Open(p string) (io.ReadCloser, error)      { return r.c.Open(p) }
func (r *remoteFS) Create(p string) (io.WriteCloser, error)   { return r.c.Create(p) }
func (r *remoteFS) MkdirAll(p string) error                   { return r.c.MkdirAll(p) }
func (r *remoteFS) RemoveAll(p string) error                  { return r.c.RemoveAll(p) }
func (r *remoteFS) Chmod(p string, m os.FileMode) error       { return r.c.Chmod(p, m) }
func (r *remoteFS) Chtimes(p string, t time.Time) error       { return r.c.Chtimes(p, t, t) }
func (r *remoteFS) Glob(pattern string) ([]string, error)     { return r.c.Glob(pattern) }
func (r *remoteFS) Join(elem ...string) string                { return path.Join(elem...) }
func (r *remoteFS) Dir(p string) string                       { return path.Dir(p) }
func (r *remoteFS) Base(p string) string                      { return path.Base(p) }
func (r *remoteFS) ModesMeaningful() bool                     { return true }

// Rename uses the posix-rename extension when available so an existing
// target is replaced, like a local rename.
func (r *remoteFS) Rename(o, n string) error {
	if err := r.c.PosixRename(o, n); err == nil {
		return nil
	}
	return r.c.Rename(o, n)
}

func (r *remoteFS) Abs(p string) (string, error) {
	switch {
	case p == "" || p == "~":
		return r.home, nil
	case strings.HasPrefix(p, "~/"):
		return path.Join(r.home, p[2:]), nil
	case path.IsAbs(p):
		return path.Clean(p), nil
	}
	return path.Join(r.home, p), nil
}

// --- Connecting ---

// sshBinary returns the path of the system ssh client, or exits with advice.
func sshBinary() string {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Println("Error: ssh client not found on PATH.")
		if runtime.GOOS == "windows" {
			fmt.Println("Install it with (admin): Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0")
		}
		os.Exit(1)
	}
	return sshPath
}

// sftpServerScript starts the remote sftp-server binary as root. Its location
// differs between distributions, so the usual places are tried in order.
const sftpServerScript = `for p in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server ` +
	`/usr/lib/ssh/sftp-server /usr/libexec/sftp-server /usr/lib/sftp-server /usr/local/libexec/sftp-server; ` +
	`do if [ -x "$p" ]; then exec "$p"; fi; done; ` +
	`echo "gossh: sftp-server not found on the remote host" >&2; exit 127`

type connectOptions struct {
	sudo bool // run sftp-server through sudo (root access)
	// sshArgs are extra ssh flags; only tests use them, to reach a test server.
	sshArgs []string
}

// stderrTap shows ssh's stderr while connecting and keeps the tail of it.
// Once the session is up it goes quiet so ssh can't scribble over a TUI.
type stderrTap struct {
	mu     sync.Mutex
	quiet  bool
	buf    bytes.Buffer
	onText func(all string) // called with everything seen so far
}

func (t *stderrTap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.quiet {
		os.Stderr.Write(p)
	}
	t.buf.Write(p)
	if t.buf.Len() > 8192 {
		tail := t.buf.Bytes()[t.buf.Len()-4096:]
		t.buf = *bytes.NewBuffer(append([]byte(nil), tail...))
	}
	if t.onText != nil {
		t.onText(t.buf.String())
	}
	return len(p), nil
}

func (t *stderrTap) silence() {
	t.mu.Lock()
	t.quiet = true
	t.mu.Unlock()
}

func (t *stderrTap) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// sftpSession is a running "ssh host sftp" process plus its client.
type sftpSession struct {
	fs  *remoteFS
	cmd *exec.Cmd
}

func (s *sftpSession) Close() {
	s.fs.c.Close()
	s.cmd.Wait()
}

// connectSFTP opens an SFTP session through the system ssh client, so
// ~/.ssh/config, agents, ProxyJump and password prompts all work as usual.
// With sudo it first tries passwordless sudo and asks for the password only
// if the server requires one.
func connectSFTP(host string, opts connectOptions) (*sftpSession, error) {
	if !opts.sudo {
		return dialSFTP(host, opts, "")
	}
	s, err := dialSFTP(host, opts, "")
	if err == nil || !strings.Contains(err.Error(), "password is required") {
		return s, err
	}
	if !term.IsTerminal(os.Stdin.Fd()) {
		return nil, fmt.Errorf("%w; run gossh in a terminal to type it, or allow passwordless sudo", err)
	}
	var password string
	if err := huh.NewInput().
		Title(fmt.Sprintf("sudo password on %s", host)).
		EchoMode(huh.EchoModePassword).
		Value(&password).
		Run(); err != nil {
		return nil, fmt.Errorf("cancelled")
	}
	return dialSFTP(host, opts, password)
}

func dialSFTP(host string, opts connectOptions, sudoPassword string) (*sftpSession, error) {
	args := []string{
		"-T",
		"-o", "ClearAllForwardings=yes",
		"-o", "ForwardX11=no",
		"-o", "ForwardAgent=no",
		"-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none",
	}
	args = append(args, opts.sshArgs...)
	switch {
	case !opts.sudo:
		args = append(args, "-s", host, "sftp")
	case sudoPassword == "":
		args = append(args, host, "sudo -n sh -c '"+sftpServerScript+"'")
	default:
		args = append(args, host, "sudo -S -p '' sh -c '"+sftpServerScript+"'")
	}

	cmd := exec.Command(sshBinary(), args...)
	tap := &stderrTap{}
	cmd.Stderr = tap
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	if sudoPassword != "" {
		// A wrong password makes sudo wait for another attempt on stdin,
		// which would hang the handshake; stop ssh as soon as it complains.
		tap.mu.Lock()
		tap.quiet = true
		tap.onText = func(all string) {
			if strings.Contains(all, "try again") || strings.Contains(all, "incorrect password") {
				cmd.Process.Kill()
			}
		}
		tap.mu.Unlock()
		if _, err := io.WriteString(stdin, sudoPassword+"\n"); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return nil, err
		}
	}

	client, err := sftp.NewClientPipe(stdout, stdin, sftp.UseConcurrentWrites(true))
	if err != nil {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, connectError(host, tap.String())
	}
	tap.silence()

	home, err := client.Getwd()
	if err != nil {
		home = "/"
	}
	return &sftpSession{fs: &remoteFS{c: client, host: host, home: home}, cmd: cmd}, nil
}

// connectError turns ssh/sudo stderr into a short explanation.
func connectError(host, stderr string) error {
	s := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(s, "password is required"):
		return fmt.Errorf("sudo on %s: a password is required", host)
	case strings.Contains(s, "try again"), strings.Contains(s, "incorrect password"):
		return fmt.Errorf("wrong sudo password for %s", host)
	case strings.Contains(s, "must have a tty"):
		return fmt.Errorf("sudo on %s requires a tty (sudoers 'requiretty'); sudo transfers need it disabled", host)
	case strings.Contains(s, "not in the sudoers"), strings.Contains(s, "may not run sudo"):
		return fmt.Errorf("your user may not use sudo on %s", host)
	case strings.Contains(s, "subsystem request failed"):
		return fmt.Errorf("%s does not have SFTP enabled (sshd Subsystem sftp)", host)
	case s == "":
		return fmt.Errorf("could not start an SFTP session with %s", host)
	}
	lines := strings.Split(s, "\n")
	return fmt.Errorf("could not connect to %s: %s", host, strings.TrimSpace(lines[len(lines)-1]))
}
