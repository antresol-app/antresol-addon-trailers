package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// testMP4 is a small MP4: ftyp, a moov whose mvhd says 2 s (version v),
// and pad bytes of media data.
func testMP4(v byte, pad int) []byte {
	be32 := func(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }
	boxOf := func(kind string, body []byte) []byte {
		return append(append(be32(uint32(8+len(body))), kind...), body...)
	}
	var mvhd []byte
	if v == 1 {
		mvhd = append([]byte{1, 0, 0, 0}, make([]byte, 16)...)
		mvhd = append(mvhd, be32(1000)...)
		mvhd = binary.BigEndian.AppendUint64(mvhd, 2000)
	} else {
		mvhd = append([]byte{0, 0, 0, 0}, make([]byte, 8)...)
		mvhd = append(mvhd, be32(90000)...)
		mvhd = append(mvhd, be32(180000)...)
	}
	mvhd = append(mvhd, make([]byte, 80)...)
	out := boxOf("ftyp", []byte("isom\x00\x00\x02\x00isomavc1"))
	out = append(out, boxOf("mdat", bytes.Repeat([]byte{7}, pad))...)
	return append(out, boxOf("moov", boxOf("mvhd", mvhd))...)
}

// fakeFetcher writes testMP4 to dst (or bad bytes); block holds each fetch
// until it is closed; fail fails every fetch.
type fakeFetcher struct {
	runs    atomic.Int32
	running atomic.Int32
	peak    atomic.Int32
	block   chan struct{}
	fail    bool
	notMP4  bool
	size    int
	mu      sync.Mutex
	urls    []string
	heights []int
}

func (f *fakeFetcher) Fetch(ctx context.Context, url, dst string, h int) error {
	f.runs.Add(1)
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
	}
	f.mu.Lock()
	f.urls, f.heights = append(f.urls, url), append(f.heights, h)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.fail {
		return errors.New("fake failure")
	}
	if f.notMP4 {
		return os.WriteFile(dst, []byte("<html>not a video</html>"), 0o644)
	}
	return os.WriteFile(dst, testMP4(0, f.size), 0o644)
}

const (
	keyA = "aaaaaaaaaaa"
	keyB = "bbbbbbbbbbb"
	keyC = "ccccccccccc"
)

type env struct {
	t     *testing.T
	f     *fakeFetcher
	cache *Cache
	h     http.Handler
}

func newEnv(t *testing.T, f *fakeFetcher, mut func(*Config)) *env {
	cfg := Config{Enabled: true, MaxHeight: 1080}
	if mut != nil {
		mut(&cfg)
	}
	c := &Cache{Dir: t.TempDir(), Fetcher: f}
	ready := &addonkit.Ready{}
	ready.Set()
	return &env{t: t, f: f, cache: c, h: newHandler(addonkit.Env{ID: "trailers", Locale: "ru"}, cfg, c, ready)}
}

