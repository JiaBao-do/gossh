package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// runCopy copies src to dst with opts and returns the finished job.
func runCopy(t *testing.T, fsys fileSystem, src, dst string, opts copyOptions) *copyJob {
	t.Helper()
	job := newCopyJob(localFS{}, fsys, opts)
	if err := job.add(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := job.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return job
}

// noPartFiles fails if a temporary upload file was left behind in dir.
func noPartFiles(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".part") {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
}

func TestCopyJobRecursive(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "app", "a.txt"), "hello")
	writeFile(t, filepath.Join(src, "app", "sub", "b.txt"), "world!")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "app", "a.txt"), old, old)

	job := runCopy(t, localFS{}, filepath.Join(src, "app"), filepath.Join(dst, "app"),
		copyOptions{recursive: true, preserveTimes: true})

	if got := readFile(t, filepath.Join(dst, "app", "a.txt")); got != "hello" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "app", "sub", "b.txt")); got != "world!" {
		t.Errorf("b.txt = %q", got)
	}
	if info, _ := os.Stat(filepath.Join(dst, "app", "a.txt")); !info.ModTime().Equal(old) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), old)
	}
	if got := job.prog.done.Load(); got != 11 {
		t.Errorf("progress done = %d, want 11", got)
	}
	if f, n := job.prog.files.Load(), job.prog.fileCount.Load(); f != 2 || n != 2 {
		t.Errorf("files = %d/%d, want 2/2", f, n)
	}
	noPartFiles(t, dst)
}

func TestCopyJobDirNeedsRecursive(t *testing.T) {
	job := newCopyJob(localFS{}, localFS{}, copyOptions{})
	if err := job.add(t.TempDir(), filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("copying a folder without recursive should fail")
	}
}

func TestConflictActions(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "new")
	newTime := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(src, "f.txt"), newTime, newTime)

	tests := []struct {
		action   conflictAction
		dstAge   time.Duration // how much older the existing file is than the source
		want     string        // content of f.txt afterwards
		wantCopy string        // content of "f (1).txt", "" if it must not exist
	}{
		{conflictOverwrite, 0, "new", ""},
		{conflictSkip, time.Hour, "old", ""},
		{conflictNewer, time.Hour, "new", ""},     // source is newer: replaced
		{conflictNewer, -time.Hour, "old", ""},    // existing is newer: kept
		{conflictNewer, 0, "old", ""},             // same time: kept
		{conflictRename, time.Hour, "old", "new"}, // keep both
	}
	for _, tt := range tests {
		dst := t.TempDir()
		target := filepath.Join(dst, "f.txt")
		writeFile(t, target, "old")
		dstTime := newTime.Add(-tt.dstAge)
		os.Chtimes(target, dstTime, dstTime)

		asked := 0
		ask := func(c conflict) conflictAction {
			asked++
			if c.dst != target || c.dstInfo.Size() != 3 {
				t.Errorf("conflict = %+v", c)
			}
			return tt.action
		}
		runCopy(t, localFS{}, filepath.Join(src, "f.txt"), target, copyOptions{onConflict: conflictAsk, ask: ask})

		if asked != 1 {
			t.Errorf("action %d: asked %d times", tt.action, asked)
		}
		if got := readFile(t, target); got != tt.want {
			t.Errorf("action %d age %v: f.txt = %q, want %q", tt.action, tt.dstAge, got, tt.want)
		}
		copyPath := filepath.Join(dst, "f (1).txt")
		if tt.wantCopy == "" {
			if _, err := os.Stat(copyPath); err == nil {
				t.Errorf("action %d: unexpected %s", tt.action, copyPath)
			}
		} else if got := readFile(t, copyPath); got != tt.wantCopy {
			t.Errorf("action %d: copy = %q, want %q", tt.action, got, tt.wantCopy)
		}
		noPartFiles(t, dst)
	}
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.tar.gz"), "")
	writeFile(t, filepath.Join(dir, "a.tar (1).gz"), "")
	writeFile(t, filepath.Join(dir, ".env"), "")
	j := newCopyJob(localFS{}, localFS{}, copyOptions{})
	for in, want := range map[string]string{"a.tar.gz": "a.tar (2).gz", ".env": ".env (1)", "x": "x (1)"} {
		if got, _ := j.freeName(filepath.Join(dir, in)); got != filepath.Join(dir, want) {
			t.Errorf("freeName(%s) = %s, want %s", in, filepath.Base(got), want)
		}
	}
}

// cancelAfterFirstRead cancels the copy as soon as data starts flowing,
// simulating the user pressing esc (or the network dying) mid-file.
type cancelAfterFirstRead struct {
	localFS
	cancel context.CancelFunc
}

func (c cancelAfterFirstRead) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return readCloser{Reader: io.MultiReader(io.LimitReader(f, 1), cancelReader{c.cancel}, f), Closer: f}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

type cancelReader struct{ cancel context.CancelFunc }

func (c cancelReader) Read([]byte) (int, error) {
	c.cancel()
	return 0, io.EOF
}

