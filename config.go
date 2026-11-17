package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol/addonkit"
)

// defaultMaxHeight: the tallest picture fetched unless set otherwise.
const defaultMaxHeight = 1080

// Config is the addon's settings, from its own variables (the core passes
// ANTRESOL_TRAILERS_* to it unchanged):
//
//	ANTRESOL_TRAILERS_ENABLED     on|off (default on): off fetches nothing
//	ANTRESOL_TRAILERS_MAX_HEIGHT  the tallest picture, 144…2160 (default 1080)
//	ANTRESOL_TRAILERS_YTDLP       the bundled yt-dlp (default bin/yt-dlp
//	                              beside the program)
//	ANTRESOL_TRAILERS_FFMPEG      ffmpeg for the merge (default the core's,
//	                              from /info, else ffmpeg from PATH)
type Config struct {
	Enabled   bool
	MaxHeight int
	YtDlp     string
	FFmpeg    string
}

func loadConfig(env addonkit.Env) (Config, error) {
	c := Config{Enabled: true, MaxHeight: defaultMaxHeight, FFmpeg: env.Var("FFMPEG")}
	switch v := strings.ToLower(strings.TrimSpace(env.Var("ENABLED"))); v {
	case "", "on", "1", "true", "yes":
	case "off", "0", "false", "no":
		c.Enabled = false
	default:
		return c, fmt.Errorf("%sENABLED: %q is not on or off", env.Prefix(), v)
	}
	if v := strings.TrimSpace(env.Var("MAX_HEIGHT")); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 144 || h > 2160 {
			return c, fmt.Errorf("%sMAX_HEIGHT: %q is not a height from 144 to 2160", env.Prefix(), v)
		}
		c.MaxHeight = h
	}
	c.YtDlp = env.Var("YTDLP")
	if c.YtDlp == "" {
		exe, err := os.Executable()
		if err != nil {
			return c, err
		}
		c.YtDlp = filepath.Join(filepath.Dir(exe), "yt-dlp")
	}
	return c, nil
}
