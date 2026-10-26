package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
)

// --- Progress ---

// progress is updated by a running copyJob and read by whoever shows it.
type progress struct {
	total     atomic.Int64 // bytes
	done      atomic.Int64
	files     atomic.Int64 // files copied
	fileCount atomic.Int64 // files still planned or copied (skips are removed)
	kept      atomic.Int64 // existing files left alone (skip / not newer)
	renamed   atomic.Int64 // copies saved under a new name

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

// --- Conflicts ---

// conflictAction says what to do when the destination file already exists.
type conflictAction int

const (
	conflictAsk conflictAction = iota // call copyOptions.ask
	conflictOverwrite
	conflictNewer // overwrite only if the source is newer
	conflictSkip
	conflictRename // keep both: save as "name (1).ext"
)

type conflict struct {
	src, dst         string
	srcInfo, dstInfo os.FileInfo
}

// newerThan compares at one second resolution, which is all SFTP keeps.
func newerThan(a, b time.Time) bool {
	return a.Truncate(time.Second).After(b.Truncate(time.Second))
}

// --- Copy engine ---

type copyOptions struct {
	recursive     bool // copy directories
	preserveTimes bool
	preserveMode  bool
	onConflict    conflictAction
	// ask decides a conflict when onConflict is conflictAsk. It is called
	// from the copying goroutine and may block.
	ask func(conflict) conflictAction
}

type copyItem struct {
	src, dst string
	info     os.FileInfo
}

type failure struct {
	path string
	err  error
}

// copyJob copies files and directory trees from one file system to another.
// Call add for every source (this only plans), then run. A file that fails
// is recorded and the rest continue; only cancellation or a lost connection
// stops the job.
type copyJob struct {
	src, dst fileSystem
	opts     copyOptions
	prog     *progress
	items    []copyItem

	mu       sync.Mutex
	skipped  []string // symlinked directories, broken links, devices, sockets...
	failures []failure
}

func newCopyJob(src, dst fileSystem, opts copyOptions) *copyJob {
	return &copyJob{src: src, dst: dst, opts: opts, prog: &progress{}}
}

func (j *copyJob) noteSkipped(p string) {
	j.mu.Lock()
	j.skipped = append(j.skipped, p)
	j.mu.Unlock()
}

func (j *copyJob) fail(p string, err error) {
	j.mu.Lock()
	j.failures = append(j.failures, failure{p, err})
	j.mu.Unlock()
}

func (j *copyJob) Skipped() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.skipped...)
}

func (j *copyJob) Failures() []failure {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]failure(nil), j.failures...)
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
		j.noteSkipped(srcPath)
		return nil
	case !j.opts.recursive:
		return fmt.Errorf("%s is a directory", srcPath)
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
				j.noteSkipped(childSrc)
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

// fatal reports errors that make carrying on pointless.
func fatal(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		errors.Is(err, sftp.ErrSSHFxNoConnection)
}

// under reports whether p is inside dir.
func under(p, dir string) bool {
	return len(p) > len(dir) && strings.HasPrefix(p, dir) && (p[len(dir)] == '/' || p[len(dir)] == '\\')
}

func (j *copyJob) run(ctx context.Context) error {
	var dirs []copyItem
	var failedDirs []string
	for _, it := range j.items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(failedDirs) > 0 && under(it.src, failedDirs[len(failedDirs)-1]) {
			// Its folder could not be created; that failure is already listed.
			if !it.info.IsDir() {
				j.forget(it)
			}
			continue
		}
		var err error
		if it.info.IsDir() {
			if err = j.dst.MkdirAll(it.dst); err == nil {
				dirs = append(dirs, it)
			} else {
				err = fmt.Errorf("creating folder %s: %w", it.dst, err)
				failedDirs = append(failedDirs, it.src)
			}
		} else {
			err = j.copyFile(ctx, it)
		}
		if err != nil {
			if fatal(ctx, err) {
				return err
			}
			j.fail(it.src, err)
		}
	}
	j.prog.setCurrent("")

	// Directory metadata goes last: writing files into a directory changes its mtime.
	for i := len(dirs) - 1; i >= 0; i-- {
		j.applyMeta(dirs[i], dirs[i].dst)
	}
	return nil
}

