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

- **All your tmux sessions, anywhere** — one websocket attaches a single
  top-level tmux client; the app lists **every** tmux session you have (including
  ones you started over plain SSH) and switches between them with
  `tmux switch-client`. Create local bash sessions ("+"), open configured SSH
  remotes, rename, and kill — all in one connection. Disconnects never lose work.
- **Login** — local PAM account (username + password) plus a 6-digit TOTP
  (Google Authenticator compatible). No `~/.config/websh.yaml` → no login. Web
  sessions are persisted, so restarting the daemon doesn't force a re-login.
- **Notifications** — when a program in the terminal needs attention (terminal
  bell, or an explicit `websh-notify` call), websh sends a **Web Push** if the
  PWA is backgrounded/closed, or an in-page hint if it's in the foreground.
- **PWA** — installable, offline app shell, in-app update prompt, and
  auto-reconnecting websocket with backoff + heartbeat.
- **Your sessions are yours** — websh never auto-kills tmux sessions (they're
  your work); remove them yourself from the list. Web login sessions expire
  after 7 days.

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
uses a locally-installed Go SDK with `GOTOOLCHAIN=local` (the system `go` is too
old and `~/go` is ephemeral, so the go.mod-driven toolchain fetch from go.dev
kept re-running and timing out). Install the SDK once:

```sh
tar -C /opt/tools -xzf /opt/tools/download/go1.26.3.linux-amd64.tar.gz  # -> /opt/tools/go
```

You don't need `build.sh` on a normal machine. CI uses `actions/setup-go` +
`GOTOOLCHAIN=local`, so it doesn't hit this.

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

There is one terminal view; tap **≡ 会话** to open the session drawer.

- **Switch** — the drawer lists **all** your tmux sessions (bash and SSH, however
  they were started); tap one to switch the client to it (`tmux switch-client`).
- **New bash** — tap **➕ 新建 Bash** (auto-named `sh1`, `sh2`, …).
- **SSH remote** — tap a configured remote to open a tmux session **on it**; each
  tap opens another, shown as `0@server`, `1@server`, … (i.e. tmux session `0`,
  `1`, … on `server`). They run on the remote and persist there independently, so
  reconnecting (or `0@server` again) reattaches the remote session.
- **Rename** — ✎ in the drawer, or tap the session name in the header. (tmux
  names can't contain `.` `:` `|`.)
- **Kill** — 🗑 in the drawer terminates a session.
- **Char mode** quick keys include the tmux prefix **^B** (your session *is* a
  tmux session) and the readline cursor keys **^A**/**^E**.

## Updating an installed PWA

The service worker serves the app shell (`index.html`, `app.js`) **network-first**,
so an installed PWA picks up a new build on the next reload while online — no
cache-busting dance. Only the big immutable vendor assets (xterm.js, icons) are
cached-first. The SW activates immediately when it changes (`skipWaiting` +
`clients.claim`) and the page reloads once to apply it; offline, the cached shell
is used. So after deploying: just reload. (One-time: an old cache-first SW from a
previous build must be replaced once — reload a couple of times, or unregister it
in DevTools, and it self-heals to network-first.)

## Notifications

Two triggers feed the same push/suppress decision:

1. **Terminal bell** — websh scans PTY output for BEL (`0x07`). claude-code and
   most TUIs ring the bell when they want you; zero setup, generic message.
2. **`websh-notify`** — call it from scripts or hooks for a specific message. It
   reads `~/.cache/websh/notify` (the daemon URL + your token, written at login,
   mode 0600) and posts to the daemon's loopback-only endpoint — so it works in
   **any** of your shells, including sessions not created by websh.

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

One websocket runs **one** PTY = one top-level tmux client (`tmux attach`, or
`new-session` if you have none) as the user, started via `pty.Open()` so websh
knows the client's tty and can target it with `tmux switch-client -c <tty> -t
<session>`. Switching/creating sessions is just a tmux command on that client —
no new connection. If the websocket drops, the tmux sessions live on;
reconnecting attaches a fresh client to your most-recent session and tmux
repaints. The client auto-reconnects with exponential backoff, and on returning
to the foreground or regaining network. Multiple devices each get their own
client/tty and switch independently.

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
internal/session/    single tmux client attach + switch/new/rename/kill (priv drop)
internal/bridge/     PTY <-> websocket, control frames, heartbeat, BEL scan
internal/presence/   per-user foreground/background tracking + push decision
internal/push/       VAPID + subscription store + send
static/              PWA: index.html, app.js, service-worker.js, manifest, xterm
```
