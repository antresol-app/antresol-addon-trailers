# Antresol trailers addon («Трейлеры»)

Plays the trailers of movie and series pages inside the Antresol apps
(contract 1, `docs/addons/contract.md` in the core, «Trailers:
`trailers.fetch`»). The core lists a title's trailers and decides who may
play which; this addon fetches the video of the page the core names, keeps
it in a cache and serves the file on its socket, where the core reads it
with Range for the apps. It needs no permission: it declares the hook
`trailers.fetch` and nothing else, and has no web part.

## How it works

| Path | What |
| --- | --- |
| `POST /hooks/trailers.fetch` | `{"key", "url", "maxHeight"?}` → 200 `{"path", "size", "durationMs", "contentType"}` when cached, 202 `{"retryAfterMs"}` while fetching (the first ask starts the fetch), 409 for a day after a fetch failed, 404 when off or for a bad key or URL (`http(s)://` only) |
| `GET`, `HEAD /media/<key>.mp4` | the cached file (`http.ServeContent`: Range, If-Range with the ETag) |

- **The cache** is `data/cache/<key>.mp4` in the addon's data directory:
  at most 3 GiB, the trailers played longest ago removed first, none
  unplayed for 30 days; a file's mtime is its last play (a read moves it,
  at most once a minute). A fetch writes into `data/cache/.tmp/` and is
  renamed in only when the file starts with an MP4 `ftyp` box. Two fetches
  run at once, one per key (others wait their turn, at most 16 queued);
  a failed key is refused for 24 h. Cleaned at start, after every fetch
  and daily.
- **yt-dlp** fetches the page's video: H.264 up to the height
  (`bv*[height<=H][vcodec^=avc1]+ba[ext=m4a]/b[ext=mp4]`), merged into an
  MP4 with ffmpeg, at most 400 MB, at the lowest priority (`nice -n 19`,
  `ionice -c 3`), in a process group of its own, killed after 3 minutes or
  when its files stop growing for 20 s. The release ships the standalone
  binary pinned in `tools/ytdlp.lock` as `bin/yt-dlp` (no Python needed);
  the addon keeps a copy in `data/tools/` updated daily (`yt-dlp -U`) and
  falls back to the shipped one when the copy stops working. The sha256 pin
  covers the shipped binary only: the updated copy is what yt-dlp's own
  updater downloads, and it runs with the addon's rights. That is kept on
  purpose: YouTube changes break a pinned build within weeks.
- The hook accepts only the YouTube page of the trailer key
  (`https://www.youtube.com/watch?v=<key>` or `https://youtu.be/<key>`),
  never another address.
- **ffmpeg** is the core's: the addon asks `GET /info` of the core API
  (no permission needed) and passes its `ffmpeg` to yt-dlp; without an
  answer, ffmpeg from `PATH`.

## Settings

The operator sets them in the server's environment (the core passes
`ANTRESOL_TRAILERS_*` to the addon unchanged); a bad value stops the
addon at start.

| Variable | Default | What |
| --- | --- | --- |
| `ANTRESOL_TRAILERS_ENABLED` | `on` | `off`: answer 404 to every ask (the apps then play trailers their own way once no other addon does) |
| `ANTRESOL_TRAILERS_MAX_HEIGHT` | `1080` | the tallest picture fetched (144 to 2160); a smaller `maxHeight` of the core wins |
| `ANTRESOL_TRAILERS_YTDLP` | `bin/yt-dlp` beside the program | the shipped tool |
| `ANTRESOL_TRAILERS_FFMPEG` | the core's | ffmpeg for the merge |

## Build

```sh
make test                                  # go vet, go test
make build                                 # dist/: three platforms
make local                                 # dist-local/trailers/: this machine
make local YTDLP_BIN=/path/to/stand-in     # with another bin/yt-dlp
```

Every platform archive holds `bin/addon` and `bin/yt-dlp`;
`tools/ytdlp.sh` downloads the pinned binary once into
`$HOME/.cache/antresol/yt-dlp` and checks its sha256 on every build, and
addonkit's `cmd/manifest` (`-require bin/yt-dlp` in the Makefile) refuses an archive without either file. To move to a new
yt-dlp, change `tools/ytdlp.lock` (the version and the sha256 of each
asset, from the release's `SHA2-256SUMS`).

## Licence

Licence: MIT

The MIT licence (see `LICENSE`), Copyright (c) 2026 Antresol Contributors. yt-dlp is
released into the public domain (Unlicense); the standalone binaries
bundle their own dependencies under their own licences.
