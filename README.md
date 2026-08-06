# claude-launcher

A small web UI that spawns **remote-controlled Claude Code sessions** in a
directory + shell of your choice, and lists/kills them. The sessions themselves
are driven from <https://claude.ai/code> or the mobile **Code** tab — this app
only launches and tracks them.

One static Go binary, no runtime dependencies: the UI (htmx + server-rendered
templates) is embedded. Runs on macOS/Linux (zsh/bash/fish/sh) and Windows
(PowerShell/pwsh/cmd/Git Bash).

## How it works

Each launch runs, through the chosen shell, in the chosen directory:

```
claude --model "<model>" --remote-control "<name>"
```

Running it *through* a shell means that shell's profile (PATH, env, nvm,
aliases) is sourced first — pick the shell whose config the session should
inherit. The model id is always quoted, because ids like `claude-opus-4-8[1m]`
contain shell-special brackets. The model list lives in
`internal/models/models.go`.

`claude --remote-control` is **interactive**: given a non-TTY stdout it drops to
`--print` mode and exits. So sessions are spawned through a real
**pseudo-terminal** (ConPTY on Windows, forkpty on macOS/Linux). The PTY is
windowless but gives `claude` the terminal it needs; its output is captured
(capped at 256 KB) to `data/logs/<id>.log`.

On first launch `claude` can block on onboarding gates — "is this a project you
trust?", "Chrome extension detected", "try the new fullscreen renderer" —
*before* the remote-control channel comes up, so nobody can answer them. The
launcher watches the PTY output and auto-answers each known gate once
(`startupPrompts` in `internal/sessions/sessions.go`).

Claude Code has no API to list active remote-control sessions, so this app
tracks the ones it spawns **in memory**. Because the PTY is owned by the
launcher process, sessions live and die with it — restart the launcher and you
relaunch your sessions.

## Prerequisites

- **Claude Code ≥ 2.1.51**, logged in via claude.ai (`claude`, *not* an
  `ANTHROPIC_API_KEY`) — remote control needs the full OAuth scope.
- A Pro/Max/Team plan (remote control isn't on API-key/free plans).
- Go ≥ 1.25 to build (not needed to run a release binary).

## Run it

```sh
cp .env.example .env     # optional; every value has a default
make run                 # or: go run .
```

Then open <http://localhost:8922/>.

Build a binary, or cross-compile the set:

```sh
make build               # ./claude-launcher for this machine
make dist                # dist/ for darwin/linux/windows
```

## The ring (multi-host)

One UI can drive launchers on several machines. Membership is **static and
hand-maintained** — no election, no discovery, no gossip. A **leader** holds the
follower list; a **follower** knows only itself. Each node is the sole authority
for its own live sessions: the leader never stores a follower's state, it
queries on demand (`/api/sessions` fans out and unions) and forwards mutations
(`?node=<id>` proxies the request verbatim). So there is nothing to keep
consistent across nodes.

On the leader:

```
CLAUDE_LAUNCHER_ROLE=leader
CLAUDE_LAUNCHER_NODE_ID=laptop
CLAUDE_LAUNCHER_FOLLOWERS=desktop|Desktop|http://desktop.example.ts.net:8922
```

On each follower:

```
CLAUDE_LAUNCHER_ROLE=follower
CLAUDE_LAUNCHER_NODE_ID=desktop
```

With a single node the host picker stays hidden and the app behaves exactly as
a one-machine launcher.

## API

- `GET /healthz` — role, node id, platform, session count, detected shells
- `GET /api/nodes` — the ring as this node sees it (self + probed followers)
- `GET /api/shells` — shells detected on the targeted host
- `GET /api/models` — models offered in the dropdown (the default is flagged)
- `GET /api/config` — `{defaultDir}` for the targeted host
- `GET /api/browse?path=` — subdirectories, for the folder picker
- `POST /api/mkdir {path, name}` — create a subfolder under `path`
- `GET /api/sessions` — the ring-wide union, newest first
- `POST /api/sessions {dir, shell, name?, model?}` — launch
- `DELETE /api/sessions/{id}` — kill + forget
- `GET /api/sessions/{id}/log` — tail the captured output

Every host-specific route takes `?node=<id>` and is proxied to that follower.
`/ui/*` serves the htmx fragments the page swaps in; it's the same data through
server-rendered HTML.

## Security posture

**There is no auth.** Anyone who can reach the port can spawn a shell on the
host, browse its directories, and create folders. Bind it to a private network
(a tailnet, a VPN, localhost) — never expose it to the internet.
