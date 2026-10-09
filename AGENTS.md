# AGENTS.md — gomuks

A terminal-based Matrix client written in Go using tcell/mauview for the TUI.

## Build & Run Commands

```bash
# Build (with version info embedded via ldflags)
./build.sh

# Build without version info
go build -v ./...

# Run
go build -o gomuks && ./gomuks

# Test
go test -v ./...

# Lint (CI uses pre-commit with goimports)
pre-commit run -a

# Install goimports (required by lint)
go install golang.org/x/tools/cmd/goimports@latest
```

Build version info is injected via `-ldflags` in `build.sh` and `.gitlab-ci.yml`: `main.Tag`, `main.Commit`, `main.BuildTime`.

## Architecture

### Core Triad

The app is built around three components connected through interfaces in `interface/`:

- **`Gomuks`** (`gomuks.go`) — Top-level orchestrator. Holds references to UI, Matrix, and Config. Manages lifecycle (Start/Stop), autosave loop.
- **`matrix.Container`** (`matrix/matrix.go`) — Matrix protocol layer. Wraps `mautrix.Client`, handles sync, message events, encryption, media upload/download, room operations.
- **`GomuksUI`** (`ui/ui.go`) — TUI layer built on `mauview` (which wraps `tcell`). Two views: `LoginView` → `MainView`.

All cross-component communication goes through the `ifc` (interface) package types: `Gomuks`, `GomuksUI`, `MatrixContainer`, `MainView`, `RoomView`, `Message`, `Crypto`.

### Package Layout

| Package | Purpose |
|---------|---------|
| `main/` (root) | Entry point (`main.go`, `gomuks.go`), CLI flags, directory resolution |
| `config/` | Config loading/saving (YAML/JSON), preferences, keybindings |
| `matrix/` | Matrix client container, syncer, event handlers, media, crypto, history |
| `matrix/muksevt/` | Extended event types (`muksevt.Event` wraps `event.Event` with gomuks-specific fields like `OutgoingState`, `Edits`) |
| `matrix/rooms/` | `Room` struct, `RoomCache` (LRU cache with gob persistence) |
| `ui/` | TUI: views, room list, message rendering, command processor |
| `ui/messages/` | Message type renderers (text, HTML, file, redacted, expanded text) |
| `ui/messages/html/` | HTML-to-terminal rendering engine |
| `ui/messages/tstring/` | Styled string primitives for terminal rendering |
| `ui/widget/` | Reusable UI widgets (borders, colors) |
| `interface/` (imported as `ifc`) | Interfaces decoupling UI ↔ Matrix ↔ Core |
| `lib/` | Platform-specific helpers: notifications, file picker, open URL, ansimage, LCP utility |
| `debug/` | Logging, panic recovery, deadlock detection |

### Data Flow

1. `matrix.Container.Start()` → calls `OnLogin()` which sets up the `GomuksSyncer`
2. Syncer receives `/sync` responses → dispatches events by type to registered handlers
3. Handlers (e.g. `HandleMessage`, `HandleMembership`) update room state, persist to `HistoryManager` (BoltDB), then call UI methods
4. UI renders via `mauview.Application.Redraw()`
5. User input → `CommandProcessor` → either local UI command or `MatrixContainer` method call
6. Config autosaves every minute via `Gomuks.StartAutosave()`

### Persistence

- **Config**: `~/.config/gomuks/config.yaml` (YAML)
- **Auth cache**: `<cache>/auth-cache.yaml`
- **Preferences**: `<cache>/preferences.yaml` (also synced via Matrix account data as `net.maunium.gomuks.preferences`)
- **Push rules**: `<cache>/pushrules.json` (JSON)
- **Room list**: `<cache>/rooms.gob.gz` (gzipped gob)
- **Room state**: `<cache>/state/` (per-room gob files)
- **Message history**: `<cache>/history.db` (BoltDB, events stored as gzipped gob)
- **Crypto (cgo builds)**: `<data>/crypto.db` (SQLite via go-sqlite3) or legacy `<data>/crypto.gob`
- **Media cache**: `<cache>/media/<homeserver>/<fileID>`