// forget takes a file that won't be copied out of the totals.
func (j *copyJob) forget(it copyItem) {
	j.prog.total.Add(-it.info.Size())
	j.prog.fileCount.Add(-1)
}

func (j *copyJob) copyFile(ctx context.Context, it copyItem) error {
	j.prog.setCurrent(it.src)
	dst := it.dst
	existing, err := j.dst.Stat(dst)
	if err == nil {
		if existing.IsDir() {
			return fmt.Errorf("%s is a folder on %s", dst, j.dst.Label())
		}
		action := j.opts.onConflict
		if action == conflictAsk {
			action = conflictOverwrite
			if j.opts.ask != nil {
				action = j.opts.ask(conflict{src: it.src, dst: dst, srcInfo: it.info, dstInfo: existing})
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		switch {
		case action == conflictSkip,
			action == conflictNewer && !newerThan(it.info.ModTime(), existing.ModTime()):
			j.forget(it)
			j.prog.kept.Add(1)
			return nil
		case action == conflictRename:
			if dst, err = j.freeName(dst); err != nil {
				return err
			}
			existing = nil
			j.prog.renamed.Add(1)
		}
	} else {
		existing = nil
	}
	if err := j.writeFile(ctx, it, dst, existing); err != nil {
		return err
	}
	j.prog.files.Add(1)
	return nil
}

// freeName finds "name (1).ext", "name (2).ext", ... that doesn't exist yet.
func (j *copyJob) freeName(p string) (string, error) {
	dir, base := j.dst.Dir(p), j.dst.Base(p)
	stem, ext := base, ""
	if i := strings.LastIndex(base, "."); i > 0 {
		stem, ext = base[:i], base[i:]
	}
	for n := 1; n < 10000; n++ {
		candidate := j.dst.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		if _, err := j.dst.Stat(candidate); err != nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for %s", p)
}

// writeFile copies into a hidden temporary file next to dst and renames it
// into place, so an interrupted copy never damages an existing file. If the
// folder doesn't allow creating files, it falls back to writing dst directly.
func (j *copyJob) writeFile(ctx context.Context, it copyItem, dst string, existing os.FileInfo) error {
	r, err := j.src.Open(it.src)
	if err != nil {
		return err
	}
	defer r.Close()

	tmp := j.dst.Join(j.dst.Dir(dst), "."+j.dst.Base(dst)+".gossh-"+strconv.FormatUint(uint64(rand.Uint32()), 36)+".part")
	target := tmp
	w, err := j.dst.Create(tmp)
	if err != nil {
		target = dst
		if w, err = j.dst.Create(dst); err != nil {
			return fmt.Errorf("creating %s: %w", dst, err)
		}
	}

	err = j.stream(ctx, w, r, it.info.Size())
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		j.dst.RemoveAll(target) // don't leave a partial file behind
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("copying %s: %w", it.src, err)
	}

	if existing != nil && target == tmp && j.dst.ModesMeaningful() {
		// Replacing a file keeps its permissions unless asked to copy them.
		j.dst.Chmod(tmp, existing.Mode().Perm())
	}
	j.applyMeta(it, target)
	if target == tmp {
		if err := j.replace(tmp, dst, existing != nil); err != nil {
			j.dst.RemoveAll(tmp)
			return fmt.Errorf("replacing %s: %w", dst, err)
		}
	}
	return nil
}

// replace renames tmp over dst. Servers without the posix-rename extension
// refuse to rename onto an existing file, so remove it first in that case.
func (j *copyJob) replace(tmp, dst string, exists bool) error {
	err := j.dst.Rename(tmp, dst)
	if err == nil || !exists {
		return err
	}
	if rmErr := j.dst.RemoveAll(dst); rmErr != nil {
		return err
	}
	return j.dst.Rename(tmp, dst)
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
func (j *copyJob) applyMeta(it copyItem, p string) {
	if j.opts.preserveMode && j.src.ModesMeaningful() {
		j.dst.Chmod(p, it.info.Mode().Perm())
	}
	if j.opts.preserveTimes {
		j.dst.Chtimes(p, it.info.ModTime())
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
