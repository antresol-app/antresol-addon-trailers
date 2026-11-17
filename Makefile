# The «Трейлеры» addon's release (docs/addons/contract.md in the core).
#   make build   dist/: one archive per platform (bin/addon and the pinned
#                bin/yt-dlp of tools/ytdlp.lock) and antresol-addon.json
#                with their sha256; no web part
#   make local   dist-local/<id>/: the same for this machine only, for a dev
#                server started with VAULT_ADDONS_DEV=<this dir>/dist-local
#                (install «local:<id>» in «Расширения»); YTDLP_BIN=<file>
#                packs that file as bin/yt-dlp instead of the pinned one
#   make test    go vet and the Go tests
# The program builds on addonkit (go.mod's require).

ID := $(shell sed -n 's/^ *"id": *"\([^"]*\)".*/\1/p' antresol-addon.json | head -n 1)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64
HOST := $(shell GOENV=off go env GOOS)/$(shell GOENV=off go env GOARCH)

# a static, reproducible binary: no GOFLAGS or go env settings of a
# surrounding build, no cgo, no paths or VCS stamp of this checkout
export GOFLAGS :=
export GOENV := off
export CGO_ENABLED := 0
GOBUILD := go build -trimpath -buildvcs=false -ldflags='-s -w'
# plain, reproducible archives: ustar (no pax headers), entries sorted,
# owner 0, fixed modes and times, gzip without a name or time; on macOS no
# extended attributes and no AppleDouble ._ files (the core refuses them)
export COPYFILE_DISABLE := 1
BSDTAR := $(findstring bsdtar,$(shell tar --version 2>/dev/null))
TAR := tar --format=ustar $(if $(BSDTAR),--no-mac-metadata --no-xattrs --uid 0 --gid 0 --uname root --gname root -n,--owner=root:0 --group=root:0 --no-recursion)

.PHONY: build local test clean

build:
	rm -rf dist build && mkdir -p dist
	$(foreach p,$(PLATFORMS),$(call pack,$(p),dist))
	go run ./tools/manifest -src antresol-addon.json -dir dist $(if $(TAG),-tag $(TAG))

local:
	rm -rf dist-local/$(ID) build && mkdir -p dist-local/$(ID)
	$(call pack,$(HOST),dist-local/$(ID))
	go run ./tools/manifest -src antresol-addon.json -dir dist-local/$(ID)

test:
	go vet ./...
	go test ./...

# targz DIR,ROOT,MODE,OUT: what ROOT (inside DIR) holds into OUT, its files
# at MODE, its folders at 755, all at one fixed time
define targz
find $(1) -type d -exec chmod 755 {} +
find $(1) -type f -exec chmod $(3) {} +
find $(1) -exec touch -h -t 198001010000 {} +
(cd $(1) && find $(2) | LC_ALL=C sort | $(TAR) -cf - -T -) | gzip -n -9 >$(4)
endef

# pack OS/ARCH,DIR: build/<os>-<arch>/bin/{addon,yt-dlp} into DIR/<id>-<os>-<arch>.tar.gz
define pack
GOOS=$(word 1,$(subst /, ,$(1))) GOARCH=$(word 2,$(subst /, ,$(1))) $(GOBUILD) -o build/$(subst /,-,$(1))/bin/addon .
sh tools/ytdlp.sh $(1) build/$(subst /,-,$(1))/bin/yt-dlp
$(call targz,build/$(subst /,-,$(1)),bin,755,$(2)/$(ID)-$(subst /,-,$(1)).tar.gz)

endef

clean:
	rm -rf build dist dist-local
