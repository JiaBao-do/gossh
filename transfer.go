package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
)

// --- Progress ---

// progress is updated by a running copyJob and read by whoever shows it.
type progress struct {
	total     atomic.Int64 // bytes
	done      atomic.Int64
	files     atomic.Int64 // files finished
	fileCount atomic.Int64 // files planned

	mu      sync.Mutex
	current string
}

func (p *progress) setCurrent(s string) {
	p.mu.Lock()
	p.current = s
	p.mu.Unlock()
}

func (p *progress) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// countingWriter / countingReader feed the progress counters and stop the
// copy once ctx is cancelled.
type countingWriter struct {
	ctx  context.Context
	w    io.Writer
	prog *progress
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	c.prog.done.Add(int64(n))
	return n, err
}

type countingReader struct {
	ctx    context.Context
	r      io.Reader
	prog   *progress
	remain int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(b)
	c.prog.done.Add(int64(n))
	c.remain -= int64(n)
	return n, err
}

// Size lets sftp.File.ReadFrom see how much is left so it uploads concurrently.
func (c *countingReader) Size() int64 { return c.remain }

// --- Copy engine ---

type copyOptions struct {
	recursive     bool // copy directories
	preserveTimes bool
	preserveMode  bool
}

type copyItem struct {
	src, dst string
	info     os.FileInfo
}

// copyJob copies files and directory trees from one file system to another.
// Call add for every source (this only plans), then run.
type copyJob struct {
	src, dst fileSystem
	opts     copyOptions
	prog     *progress
	items    []copyItem
	skipped  []string // symlinked directories, broken links, devices, sockets...
}

func newCopyJob(src, dst fileSystem, opts copyOptions) *copyJob {
	return &copyJob{src: src, dst: dst, opts: opts, prog: &progress{}}
}

// add plans copying srcPath to exactly dstPath (not into it).
func (j *copyJob) add(srcPath, dstPath string) error {
	j.prog.setCurrent("scanning " + srcPath)
	info, err := j.src.Stat(srcPath)
	if err != nil {
		return err
	}
	return j.plan(srcPath, dstPath, info)
}

func (j *copyJob) plan(srcPath, dstPath string, info os.FileInfo) error {
	switch {
	case info.Mode().IsRegular():
		j.items = append(j.items, copyItem{srcPath, dstPath, info})
		j.prog.total.Add(info.Size())
		j.prog.fileCount.Add(1)
		return nil
	case !info.IsDir():
		j.skipped = append(j.skipped, srcPath)
		return nil
	case !j.opts.recursive:
		return fmt.Errorf("%s is a directory (use -r to copy folders)", srcPath)
	}

	j.items = append(j.items, copyItem{srcPath, dstPath, info})
	children, err := j.src.ReadDir(srcPath)
	if err != nil {
		return err
	}
	for _, c := range children {
		childSrc := j.src.Join(srcPath, c.Name())
		childDst := j.dst.Join(dstPath, c.Name())
		if c.Mode()&os.ModeSymlink != 0 {
			// Follow links to files; skip links to directories so a loop
			// can't recurse forever.
			target, err := j.src.Stat(childSrc)
			if err != nil || target.IsDir() {
				j.skipped = append(j.skipped, childSrc)
				continue
			}
			c = target
		}
		if err := j.plan(childSrc, childDst, c); err != nil {
			return err
		}
	}
	return nil
}

func (j *copyJob) run(ctx context.Context) error {
	var dirs []copyItem
	for _, it := range j.items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if it.info.IsDir() {
			if err := j.dst.MkdirAll(it.dst); err != nil {
				return fmt.Errorf("creating %s: %w", it.dst, err)
			}
			dirs = append(dirs, it)
			continue
		}
		if err := j.copyFile(ctx, it); err != nil {
			return err
		}
	}
	j.prog.setCurrent("")

	// Directory metadata goes last: writing files into a directory changes its mtime.
	for i := len(dirs) - 1; i >= 0; i-- {
		j.applyMeta(dirs[i])
	}
	return nil
}

func (j *copyJob) copyFile(ctx context.Context, it copyItem) error {
	j.prog.setCurrent(it.src)
	r, err := j.src.Open(it.src)
	if err != nil {
		return err
	}
	defer r.Close()

	w, err := j.dst.Create(it.dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", it.dst, err)
	}
	err = j.stream(ctx, w, r, it.info.Size())
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		j.dst.RemoveAll(it.dst) // don't leave a truncated file behind
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("copying %s: %w", it.src, err)
	}
	j.applyMeta(it)
	j.prog.files.Add(1)
	return nil
}

// stream copies r to w, letting the SFTP side drive so reads/writes are
// pipelined (a big speed-up on high latency links).
func (j *copyJob) stream(ctx context.Context, w io.Writer, r io.Reader, size int64) error {
	cw := &countingWriter{ctx: ctx, w: w, prog: j.prog}
	if wt, ok := r.(io.WriterTo); ok && j.src.Remote() {
		_, err := wt.WriteTo(cw)
		return err
	}
	cr := &countingReader{ctx: ctx, r: r, prog: j.prog, remain: size}
	if rf, ok := w.(io.ReaderFrom); ok && j.dst.Remote() {
		_, err := rf.ReadFrom(cr)
		return err
	}
	_, err := io.Copy(w, cr)
	return err
}

