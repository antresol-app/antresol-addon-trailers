package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// stallAfter: a fetch whose files do not grow for this long (no first
	// byte, a stuck download or merge) is killed.
	stallAfter = 20 * time.Second
	// updateLimit bounds the tool's self-update.
	updateLimit = 2 * time.Minute
	// maxFileSize caps one trailer.
	maxFileSize = "400M"
)

// format is the tool's format choice: H.264 video up to height with AAC
// audio, merged into an MP4, else the best single MP4.
func format(height int) string {
	return "bv*[height<=" + strconv.Itoa(height) + "][vcodec^=avc1]+ba[ext=m4a]/b[ext=mp4]"
}

// YtDlp fetches with yt-dlp: Bundled is the binary the release ships, and
// the copy in ToolsDir (updated daily) is used while it works.
type YtDlp struct {
	Bundled, ToolsDir string
	// FFmpeg is the ffmpeg the tool merges with ("" = from PATH).
	FFmpeg func() string
	// Run runs a command (nil: cmd.Run; tests).
	Run func(*exec.Cmd) error
	// Stall overrides stallAfter (tests).
	Stall time.Duration

	cur atomic.Value // string: the binary fetches use now
	upd sync.Mutex
}

func (y *YtDlp) copyPath() string { return filepath.Join(y.ToolsDir, "yt-dlp") }

// Current is the binary fetches use now: the updated copy once it has
// answered --version, else the bundled one.
func (y *YtDlp) Current() string {
	if v, _ := y.cur.Load().(string); v != "" {
		return v
	}
	return y.Bundled
}

func (y *YtDlp) run(cmd *exec.Cmd) error {
	if y.Run != nil {
		return y.Run(cmd)
	}
	return cmd.Run()
}

// Fetch writes the video at url to dst, at the lowest priority, in a
// process group of its own (the standalone tool is a launcher whose child
// does the work, and ffmpeg its child in turn).
func (y *YtDlp) Fetch(ctx context.Context, url, dst string, maxHeight int) error {
	ff := ""
	if y.FFmpeg != nil {
		ff = y.FFmpeg()
	}
	if ff == "" {
		ff = "ffmpeg"
	}
	if p, err := exec.LookPath(ff); err == nil {
		ff = p
	}
	dir := filepath.Dir(dst)
	base := strings.TrimSuffix(filepath.Base(dst), filepath.Ext(dst))
	args := []string{"--ignore-config", "--no-playlist", "--no-progress", "--quiet", "--no-warnings",
		// the file's time is when it was fetched: the cache's clock
		"--no-mtime", "--no-part", "--socket-timeout", "30", "--max-filesize", maxFileSize,
		"-f", format(maxHeight), "--merge-output-format", "mp4", "--ffmpeg-location", ff,
		"-o", filepath.Join(dir, base+".%(ext)s"), "--", url}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := y.Stall
	if stall <= 0 {
		stall = stallAfter
	}
	stalled := watchGrowth(ctx, dir, stall, cancel)
	var stderr strings.Builder
	cmd := lowPriority(exec.CommandContext(ctx, y.Current(), args...))
	cmd.Stderr = &capped{w: &stderr, n: 4096}
	cmd.Dir = dir
	ownGroup(cmd)
	if err := y.run(cmd); err != nil {
		if stalled.Load() {
			return fmt.Errorf("yt-dlp: no progress for %s", stall)
		}
		return fmt.Errorf("yt-dlp: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out := filepath.Join(dir, base+".mp4"); out != dst {
		return os.Rename(out, dst)
	}
	return nil
}

// watchGrowth cancels (and sets the flag) once the files in dir have not
// grown for stall, until ctx ends.
func watchGrowth(ctx context.Context, dir string, stall time.Duration, cancel func()) *atomic.Bool {
	var stalled atomic.Bool
	go func() {
		tick := time.NewTicker(min(stall/4, 2*time.Second))
		defer tick.Stop()
		last, since := int64(-1), time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if n := dirSize(dir); n != last {
				last, since = n, time.Now()
			} else if time.Since(since) >= stall {
				stalled.Store(true)
				cancel()
				return
			}
		}
	}()
	return &stalled
}

func dirSize(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			n += info.Size()
		}
	}
	return n
}

// Loop updates the tool at start and daily until ctx ends.
func (y *YtDlp) Loop(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		y.Update(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Update refreshes the copy in ToolsDir (sites break old versions often):
// made from the bundled one when missing or older, then yt-dlp -U; fetches
// use it while it answers --version, else the bundled one.
func (y *YtDlp) Update(ctx context.Context) {
	y.upd.Lock()
	defer y.upd.Unlock()
	cp := y.copyPath()
	if err := os.MkdirAll(y.ToolsDir, 0o755); err != nil {
		log.Printf("trailers: tools dir: %v", err)
		return
	}
	bundledV, copyV := y.version(ctx, y.Bundled), y.version(ctx, cp)
	// versions are dates (2026.10.26): a later one sorts after
	if copyV == "" || (bundledV != "" && bundledV > copyV) {
		if err := copyFile(y.Bundled, cp); err != nil {
			log.Printf("trailers: copy yt-dlp: %v", err)
			y.cur.Store(y.Bundled)
			return
		}
	}
	uctx, cancel := context.WithTimeout(ctx, updateLimit)
	var out strings.Builder
	cmd := lowPriority(exec.CommandContext(uctx, cp, "-U"))
	w := &capped{w: &out, n: 4096}
	cmd.Stdout, cmd.Stderr = w, w
	if err := y.run(cmd); err != nil {
		log.Printf("trailers: yt-dlp -U: %v: %s", err, strings.TrimSpace(out.String()))
	}
	cancel()
	if y.version(ctx, cp) != "" {
		y.cur.Store(cp)
		return
	}
	// a broken update: the bundled one now, a fresh copy next time
	os.Remove(cp)
	y.cur.Store(y.Bundled)
}

// version is what tool --version prints ("" when it does not run).
func (y *YtDlp) version(ctx context.Context, tool string) string {
	if st, err := os.Stat(tool); err != nil || st.IsDir() {
		return ""
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out strings.Builder
	cmd := exec.CommandContext(vctx, tool, "--version")
	cmd.Stdout = &capped{w: &out, n: 256}
	if err := y.run(cmd); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// lowPriority runs cmd under nice -n 19 and ionice -c 3 where they exist.
func lowPriority(cmd *exec.Cmd) *exec.Cmd {
	args := cmd.Args
	if nice, err := exec.LookPath("nice"); err == nil {
		args = append([]string{nice, "-n", "19"}, args...)
	}
	if ionice, err := exec.LookPath("ionice"); err == nil {
		args = append([]string{ionice, "-c", "3"}, args...)
	}
	if len(args) != len(cmd.Args) {
		cmd.Path, cmd.Args = args[0], args
	}
	return cmd
}

// ownGroup puts cmd in a process group of its own and kills the whole
// group when its context ends.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
}

// copyFile copies the executable from to to, through a temp name.
func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	tmp := to + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, to)
}

// capped keeps the first n bytes written to it.
type capped struct {
	w interface{ Write([]byte) (int, error) }
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n > 0 {
		k := min(len(p), c.n)
		c.w.Write(p[:k])
		c.n -= k
	}
	return len(p), nil
}