Directory overrides: `GOMUKS_ROOT`, `GOMUKS_CONFIG_HOME`, `GOMUKS_DATA_HOME`, `GOMUKS_CACHE_HOME`, `GOMUKS_DOWNLOAD_HOME`.

## Critical Build Tags: `cgo` / `!cgo`

Crypto support (E2EE via libolm) is gated by build tags:

- `matrix/crypto.go` — `//go:build cgo` — Full crypto with `mautrix/crypto` (OlmMachine, SQLCryptoStore)
- `matrix/nocrypto.go` — `//go:build !cgo` — No-op stubs
- `ui/crypto-commands.go` — `//go:build cgo` — `/verify`, `/devices`, `/import`, etc.
- `ui/no-crypto-commands.go` — `//go:build !cgo` — Stubs that reply "built without encryption support"

**To build with crypto**: you need `libolm-dev` installed and CGO enabled. CI installs `libolm-dev libolm3` on Ubuntu. On macOS, requires Homebrew's libolm with `LIBRARY_PATH`/`CPATH` set.

Without CGO, the binary compiles but all crypto commands are no-ops and encrypted messages show as "encryption unsupported".

## Conventions

- **Indentation**: Tabs, tab width 4 (see `.editorconfig`)
- **Line endings**: LF
- **Import alias**: `interface` package is always imported as `ifc`
- **Sync import alias**: `github.com/sasha-s/go-deadlock` is imported as `sync` (deadlock-detecting mutex replacement)
- **Error handling pattern**: Most event handlers use `defer debug.Recover()` for goroutine panic recovery. Non-fatal errors are logged via `debug.Print/Printf` and silently ignored.
- **Custom event types**: `muksevt.EventBadEncrypted` and `muksevt.EventEncryptionUnsupported` are gomuks-specific event types registered in `mautrix`'s `event.TypeMap` via `init()`
- **Config save gating**: `config.nosave` bool prevents saves after cache clear
- **No test suite**: There are effectively no unit tests. `go test ./...` runs but finds nothing meaningful.

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `DEBUG=1` | Enable deadlock detection, disable pretty-panic, enable log writes |
| `DEBUG_DIR` | Override debug log directory |
| `GOMUKS_ALLOW_INSECURE_CONNECTIONS` | Skip TLS verification |
| `GOMUKS_ROOT` | Set root for config/data/cache subdirs |
| `GOMUKS_CONFIG_HOME` | Override config directory |
| `GOMUKS_DATA_HOME` | Override data directory |
| `GOMUKS_CACHE_HOME` | Override cache directory |
| `GOMUKS_DOWNLOAD_HOME` | Override download directory |
| `TCELLDB` | Override tcell terminal database path |

## Gotchas

- **`go-sqlite3` requires CGO**: Building with crypto support (`cgo` tag) requires a C compiler. Pure Go builds (`!cgo`) lose all E2EE functionality.
- **`runewidth` replace directive**: `go.mod` replaces `github.com/mattn/go-runewidth` with a custom fork for better terminal width handling.
- **Media proxy**: `Container` starts a local HTTP server (`httptest.NewServer`) as a media proxy for inline URL rendering in terminals.
- **HistoryManager uses BoltDB** with a split uint64 key space: first half for prepend (history pagination), second half for append (new messages). The `newDBPointer` from server-side history loads is known to be inaccurate (see TODOs in `history.go`).
- **SSO login** listens on `localhost:29325` — hardcoded port.
- **`gob.Register` calls** in `init()` functions are critical for serialization; adding new types to `muksevt` or `rooms` requires registering them.
- **`RoomCache` is an LRU** backed by a doubly-linked list; rooms are loaded/unloaded automatically. Call `DisableUnloading()`/`EnableUnloading()` during initial sync.
- **The app exits via `os.Exit()`** everywhere; returning from `gmx.Start()` is treated as unexpected.
- **CI**: GitHub Actions (lint + build + test on Go 1.21/1.22), GitLab CI (cross-platform builds: linux/amd64, linux/arm, linux/arm64, windows/amd64, macos/arm64 + Debian packaging).
