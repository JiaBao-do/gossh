package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func TestParseLocation(t *testing.T) {
	tests := []struct {
		in      string
		windows bool
		want    location
	}{
		{"prod:/var/log", false, location{"prod", "/var/log"}},
		{"prod:", false, location{"prod", ""}},
		{"deploy@10.0.0.5:app.log", false, location{"deploy@10.0.0.5", "app.log"}},
		{"[::1]:/tmp/x", false, location{"::1", "/tmp/x"}},
		{"me@[fe80::1]:x", false, location{"me@fe80::1", "x"}},
		{"./a:b", false, location{"", "./a:b"}},
		{"dir/a:b", false, location{"", "dir/a:b"}},
		{"file.txt", false, location{"", "file.txt"}},
		{":x", false, location{"", ":x"}},
		{`C:\Users\me\file`, true, location{"", `C:\Users\me\file`}},
		{"C:/Users/me", true, location{"", "C:/Users/me"}},
		{"c:/tmp", false, location{"c", "/tmp"}},
		{"prod:/tmp", true, location{"prod", "/tmp"}},
	}
	for _, tt := range tests {
		if got := parseLocationFor(tt.in, tt.windows); got != tt.want {
			t.Errorf("parseLocationFor(%q, %v) = %+v, want %+v", tt.in, tt.windows, got, tt.want)
		}
	}
}

func TestParseCpArgs(t *testing.T) {
	a, err := parseCpArgs([]string{"-rp", "-P", "2222", "--sudo", "-i", "k", "a", "b", "host:/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.recursive || !a.preserve || a.quiet || !a.sudo {
		t.Errorf("flags = %+v", a)
	}
	if want := []string{"-p", "2222", "-i", "k"}; !reflect.DeepEqual(a.sshArgs, want) {
		t.Errorf("sshArgs = %v, want %v", a.sshArgs, want)
	}
	if want := []string{"a", "b", "host:/x"}; !reflect.DeepEqual(a.paths, want) {
		t.Errorf("paths = %v, want %v", a.paths, want)
	}

	for _, bad := range [][]string{{"-x", "a", "b"}, {"a"}, {"-P"}} {
		if _, err := parseCpArgs(bad); err == nil {
			t.Errorf("parseCpArgs(%v) should fail", bad)
		}
	}
}

func TestCpHost(t *testing.T) {
	loc := parseLocationFor
	if h, down, err := cpHost([]location{loc("prod:/a", false)}, loc(".", false)); err != nil || h != "prod" || !down {
		t.Errorf("download: %q %v %v", h, down, err)
	}
	if h, down, err := cpHost([]location{loc("a", false), loc("b", false)}, loc("prod:/x", false)); err != nil || h != "prod" || down {
		t.Errorf("upload: %q %v %v", h, down, err)
	}
	bad := []struct {
		srcs []location
		dst  location
	}{
		{[]location{loc("a", false)}, loc("b", false)},
		{[]location{loc("p:/a", false)}, loc("q:/b", false)},
		{[]location{loc("p:/a", false), loc("q:/b", false)}, loc(".", false)},
		{[]location{loc("p:/a", false), loc("b", false)}, loc(".", false)},
	}
	for _, b := range bad {
		if _, _, err := cpHost(b.srcs, b.dst); err == nil {
			t.Errorf("cpHost(%v, %v) should fail", b.srcs, b.dst)
		}
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyJobRecursive(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "app", "a.txt"), "hello")
	writeFile(t, filepath.Join(src, "app", "sub", "b.txt"), "world!")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "app", "a.txt"), old, old)

	job := newCopyJob(localFS{}, localFS{}, copyOptions{recursive: true, preserveTimes: true})
	if err := job.add(filepath.Join(src, "app"), filepath.Join(dst, "app")); err != nil {
		t.Fatal(err)
	}
	if err := job.run(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"a.txt": "hello", filepath.Join("sub", "b.txt"): "world!"} {
		got, err := os.ReadFile(filepath.Join(dst, "app", name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
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
}

func TestCopyJobDirNeedsRecursive(t *testing.T) {
	src := t.TempDir()
	job := newCopyJob(localFS{}, localFS{}, copyOptions{})
	err := job.add(src, filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "-r") {
		t.Errorf("err = %v, want a hint about -r", err)
	}
}

func TestCopyJobCancel(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "f"), strings.Repeat("x", 1<<20))
	job := newCopyJob(localFS{}, localFS{}, copyOptions{})
	if err := job.add(filepath.Join(src, "f"), filepath.Join(dst, "f")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := job.run(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, err := os.Stat(filepath.Join(dst, "f")); !os.IsNotExist(err) {
		t.Errorf("partial file left behind: %v", err)
	}
}

func TestCpTargets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "existing.txt"), "x")
	fs := localFS{}

	got, err := cpTargets(fs, fs, []string{"/src/a.txt", "/src/b"}, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"/src/a.txt", filepath.Join(dir, "a.txt")}, {"/src/b", filepath.Join(dir, "b")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("into dir: %v, want %v", got, want)
	}

	renamed := filepath.Join(dir, "new.txt")
	if got, err := cpTargets(fs, fs, []string{"/src/a.txt"}, renamed, false); err != nil || got[0][1] != renamed {
		t.Errorf("single file rename: %v %v", got, err)
	}

	if _, err := cpTargets(fs, fs, []string{"a", "b"}, filepath.Join(dir, "missing"), false); err == nil {
		t.Error("several sources into a missing dir should fail without a trailing slash")
	}
	if _, err := cpTargets(fs, fs, []string{"a", "b"}, filepath.Join(dir, "existing.txt"), false); err == nil {
		t.Error("several sources into a file should fail")
	}

	created := filepath.Join(dir, "made")
	if got, err := cpTargets(fs, fs, []string{"/src/a"}, created, true); err != nil || got[0][1] != filepath.Join(created, "a") {
		t.Errorf("trailing slash: %v %v", got, err)
	}
}

func TestExpandSourcesGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.log"), "")
	writeFile(t, filepath.Join(dir, "b.log"), "")
	writeFile(t, filepath.Join(dir, "c.txt"), "")
	got, err := expandSources(localFS{}, []location{{path: filepath.Join(dir, "*.log")}})
	if err != nil || len(got) != 2 {
		t.Errorf("glob = %v, %v", got, err)
	}
	if _, err := expandSources(localFS{}, []location{{path: filepath.Join(dir, "*.none")}}); err == nil {
		t.Error("glob with no matches should fail")
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
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
	}
	if want := []string{"..", "zdir", "A.txt", "b.txt"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
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

// toSlash turns a local temp path into what the in-process server expects.
func toSlash(p string) string { return filepath.ToSlash(p) }

func TestCopyJobOverSFTP(t *testing.T) {
	remote := pipeRemote(t)
	local, srv := t.TempDir(), t.TempDir()
	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB: many concurrent packets
	writeFile(t, filepath.Join(local, "up", "big.bin"), big)
	writeFile(t, filepath.Join(local, "up", "nested", "small.txt"), "hi")

	up := newCopyJob(localFS{}, remote, copyOptions{recursive: true, preserveTimes: true})
	if err := up.add(filepath.Join(local, "up"), toSlash(filepath.Join(srv, "up"))); err != nil {
		t.Fatal(err)
	}
	if err := up.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(srv, "up", "big.bin")); string(got) != big {
		t.Fatalf("uploaded big.bin differs (%d bytes)", len(got))
	}

	down := newCopyJob(remote, localFS{}, copyOptions{recursive: true})
	back := filepath.Join(local, "back")
	if err := down.add(toSlash(filepath.Join(srv, "up")), back); err != nil {
		t.Fatal(err)
	}
	if err := down.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(back, "big.bin")); string(got) != big {
		t.Fatalf("downloaded big.bin differs (%d bytes)", len(got))
	}
	if got, _ := os.ReadFile(filepath.Join(back, "nested", "small.txt")); string(got) != "hi" {
		t.Fatalf("small.txt = %q", got)
	}
	if d, total := down.prog.done.Load(), down.prog.total.Load(); d != total || total != int64(len(big)+2) {
		t.Errorf("download progress %d/%d", d, total)
	}
}
