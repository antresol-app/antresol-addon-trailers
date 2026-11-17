package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTool answers like yt-dlp: --version prints versions[tool], -U sets
// the copy's version to updated, a fetch writes an MP4 where -o says.
type fakeTool struct {
	mu       sync.Mutex
	versions map[string]string
	updated  string
	calls    [][]string
}

func (f *fakeTool) run(cmd *exec.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := cmd.Args
	f.calls = append(f.calls, args)
	tool := ""
	for _, a := range args { // nice and ionice wrap it: find it
		if _, ok := f.versions[a]; ok {
			tool = a
		}
	}
	switch {
	case slices.Contains(args, "--version"):
		v := f.versions[tool]
		if v == "" {
			return errors.New("exec format error")
		}
		cmd.Stdout.Write([]byte(v + "\n"))
		return nil
	case slices.Contains(args, "-U"):
		f.versions[tool] = f.updated
		return nil
	}
	i := slices.Index(args, "-o")
	out := strings.Replace(args[i+1], "%(ext)s", "mp4", 1)
	return os.WriteFile(out, testMP4(0, 10), 0o644)
}

func newTool(t *testing.T, f *fakeTool) *YtDlp {
	dir := t.TempDir()
	bundled := filepath.Join(dir, "bundled")
	os.WriteFile(bundled, []byte("bin"), 0o755)
	y := &YtDlp{Bundled: bundled, ToolsDir: filepath.Join(dir, "tools"), Run: f.run,
		FFmpeg: func() string { return "/opt/ffmpeg" }}
	f.versions[bundled] = "2026.10.26"
	f.versions[y.copyPath()] = ""
	return y
}

func TestFetchArgs(t *testing.T) {
	f := &fakeTool{versions: map[string]string{}}
	y := newTool(t, f)
	dst := filepath.Join(t.TempDir(), "k.mp4")
	if err := y.Fetch(context.Background(), "https://video.example/v/1", dst, 720); err != nil {
		t.Fatal(err)
	}
	if !looksMP4(dst) {
		t.Fatal("no file")
	}
	args := strings.Join(f.calls[0], " ")
	for _, want := range []string{
		y.Bundled + " --ignore-config", "--no-playlist", "--no-mtime", "--no-part", "--max-filesize 400M",
		"-f bv*[height<=720][vcodec^=avc1]+ba[ext=m4a]/b[ext=mp4]", "--merge-output-format mp4",
		"--ffmpeg-location /opt/ffmpeg", "-- https://video.example/v/1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q: %s", want, args)
		}
	}
	if _, err := exec.LookPath("nice"); err == nil && !strings.Contains(args, "nice -n 19") {
		t.Errorf("not at the lowest priority: %s", args)
	}
}

func TestUpdateUsesTheCopyAndFallsBack(t *testing.T) {
	f := &fakeTool{versions: map[string]string{}, updated: "2026.11.30"}
	y := newTool(t, f)
	if y.Current() != y.Bundled {
		t.Fatal("before an update")
	}
	y.Update(context.Background())
	if y.Current() != y.copyPath() || f.versions[y.copyPath()] != "2026.11.30" {
		t.Fatalf("after an update: %s %v", y.Current(), f.versions)
	}
	// a broken update: the bundled one again, the copy removed
	f.updated = ""
	y.Update(context.Background())
	if y.Current() != y.Bundled {
		t.Fatalf("broken update: %s", y.Current())
	}
	if _, err := os.Stat(y.copyPath()); err == nil {
		t.Fatal("kept a broken copy")
	}
	// a newer bundled tool (a new release) replaces an older copy
	f.updated = "2026.10.01"
	y.Update(context.Background()) // copy at 2026.10.01 after -U
	f.versions[y.Bundled] = "2026.12.01"
	f.updated = "2026.12.01"
	y.Update(context.Background())
	if y.Current() != y.copyPath() || f.versions[y.copyPath()] != "2026.12.01" {
		t.Fatalf("newer bundled: %v", f.versions)
	}
}

func TestStallKillsTheGroup(t *testing.T) {
	dir := t.TempDir()
	// a launcher whose child hangs without writing anything
	script := filepath.Join(dir, "yt-dlp")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 60 &\nwait\n"), 0o755)
	y := &YtDlp{Bundled: script, ToolsDir: filepath.Join(dir, "tools"), Stall: time.Second}
	start := time.Now()
	err := y.Fetch(context.Background(), "https://video.example/v/1", filepath.Join(t.TempDir(), "k.mp4"), 1080)
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("took %v", d)
	}
}
