# websh

[![CI](https://github.com/juliuswwj/websh/actions/workflows/ci.yml/badge.svg)](https://github.com/juliuswwj/websh/actions/workflows/ci.yml)

A self-contained mobile shell-terminal **PWA**. Log in with a local account
(PAM password + TOTP), then open as many **tmux** sessions as you like — ad-hoc
local shells, or SSH to remotes from your config using your own `~/.ssh` keys.
Because every session is a tmux session, dropped websockets don't lose your
work: reconnecting reattaches to the live session.

It is inspired by an internal tool that proxied terminals through an
"x-workbench" service over websockets (fragile, disconnect-prone). websh owns
the PTY/tmux itself and has no such dependency.

## Features

- **tmux sessions** — create local bash sessions on the fly ("+"), or SSH to
  configured remotes with your own `~/.ssh` keys. List, rename, and kill any
  live session; sessions persist across disconnects and reconnect reattaches.
- **Login** — local PAM account (username + password) plus a 6-digit TOTP
  (Google Authenticator compatible). No `~/.config/websh.yaml` → no login. Web
  sessions are persisted, so restarting the daemon doesn't force a re-login.
- **Notifications** — when a program in the terminal needs attention (terminal
  bell, or an explicit `websh-notify` call), websh sends a **Web Push** if the
  PWA is backgrounded/closed, or an in-page hint if it's in the foreground.
- **PWA** — installable, offline app shell, in-app update prompt, and
  auto-reconnecting websockets with backoff + heartbeat.
- **Resource hygiene** — web login sessions expire after 7 days; tmux sessions
  with no user input for 3 days are reclaimed automatically.

## Build

websh links libpam via cgo (Go ≥ 1.23, coder/websocket). On a normal box just
install the dev header and build with the standard toolchain:

```sh
sudo apt-get install -y libpam0g-dev tmux   # pam_appl.h + tmux at runtime
go build ./cmd/websh
CGO_ENABLED=0 go build ./cmd/websh-notify   # websh-notify is pure Go
```

`go test ./...` and CI do exactly this (see `.github/workflows/ci.yml`).

### `build.sh` (this dev host only)

On the original dev host `libpam0g-dev` wasn't installed system-wide and there
was no root, so the dev package was extracted locally under `.builddeps/` and
`build.sh` points cgo at it. It also forces the **system** gcc (`/usr/bin/gcc`)
— a conda gcc on `PATH` has the wrong sysroot and fails to link libpam — and
pins a Go toolchain. You don't need `build.sh` on a normal machine.

## Releases

Pushing a `v*` tag builds a `websh-<tag>-linux-amd64.tar.gz` bundle (the two
binaries — the web UI is embedded — plus config samples) and attaches it to a
GitHub Release (`.github/workflows/release.yml`).

## Install / run

```sh
sudo install -m755 bin/websh         /usr/local/bin/websh
sudo install -m755 bin/websh-notify  /usr/local/bin/websh-notify
sudo cp pam.d/websh /etc/pam.d/websh
sudo cp websh.service /etc/systemd/system/ && sudo systemctl enable --now websh
```

The web UI is **embedded in the `websh` binary** (`//go:embed static`), so there
is no asset directory to deploy — just the two binaries. For frontend
development, `websh --static ./static` serves the files from disk instead.

Put `nginx.example.conf` in front for HTTPS (**required** — service workers and
Web Push need a secure context).

### Why it must run as root

The daemon reads `~<user>/.config/websh.yaml` for any user and spawns their
shells **as that user** (drops to their uid/gid + supplementary groups per
session). The systemd unit therefore runs `User=root` and must **not** set
`NoNewPrivileges=true` (breaks setuid) or `ProtectHome` (needs other homes).
Keep the root-side code minimal; all per-session work runs privilege-dropped.

## First-run setup — `websh config`

Each user runs this **once, as themselves** (no root needed) to create
`~/.config/websh.yaml` and enroll their OTP:

```sh
websh config            # creates the file + prints a QR code in the terminal
websh config --regen    # rotate the OTP secret
websh config --invert   # flip QR colors for a light-background terminal
```

It generates a TOTP secret and renders a **scannable QR code right in the
terminal** — scan it with Google Authenticator (or any TOTP app), and it also
prints the raw secret for manual entry. Local shells need no config (create them
in the UI); add SSH targets under `remotes:` (see below).

## User config — `~/.config/websh.yaml`

`websh config` writes this for you. Local shells are created ad-hoc in the UI
("+ new bash"); only SSH remotes are configured. Full schema (see
`websh.example.yaml`):

```yaml
otp_secret: "BASE32SECRET"     # filled in by `websh config`
display_name: "Wen Jun"        # optional
remotes:
  - host: "gpu01.internal"     # required; session id is derived from the host
    name: "GPU 01"             # optional display name (defaults to host)
    user: "deploy"             # optional ssh login user
    port: 22                   # optional
    # id: gpu                  # optional override; only to disambiguate same host
```

`host` and `ssh_options` are validated; dangerous ssh options (`ProxyCommand`,
`LocalCommand`, `PermitLocalCommand`) are rejected. Each remote uses the logged-in
user's own `~/.ssh` keys.

## Using the terminal

- **New bash** — tap **＋** (tab bar or session list). Each one is its own tmux
  session with an auto-assigned numeric id.
- **Switch / reattach** — the session list shows every live tmux session; tap to
  (re)attach. The **‹ 返回** button returns to the list.
- **Rename** — tap a session's ✎ in the list, or tap the title in the terminal
  header. The label is stored on the tmux session, so it survives reconnects.
- **Kill** — tap 🗑 in the list to terminate a session.
- **Char mode** quick keys include the tmux prefix **^B** (your session *is* a
  tmux session) and the readline cursor keys **^A**/**^E**.

## Updating an installed PWA

The web UI is cached for offline use, so an installed PWA keeps the cached
version until it updates. When you deploy a new build (rebuild, bump the cache
version in `service-worker.js`, restart), the app shows a **「发现新版本 · 点击更新」**
banner on next launch; tapping it activates the new version and reloads. The
terminal isn't interrupted otherwise (tmux persists and reconnects).

## Notifications

Two triggers feed the same push/suppress decision:

1. **Terminal bell** — websh scans PTY output for BEL (`0x07`). claude-code and
   most TUIs ring the bell when they want you; zero setup, generic message.
2. **`websh-notify`** — call it from scripts or hooks for a specific message. It
   reads `WEBSH_SESSION` / `WEBSH_NOTIFY_TOKEN` / `WEBSH_NOTIFY_URL` (injected
   into the shell) and posts to the daemon's loopback-only endpoint.

   ```sh
   websh-notify "build finished"
   ```

   claude-code Notification hook (`~/.claude/settings.json`):

   ```json
   { "hooks": { "Notification": [ { "hooks": [ { "type": "command", "command": "websh-notify --from claude" } ] } ] } }
   ```

The browser must grant notification permission (tap **🔔 开启通知**) and the app
must be served over HTTPS.

### iOS note

Web Push on iOS needs **iOS 16.4+** and the app **installed to the Home Screen**
(Add to Home Screen); permission must be granted from inside the installed PWA.
Android Chrome works directly.

## How persistence/reconnect works

Each terminal is a tmux session `websh-<uid>-<id>`. A websocket spawns a PTY
running `tmux new-session -A -s <name>` (attach-or-create) as the user. If the
websocket drops, that attach client dies but the tmux session lives on;
reconnecting runs the same command and tmux repaints. The client auto-reconnects
with exponential backoff, and on returning to the foreground or regaining
network.

The server heartbeats every `--ws-heartbeat` (default 15s) to keep idle
connections alive through reverse-proxy idle timeouts — **set your proxy's
`proxy_read_timeout` high** (the example uses 3600s); if you can't, lower
`--ws-heartbeat` below the proxy timeout. Web login sessions are persisted to
`--session-store` (default `/run/websh/sessions.json`) so a restart doesn't log
everyone out.

## Layout

```
assets.go            //go:embed of static/ (web UI baked into the binary)
cmd/websh/           HTTP server, auth, ws, push, notify endpoints
cmd/websh-notify/    CLI for scripts / claude-code hook
internal/config/     ~/.config/websh.yaml load + validation
internal/auth/       PAM, TOTP, web-session store + cookie
internal/session/    PTY + tmux spawn (privilege drop), idle janitor
internal/bridge/     PTY <-> websocket, heartbeat, BEL scan
internal/presence/   foreground/background tracking + push decision
internal/push/       VAPID + subscription store + send
static/              PWA: index.html, app.js, service-worker.js, manifest, xterm
```