func TestCancelledOverwriteKeepsOriginal(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "f"), strings.Repeat("x", 1<<20))
	writeFile(t, filepath.Join(dst, "f"), "original")

	ctx, cancel := context.WithCancel(context.Background())
	job := newCopyJob(cancelAfterFirstRead{cancel: cancel}, localFS{}, copyOptions{onConflict: conflictOverwrite})
	if err := job.add(filepath.Join(src, "f"), filepath.Join(dst, "f")); err != nil {
		t.Fatal(err)
	}
	if err := job.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := readFile(t, filepath.Join(dst, "f")); got != "original" {
		t.Errorf("existing file was damaged: %q", got[:min(len(got), 20)])
	}
	noPartFiles(t, dst)
}

// failOpen makes opening one particular file fail.
type failOpen struct {
	localFS
	bad string
}

func (f failOpen) Open(p string) (io.ReadCloser, error) {
	if filepath.Base(p) == f.bad {
		return nil, errors.New("simulated read error")
	}
	return os.Open(p)
}

func TestFailedFileDoesNotStopTheRest(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "d", "a"), "a")
	writeFile(t, filepath.Join(src, "d", "bad"), "b")
	writeFile(t, filepath.Join(src, "d", "c"), "c")

	job := newCopyJob(failOpen{bad: "bad"}, localFS{}, copyOptions{recursive: true})
	if err := job.add(filepath.Join(src, "d"), filepath.Join(dst, "d")); err != nil {
		t.Fatal(err)
	}
	if err := job.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fails := job.Failures()
	if len(fails) != 1 || filepath.Base(fails[0].path) != "bad" {
		t.Errorf("failures = %v", fails)
	}
	for _, n := range []string{"a", "c"} {
		if got := readFile(t, filepath.Join(dst, "d", n)); got != n {
			t.Errorf("%s = %q", n, got)
		}
	}
}

func TestListDirOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.txt"), "")
	writeFile(t, filepath.Join(dir, "A.txt"), "")
	writeFile(t, filepath.Join(dir, ".hidden"), "")
	os.Mkdir(filepath.Join(dir, "zdir"), 0755)

	entries, err := listDir(localFS{}, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"..", "zdir", "A.txt", "b.txt"}; !reflect.DeepEqual(entryNames(entries), want) {
		t.Errorf("names = %v, want %v", entryNames(entries), want)
	}
	if entries, _ := listDir(localFS{}, dir, true); len(entries) != 5 {
		t.Errorf("with hidden: %d entries, want 5", len(entries))
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 5 << 20: "5.0 MB"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRemoteAbs(t *testing.T) {
	r := &remoteFS{home: "/home/me"}
	for in, want := range map[string]string{"": "/home/me", "~": "/home/me", "~/x": "/home/me/x", "x/y": "/home/me/x/y", "/etc/": "/etc"} {
		if got, _ := r.Abs(in); got != want {
			t.Errorf("Abs(%q) = %q, want %q", in, got, want)
		}
	}
}

// pipeRemote connects a remoteFS to an in-process SFTP server serving the
// local disk, exercising the same client code paths as a real host.
func pipeRemote(t *testing.T) *remoteFS {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	server, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw})
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	client, err := sftp.NewClientPipe(cr, cw, sftp.UseConcurrentWrites(true), sftp.MaxPacket(1<<15))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sw.Close() // ends the client's reader so Close can return
		client.Close()
		server.Close()
	})
	return &remoteFS{c: client, host: "test", home: "/"}
}

func TestCopyJobOverSFTP(t *testing.T) {
	remote := pipeRemote(t)
	local, srv := t.TempDir(), t.TempDir()
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB: many concurrent packets
	writeFile(t, filepath.Join(local, "up", "big.bin"), big)
	writeFile(t, filepath.Join(local, "up", "nested", "small.txt"), "hi")
	writeFile(t, filepath.Join(srv, "up", "nested", "small.txt"), "old remote")

	// Upload, overwriting the existing remote file through the temp-file rename.
	runCopy(t, remote, filepath.Join(local, "up"), filepath.ToSlash(filepath.Join(srv, "up")),
		copyOptions{recursive: true, preserveTimes: true, onConflict: conflictOverwrite})
	if got := readFile(t, filepath.Join(srv, "up", "big.bin")); got != big {
		t.Fatalf("uploaded big.bin differs (%d bytes)", len(got))
	}
	if got := readFile(t, filepath.Join(srv, "up", "nested", "small.txt")); got != "hi" {
		t.Fatalf("remote small.txt = %q, want overwritten", got)
	}
	noPartFiles(t, srv)

	down := newCopyJob(remote, localFS{}, copyOptions{recursive: true})
	back := filepath.Join(local, "back")
	if err := down.add(filepath.ToSlash(filepath.Join(srv, "up")), back); err != nil {
		t.Fatal(err)
	}
	if err := down.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(back, "big.bin")); got != big {
		t.Fatalf("downloaded big.bin differs (%d bytes)", len(got))
	}
	if d, total := down.prog.done.Load(), down.prog.total.Load(); d != total || total != int64(len(big)+2) {
		t.Errorf("download progress %d/%d", d, total)
	}
}
