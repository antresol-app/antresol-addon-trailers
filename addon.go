package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// hookName is the one hook this addon answers (its manifest declares it).
const hookName = "trailers.fetch"

// retryAfterMs: how soon the core asks again while a trailer is fetched.
const retryAfterMs = 1000

// failedText is the refusal the user sees for a trailer that could not
// be fetched, in the server's language.
func failedText(locale string) string {
	if locale == "en" {
		return "Couldn’t load the trailer"
	}
	return "Не удалось загрузить трейлер"
}

// newHandler serves /health, the hook and the files (/media/<key>.mp4,
// which the core reads with Range on the socket only).
func newHandler(env addonkit.Env, cfg Config, cache *Cache, ready *addonkit.Ready) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", ready.Health)
	hooks := &addonkit.HookMux{}
	hooks.Handle(hookName, func(_ context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
		var req addonkit.TrailerFetch
		if err := json.Unmarshal(in, &req); err != nil {
			return nil, err
		}
		if !cfg.Enabled || !validKey(req.Key) || !fetchable(req.URL) {
			return &addonkit.Answer{Status: http.StatusNotFound}, nil
		}
		height := cfg.MaxHeight
		if req.MaxHeight > 0 {
			height = min(height, max(req.MaxHeight, 144))
		}
		p, st, err := cache.Ask(req.Key, req.URL, height)
		switch {
		case err != nil:
			return nil, addonkit.Refuse(failedText(env.Locale))
		case p == "":
			return &addonkit.Answer{Status: http.StatusAccepted, Body: addonkit.TrailerPending{RetryAfterMs: retryAfterMs}}, nil
		}
		return &addonkit.Answer{Status: http.StatusOK, Body: addonkit.TrailerFile{
			Path: "/media/" + req.Key + ".mp4", Size: st.Size(), DurationMs: mp4Duration(p), ContentType: "video/mp4"}}, nil
	})
	mux.Handle("POST /hooks/", hooks)
	media := func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutSuffix(r.PathValue("file"), ".mp4")
		p, st := "", os.FileInfo(nil)
		if ok {
			p, st = cache.File(key)
		}
		if p == "" {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		cache.Played(p, st)
		// the mtime is the cache's clock (a play moves it): the ETag, not
		// Last-Modified, keeps If-Range working across plays
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", `"`+key+"-"+strconv.FormatInt(st.Size(), 16)+`"`)
		http.ServeContent(w, r, "", time.Time{}, f)
	}
	mux.HandleFunc("GET /media/{file}", media)
	mux.HandleFunc("HEAD /media/{file}", media)
	return mux
}

// fetchable: an http or https URL with a host (never a local file).
func fetchable(u string) bool {
	rest, ok := strings.CutPrefix(u, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(u, "http://")
	}
	return ok && rest != "" && rest[0] != '/' && !strings.ContainsAny(u, " \t\r\n")
}
