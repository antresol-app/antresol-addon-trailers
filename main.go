// The «Трейлеры» addon (contract 1, docs/addons/contract.md in the core):
// it answers the trailers.fetch hook. Asked for a trailer it fetches the
// page's video with yt-dlp (the pinned binary of its release, kept up to
// date in its data directory) into a cache of its own, and serves the
// file on its socket, where the core reads it with Range for the apps.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

func main() {
	env, err := addonkit.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := loadConfig(env)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(env.DataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	cacheDir := filepath.Join(env.DataDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	os.Remove(env.Socket) // a stale socket of an earlier run
	ln, err := net.Listen("unix", env.Socket)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	// ffmpeg for the merge: the setting, else the core's (/info), asked in
	// the background, else ffmpeg from PATH
	var ffmpeg atomic.Value
	ffmpeg.Store(cfg.FFmpeg)
	tool := &YtDlp{Bundled: cfg.YtDlp, ToolsDir: filepath.Join(env.DataDir, "tools"),
		FFmpeg: func() string { s, _ := ffmpeg.Load().(string); return s }}
	cache := &Cache{Dir: cacheDir, Fetcher: tool}
	ready := &addonkit.Ready{}
	srv := &http.Server{Handler: newHandler(env, cfg, cache, ready), ReadHeaderTimeout: 10 * time.Second}

	// the core stops an addon with SIGTERM and kills it 10 s later
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	cache.Ctx = ctx // a stop kills the fetches' process groups
	var loops sync.WaitGroup
	loops.Go(func() { cache.Run(ctx) })
	if cfg.Enabled {
		loops.Go(func() { tool.Loop(ctx) })
	}
	if cfg.FFmpeg == "" {
		loops.Go(func() {
			core := addonkit.NewClient(env)
			for wait := time.Second; ctx.Err() == nil; wait = min(2*wait, time.Minute) {
				info, err := core.Info(ctx)
				if err == nil {
					ffmpeg.Store(info.FFmpeg)
					return
				}
				log.Printf("trailers: core info: %v", err)
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
			}
		})
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	ready.Set()
	log.Printf("%s: serving on %s (fetching %v, up to %dp)", env.ID, env.Socket, cfg.Enabled, cfg.MaxHeight)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	loops.Wait()
	cache.Wait()
}