// hook asks trailers.fetch as the core does: its status and body.
func (e *env) hook(key, url string, maxHeight int) (int, string) {
	e.t.Helper()
	b, _ := json.Marshal(addonkit.TrailerFetch{Key: key, URL: url, MaxHeight: maxHeight})
	r := httptest.NewRequest("POST", "/hooks/trailers.fetch", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Antresol-Hook", "trailers.fetch")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func (e *env) get(method, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

const page = "https://video.example/watch?v=" + keyA

func TestHookFetchesThenServesWithRange(t *testing.T) {
	e := newEnv(t, &fakeFetcher{size: 5000}, nil)
	// first ask: 202, the fetch runs in the background
	code, body := e.hook(keyA, page, 0)
	if code != 202 || strings.TrimSpace(body) != `{"retryAfterMs":1000}` {
		t.Fatalf("%d %s", code, body)
	}
	e.cache.Wait()
	if e.f.urls[0] != page || e.f.heights[0] != 1080 {
		t.Fatalf("fetched %v %v", e.f.urls, e.f.heights)
	}
	code, body = e.hook(keyA, page, 0)
	var f addonkit.TrailerFile
	if code != 200 || json.Unmarshal([]byte(body), &f) != nil {
		t.Fatalf("%d %s", code, body)
	}
	want := testMP4(0, 5000)
	if f.Path != "/media/"+keyA+".mp4" || f.Size != int64(len(want)) || f.DurationMs != 2000 || f.ContentType != "video/mp4" {
		t.Fatalf("%+v", f)
	}
	if e.f.runs.Load() != 1 {
		t.Fatalf("fetched again: %d", e.f.runs.Load())
	}
	w := e.get("GET", f.Path, "Range", "bytes=10-19")
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), want[10:20]) || w.Header().Get("Content-Type") != "video/mp4" ||
		w.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range %d %v", w.Code, w.Header())
	}
	// If-Range with the ETag keeps the range; HEAD has the length
	etag := w.Header().Get("ETag")
	if w := e.get("GET", f.Path, "Range", "bytes=0-3", "If-Range", etag); w.Code != 206 || w.Body.Len() != 4 {
		t.Fatalf("if-range %d", w.Code)
	}
	if w := e.get("HEAD", f.Path); w.Code != 200 || w.Header().Get("Content-Length") != itoa(len(want)) || w.Body.Len() != 0 {
		t.Fatalf("head %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/media/" + keyB + ".mp4", "/media/" + keyA, "/media/..%2Fx.mp4", "/media/" + keyA + ".webm"} {
		if w := e.get("GET", p); w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	// a smaller maxHeight goes to the tool, never above the setting
	e.hook(keyB, page, 480)
	e.hook(keyC, page, 4000)
	e.cache.Wait()
	e.f.mu.Lock()
	hs := append([]int(nil), e.f.heights...)
	e.f.mu.Unlock()
	if len(hs) != 3 || !(hs[1] == 480 && hs[2] == 1080 || hs[1] == 1080 && hs[2] == 480) {
		t.Fatalf("heights %v", hs)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestHookNotHere(t *testing.T) {
	e := newEnv(t, &fakeFetcher{}, nil)
	for _, c := range []struct{ key, url string }{
		{"short", page}, {keyA, ""}, {keyA, "file:///etc/passwd"}, {keyA, "/local/file"}, {keyA, "https:///x"}, {keyA, "https://a b"},
	} {
		if code, _ := e.hook(c.key, c.url, 0); code != 404 {
			t.Fatalf("%+v: %d", c, code)
		}
	}
	off := newEnv(t, &fakeFetcher{}, func(c *Config) { c.Enabled = false })
	if code, _ := off.hook(keyA, page, 0); code != 404 || off.f.runs.Load() != 0 {
		t.Fatalf("off: %d", code)
	}
	if e.f.runs.Load() != 0 {
		t.Fatal("fetched a bad request")
	}
}

func TestHookFailureMemo(t *testing.T) {
	f := &fakeFetcher{fail: true}
	e := newEnv(t, f, nil)
	if code, _ := e.hook(keyA, page, 0); code != 202 {
		t.Fatal(code)
	}
	e.cache.Wait()
	code, body := e.hook(keyA, page, 0)
	if code != 409 || !strings.Contains(body, "Не удалось загрузить трейлер") {
		t.Fatalf("%d %s", code, body)
	}
	// a day: still refused, not fetched again
	now := time.Now()
	e.cache.now = func() time.Time { return now.Add(23 * time.Hour) }
	if code, _ := e.hook(keyA, page, 0); code != 409 || f.runs.Load() != 1 {
		t.Fatalf("within a day %d runs %d", code, f.runs.Load())
	}
	// after it: tried again
	f.fail = false
	e.cache.now = func() time.Time { return now.Add(25 * time.Hour) }
	if code, _ := e.hook(keyA, page, 0); code != 202 {
		t.Fatal(code)
	}
	e.cache.Wait()
	if code, _ := e.hook(keyA, page, 0); code != 200 || f.runs.Load() != 2 {
		t.Fatalf("after a day %d runs %d", code, f.runs.Load())
	}
	// a file that is not an MP4 is a failure, never served
	bad := newEnv(t, &fakeFetcher{notMP4: true}, nil)
	bad.hook(keyB, page, 0)
	bad.cache.Wait()
	if code, _ := bad.hook(keyB, page, 0); code != 409 {
		t.Fatalf("not mp4 %d", code)
	}
	if w := bad.get("GET", "/media/"+keyB+".mp4"); w.Code != 404 {
		t.Fatalf("served a non-MP4: %d", w.Code)
	}
	if n, _ := os.ReadDir(filepath.Join(bad.cache.Dir, ".tmp")); len(n) != 0 {
		t.Fatalf("temp dirs left: %v", n)
	}
}

func TestSingleflightAndTwoAtOnce(t *testing.T) {
	f := &fakeFetcher{block: make(chan struct{})}
	e := newEnv(t, f, nil)
	keys := []string{keyA, keyB, keyC, "ddddddddddd"}
	for range 3 {
		for _, k := range keys {
			if code, _ := e.hook(k, page, 0); code != 202 {
				t.Fatal(code)
			}
		}
	}
	time.Sleep(100 * time.Millisecond)
	if f.running.Load() != 2 {
		t.Fatalf("running %d", f.running.Load())
	}
	close(f.block)
	e.cache.Wait()
	if f.runs.Load() != int32(len(keys)) || f.peak.Load() != 2 {
		t.Fatalf("runs %d peak %d", f.runs.Load(), f.peak.Load())
	}
	for _, k := range keys {
		if code, _ := e.hook(k, page, 0); code != 200 {
			t.Fatalf("%s: %d", k, code)
		}
	}
}

func TestCacheLRUByPlayAndCleanup(t *testing.T) {
	f := &fakeFetcher{size: 1000}
	e := newEnv(t, f, nil)
	size := int64(len(testMP4(0, 1000)))
	e.cache.init()
	e.cache.MaxBytes = 2*size + 10
	base := time.Now().Add(-48 * time.Hour)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	fetch := func(k string, h int) {
		e.cache.now = func() time.Time { return at(h) }
		e.hook(k, page, 0)
		e.cache.Wait()
	}
	fetch(keyA, 0)
	fetch(keyB, 1)
	// A is played later than B: B goes first when C comes
	e.cache.now = func() time.Time { return at(2) }
	if w := e.get("GET", "/media/"+keyA+".mp4", "Range", "bytes=0-0"); w.Code != 206 {
		t.Fatal(w.Code)
	}
	fetch(keyC, 3)
	has := func(k string) bool { p, _ := e.cache.File(k); return p != "" }
	if !has(keyA) || has(keyB) || !has(keyC) {
		t.Fatalf("after C: A %v B %v C %v", has(keyA), has(keyB), has(keyC))
	}
	// unplayed for 30 days: removed; a temp dir of a dead fetch too
	dead := filepath.Join(e.cache.Dir, ".tmp", "x-1")
	os.MkdirAll(dead, 0o755)
	os.Chtimes(dead, at(0), at(0))
	e.cache.now = func() time.Time { return at(3).Add(30*24*time.Hour + time.Minute) }
	e.cache.Cleanup("")
	if has(keyA) || has(keyC) {
		t.Fatal("kept a file unplayed for 30 days")
	}
	if _, err := os.Stat(dead); err == nil {
		t.Fatal("kept a dead temp dir")
	}
}

func TestMP4Checks(t *testing.T) {
	dir := t.TempDir()
	for v, b := range map[byte][]byte{0: testMP4(0, 10), 1: testMP4(1, 10)} {
		p := filepath.Join(dir, "v.mp4")
		os.WriteFile(p, b, 0o644)
		if !looksMP4(p) || mp4Duration(p) != 2000 {
			t.Fatalf("version %d: %v %d", v, looksMP4(p), mp4Duration(p))
		}
	}
	p := filepath.Join(dir, "x")
	os.WriteFile(p, []byte("<html></html>xxxxxx"), 0o644)
	if looksMP4(p) || mp4Duration(p) != 0 || mp4Duration(filepath.Join(dir, "missing")) != 0 {
		t.Fatal("not an MP4")
	}
	// a truncated moov: no duration, no panic
	b := testMP4(0, 10)
	os.WriteFile(p, b[:len(b)-50], 0o644)
	if mp4Duration(p) != 0 {
		t.Fatal("truncated")
	}
}

func TestLoadConfig(t *testing.T) {
	env := addonkit.Env{ID: "trailers"}
	t.Setenv("ANTRESOL_TRAILERS_YTDLP", "/opt/yt")
	c, err := loadConfig(env)
	if err != nil || !c.Enabled || c.MaxHeight != 1080 || c.YtDlp != "/opt/yt" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("ANTRESOL_TRAILERS_ENABLED", "off")
	t.Setenv("ANTRESOL_TRAILERS_MAX_HEIGHT", "720")
	if c, err := loadConfig(env); err != nil || c.Enabled || c.MaxHeight != 720 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("ANTRESOL_TRAILERS_MAX_HEIGHT", "99999")
	if _, err := loadConfig(env); err == nil {
		t.Fatal("bad height")
	}
	t.Setenv("ANTRESOL_TRAILERS_MAX_HEIGHT", "")
	t.Setenv("ANTRESOL_TRAILERS_ENABLED", "maybe")
	if _, err := loadConfig(env); err == nil {
		t.Fatal("bad switch")
	}
}
