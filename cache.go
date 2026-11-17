package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fetcher writes the video at url to dst (an MP4: H.264 and AAC) in a
// temp directory of its own, no taller than maxHeight. It stops with ctx.
type Fetcher interface {
	Fetch(ctx context.Context, url, dst string, maxHeight int) error
}

const (
	// defaultMaxBytes: above it the trailers played longest ago go first.
	defaultMaxBytes = 3 << 30
	// defaultMaxAge: a trailer nobody played for this long is removed.
	defaultMaxAge = 30 * 24 * time.Hour
	// fetchSlots: fetches at once; the others wait their turn.
	fetchSlots = 2
	// maxQueued: fetches started or waiting; a key beyond is not started
	// (its ask answers 202, a later ask starts it).
	maxQueued = 16
	// fetchLimit bounds one fetch.
	fetchLimit = 3 * time.Minute
	// failMemo: a key whose fetch failed is refused for this long.
	failMemo = 24 * time.Hour
	// tmpAge: a temp directory older than this is a fetch that died.
	tmpAge = time.Hour
	// touchEvery: a play marks its file (its mtime) at most this often.
	touchEvery = time.Minute
)

// keyRe: a trailer's id as TMDB lists it (also the cache's file name).
var keyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func validKey(k string) bool { return keyRe.MatchString(k) }

// errFailed: the key's fetch failed within failMemo.
var errFailed = errors.New("trailers: the fetch failed")

// Cache keeps fetched trailers in Dir as <key>.mp4: at most MaxBytes,
// least recently played first out, none unplayed for MaxAge. A file's
// mtime is its last play.
type Cache struct {
	Dir      string
	Fetcher  Fetcher
	MaxBytes int64
	MaxAge   time.Duration
	// Limit bounds one fetch (0: fetchLimit).
	Limit time.Duration
	// Ctx ends every fetch when it ends (nil: never; the program's stop).
	Ctx context.Context

	now func() time.Time // test clock (nil: time.Now)

	once   sync.Once
	sem    chan struct{}
	mu     sync.Mutex
	flight map[string]bool      // keys being fetched or waiting for a slot
	failed map[string]time.Time // key → when its fetch failed
	bg     sync.WaitGroup
	evict  sync.Mutex
}

func (c *Cache) init() {
	c.once.Do(func() {
		c.sem = make(chan struct{}, fetchSlots)
		c.flight = map[string]bool{}
		c.failed = map[string]time.Time{}
		if c.MaxBytes <= 0 {
			c.MaxBytes = defaultMaxBytes
		}
		if c.MaxAge <= 0 {
			c.MaxAge = defaultMaxAge
		}
	})
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// File is the cached file of key ("" when not cached).
func (c *Cache) File(key string) (string, os.FileInfo) {
	if !validKey(key) {
		return "", nil
	}
	p := filepath.Join(c.Dir, key+".mp4")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", nil
	}
	return p, st
}

// Ask is the state of key: its file when cached; errFailed within a day
// of a failed fetch; otherwise "" and nil, after starting its fetch from
// url in the background (once per key at a time).
func (c *Cache) Ask(key, url string, maxHeight int) (string, os.FileInfo, error) {
	c.init()
	if p, st := c.File(key); p != "" {
		return p, st, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.failed[key]; ok {
		if c.clock().Sub(at) < failMemo {
			return "", nil, errFailed
		}
		delete(c.failed, key)
	}
	if c.flight[key] || len(c.flight) >= maxQueued {
		return "", nil, nil
	}
	c.flight[key] = true
	c.bg.Add(1)
	go c.run(key, url, maxHeight)
	return "", nil, nil
}

// Wait waits for the fetches started so far (tests).
func (c *Cache) Wait() { c.bg.Wait() }

func (c *Cache) run(key, url string, maxHeight int) {
	defer c.bg.Done()
	c.sem <- struct{}{}
	err := c.download(key, url, maxHeight)
	<-c.sem
	c.mu.Lock()
	delete(c.flight, key)
	if err != nil {
		c.failed[key] = c.clock()
	}
	c.mu.Unlock()
	if err != nil {
		log.Printf("trailers: fetch %s: %v", key, err)
		return
	}
	c.Cleanup(key)
}

func (c *Cache) download(key, url string, maxHeight int) error {
	tmpRoot := filepath.Join(c.Dir, ".tmp")
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(tmpRoot, key+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	limit := c.Limit
	if limit <= 0 {
		limit = fetchLimit
	}
	parent := c.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	out := filepath.Join(tmp, key+".mp4")
	if err := c.Fetcher.Fetch(ctx, url, out, maxHeight); err != nil {
		return err
	}
	if !looksMP4(out) {
		return errors.New("no MP4 written")
	}
	now := c.clock()
	_ = os.Chtimes(out, now, now) // fetched counts as played: the LRU's clock
	return os.Rename(out, filepath.Join(c.Dir, key+".mp4"))
}

// Played marks key's file as played now (at most every touchEvery).
func (c *Cache) Played(p string, st os.FileInfo) {
	now := c.clock()
	if now.Sub(st.ModTime()) >= touchEvery {
		_ = os.Chtimes(p, now, now)
	}
}

// Cleanup removes the files unplayed for MaxAge, then those played longest
// ago while the cache is above MaxBytes (never keep, a file just fetched),
// and temp directories of fetches that died.
func (c *Cache) Cleanup(keep string) {
	c.init()
	c.evict.Lock()
	defer c.evict.Unlock()
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	now := c.clock()
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".mp4") || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(c.Dir, e.Name())
		if now.Sub(info.ModTime()) > c.MaxAge {
			os.Remove(p)
			continue
		}
		files = append(files, file{p, info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	kept := filepath.Join(c.Dir, keep+".mp4")
	for _, f := range files {
		if total <= c.MaxBytes {
			break
		}
		if keep != "" && f.path == kept {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
	tmpRoot := filepath.Join(c.Dir, ".tmp")
	if tmps, err := os.ReadDir(tmpRoot); err == nil {
		for _, e := range tmps {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > tmpAge {
				os.RemoveAll(filepath.Join(tmpRoot, e.Name()))
			}
		}
	}
}

// Run cleans the cache at start and daily until ctx ends.
func (c *Cache) Run(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		c.Cleanup("")
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