// applyMeta copies permissions and modification time when asked to.
// Failures are ignored, like scp: the data itself made it across.
func (j *copyJob) applyMeta(it copyItem) {
	if j.opts.preserveMode && j.src.ModesMeaningful() {
		j.dst.Chmod(it.dst, it.info.Mode().Perm())
	}
	if j.opts.preserveTimes {
		j.dst.Chtimes(it.dst, it.info.ModTime())
	}
}

// --- Formatting ---

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func formatRate(bytes int64, elapsed time.Duration) string {
	if elapsed < time.Second/10 {
		return "-"
	}
	return formatSize(int64(float64(bytes)/elapsed.Seconds())) + "/s"
}

func percent(done, total int64) int {
	if total <= 0 {
		return 100
	}
	return int(done * 100 / total)
}

// --- gossh cp ---

// location is one gossh cp argument: a local path or host:path.
type location struct {
	host string // "" for local
	path string
}

func parseLocation(s string) location {
	return parseLocationFor(s, runtime.GOOS == "windows")
}

// parseLocationFor follows scp: "host:path" is remote when the colon comes
// before any slash. "[::1]:path" and "user@[::1]:path" are IPv6 hosts and,
// on Windows, "C:\..." is a drive letter.
func parseLocationFor(s string, windows bool) location {
	if open := strings.Index(s, "["); open >= 0 && (open == 0 || s[open-1] == '@') {
		if end := strings.Index(s, "]:"); end > open {
			return location{host: s[:open] + s[open+1:end], path: s[end+2:]}
		}
	}
	colon := strings.Index(s, ":")
	if colon <= 0 || strings.ContainsAny(s[:colon], `/\`) {
		return location{path: s}
	}
	if windows && colon == 1 {
		return location{path: s}
	}
	return location{host: s[:colon], path: s[colon+1:]}
}

type cpArgs struct {
	recursive bool
	preserve  bool
	quiet     bool
	sudo      bool
	sshArgs   []string
	paths     []string
}

// parseCpArgs accepts scp's common flags: -r -p -q (combinable, e.g. -rp),
// -P port, -i key, -J jump, -o option, plus --sudo.
func parseCpArgs(args []string) (cpArgs, error) {
	var a cpArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			a.paths = append(a.paths, args[i+1:]...)
			i = len(args)
		case arg == "--sudo":
			a.sudo = true
		case arg == "-P" || arg == "-i" || arg == "-J" || arg == "-o":
			if i+1 >= len(args) {
				return a, fmt.Errorf("%s needs a value", arg)
			}
			i++
			flag := arg
			if arg == "-P" {
				flag = "-p" // scp's -P is ssh's -p
			}
			a.sshArgs = append(a.sshArgs, flag, args[i])
		case len(arg) > 1 && arg[0] == '-':
			for _, c := range arg[1:] {
				switch c {
				case 'r', 'R':
					a.recursive = true
				case 'p':
					a.preserve = true
				case 'q':
					a.quiet = true
				default:
					return a, fmt.Errorf("unknown option -%c", c)
				}
			}
		default:
			a.paths = append(a.paths, arg)
		}
	}
	if len(a.paths) < 2 {
		return a, fmt.Errorf("need a source and a destination")
	}
	return a, nil
}

func printCpUsage() {
	fmt.Println("Usage: gossh cp [-r] [-p] [-q] [--sudo] [-P port] [-i key] [-J jump] SRC... DST")
	fmt.Println()
	fmt.Println("  gossh cp prod:/var/log/app.log .        download a file")
	fmt.Println("  gossh cp -r ./dist prod:/srv/app        upload a folder")
	fmt.Println(`  gossh cp "prod:/var/log/*.log" logs/    download matching files`)
	fmt.Println("  gossh cp --sudo nginx.conf prod:/etc/nginx/   write where only root can")
}

func cpCommand(args []string) {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printCpUsage()
		return
	}
	a, err := parseCpArgs(args)
	if err != nil {
		fmt.Println("Error:", err)
		fmt.Println()
		printCpUsage()
		os.Exit(1)
	}
	if err := runCp(a); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}

// cpHost works out which host is involved and in which direction.
func cpHost(srcs []location, dst location) (host string, download bool, err error) {
	for _, s := range srcs {
		if (s.host != "") != (srcs[0].host != "") {
			return "", false, fmt.Errorf("sources mix local and remote paths")
		}
		if s.host != srcs[0].host {
			return "", false, fmt.Errorf("all sources must be on the same host")
		}
	}
	download = srcs[0].host != ""
	switch {
	case download && dst.host != "":
		return "", false, fmt.Errorf("both sides are remote; one side must be a local path")
	case !download && dst.host == "":
		return "", false, fmt.Errorf("neither side is remote; write the remote side as host:path (e.g. prod:/tmp)")
	case download:
		return srcs[0].host, true, nil
	}
	return dst.host, false, nil
}

func runCp(a cpArgs) error {
	srcs := make([]location, len(a.paths)-1)
	for i, p := range a.paths[:len(a.paths)-1] {
		srcs[i] = parseLocation(p)
	}
	dst := parseLocation(a.paths[len(a.paths)-1])
	host, download, err := cpHost(srcs, dst)
	if err != nil {
		return err
	}

	sess, err := connectSFTP(host, connectOptions{sshArgs: a.sshArgs, sudo: a.sudo})
	if err != nil {
		return err
	}
	defer sess.Close()

	var srcFS, dstFS fileSystem = localFS{}, sess.fs
	if download {
		srcFS, dstFS = sess.fs, localFS{}
	}
	srcPaths, err := expandSources(srcFS, srcs)
	if err != nil {
		return err
	}
	dstPath, err := dstFS.Abs(dst.path)
	if err != nil {
		return err
	}
	wantDir := strings.HasSuffix(dst.path, "/") || (!dstFS.Remote() && strings.HasSuffix(dst.path, `\`))
	targets, err := cpTargets(srcFS, dstFS, srcPaths, dstPath, wantDir)
	if err != nil {
		return err
	}

	job := newCopyJob(srcFS, dstFS, copyOptions{
		recursive:     a.recursive,
		preserveTimes: a.preserve,
		preserveMode:  a.preserve,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	stopProgress := func() {}
	if !a.quiet && term.IsTerminal(os.Stdout.Fd()) {
		stopProgress = showProgress(job.prog, start)
	}
	err = func() error {
		for _, t := range targets {
			if err := job.add(t[0], t[1]); err != nil {
				return err
			}
		}
		return job.run(ctx)
	}()
	stopProgress()

	for _, s := range job.skipped {
		fmt.Printf("skipped %s (not a regular file, or a link to a directory)\n", s)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("cancelled; a partially written file may remain on %s", dstFS.Label())
		}
		return err
	}
	if !a.quiet {
		fmt.Printf("%d file(s), %s in %s (%s)\n",
			job.prog.files.Load(), formatSize(job.prog.done.Load()),
			time.Since(start).Round(time.Millisecond), formatRate(job.prog.done.Load(), time.Since(start)))
	}
	return nil
}

func hasGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// expandSources resolves each source to absolute paths, expanding wildcards
// (so they work on remote paths and in shells that don't expand them).
func expandSources(fsys fileSystem, locs []location) ([]string, error) {
	var out []string
	for _, l := range locs {
		p, err := fsys.Abs(l.path)
		if err != nil {
			return nil, err
		}
		if _, statErr := fsys.Stat(p); statErr == nil || !hasGlob(l.path) {
			out = append(out, p)
			continue
		}
		matches, err := fsys.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", l.path, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%s: no matches on %s", l.path, fsys.Label())
		}
		out = append(out, matches...)
	}
	return out, nil
}

// cpTargets follows cp/scp: sources go into dstPath if it is a directory,
// otherwise a single source is copied to dstPath itself. A trailing slash
// on the destination creates the directory when it is missing.
func cpTargets(srcFS, dstFS fileSystem, srcPaths []string, dstPath string, wantDir bool) ([][2]string, error) {
	info, err := dstFS.Stat(dstPath)
	exists := err == nil
	isDir := exists && info.IsDir()
	if !isDir {
		if exists && (wantDir || len(srcPaths) > 1) {
			return nil, fmt.Errorf("%s is not a directory", dstPath)
		}
		if len(srcPaths) > 1 && !wantDir {
			return nil, fmt.Errorf("%s does not exist (add a trailing / to create it)", dstPath)
		}
		if wantDir {
			if err := dstFS.MkdirAll(dstPath); err != nil {
				return nil, fmt.Errorf("creating %s: %w", dstPath, err)
			}
			isDir = true
		}
	}

	targets := make([][2]string, len(srcPaths))
	for i, s := range srcPaths {
		targets[i] = [2]string{s, dstPath}
		if isDir {
			targets[i][1] = dstFS.Join(dstPath, srcFS.Base(s))
		}
	}
	return targets, nil
}

// showProgress redraws a one-line progress report until the returned func is called.
func showProgress(p *progress, start time.Time) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	draw := func() {
		width := 80
		if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 20 {
			width = w
		}
		d, t := p.done.Load(), p.total.Load()
		stats := fmt.Sprintf(" %3d%%  %s / %s  %s  [%d/%d]",
			percent(d, t), formatSize(d), formatSize(t),
			formatRate(d, time.Since(start)), p.files.Load(), p.fileCount.Load())
		name := p.Current()
		if room := width - len(stats) - 1; len(name) > room {
			if room < 4 {
				name = ""
			} else {
				name = "…" + name[len(name)-room+1:]
			}
		}
		line := name + stats
		fmt.Printf("\r%-*s", width-1, line)
	}
	go func() {
		defer close(finished)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				draw()
				fmt.Println()
				return
			case <-tick.C:
				draw()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}
