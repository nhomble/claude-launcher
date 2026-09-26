# claude-launcher

A small web UI that spawns **remote-controlled Claude Code sessions** in a
directory + shell of your choice, and lists/kills them. You drive the sessions
themselves from <https://claude.ai/code> or the mobile **Code** tab.

One static Go binary — the htmx UI is embedded. macOS/Linux and Windows.

![](./overview.png)

## Run it

```sh
make run                      # or: go run .   → http://localhost:8922/
make build                    # ./claude-launcher
make dist                     # dist/ for darwin, linux, windows
```

Needs **Claude Code ≥ 2.1.51** (tested against 2.1.280) logged in via claude.ai
(`claude`, *not* an `ANTHROPIC_API_KEY`) on a Pro/Max/Team plan — remote
control needs the full OAuth scope. Go ≥ 1.25 to build.

## How it works

Each launch runs, through the chosen shell, in the chosen directory:

```
claude --model "<model>" --remote-control "<name>"
```

Going *through* a shell sources its profile first (PATH, nvm, aliases), so pick
the shell whose config the session should inherit. The model id is always
quoted — some model ids contain shell-special characters (e.g. older
`[1m]`-suffixed context-window variants).

`claude --remote-control` is **interactive**: with a non-TTY stdout it drops to
`--print` mode and exits. So sessions run in a real **pseudo-terminal** (ConPTY
on Windows, forkpty elsewhere), windowless, output captured to
`data/logs/<id>.log` (capped at 256 KB).

On a first launch `claude` can block on onboarding gates — folder trust, Chrome
extension, fullscreen renderer — *before* remote control is up, so nobody can
answer them. The launcher watches the PTY and auto-answers each known gate once
(`startupPrompts` in `internal/sessions/sessions.go`).

Sessions are tracked **in memory** and owned by the launcher process: restart it
and you relaunch them.

## Configuration

- **`.env`** — per-machine wiring: port, node id, data dir. See `.env.example`.
- **`config.yaml`** — the catalog: models offered, shells known. Optional;
  built-in defaults apply otherwise.

```sh
claude-launcher --dump-config > config.yaml    # start from the defaults
```

A `config.yaml` **replaces** the defaults wholesale, so you can drop models you
never use. It is re-read when it changes — no restart, which matters because a
restart kills every live session. Invalid files are ignored, last good wins.

## The ring (multi-host)

One UI can drive launchers on several machines. Membership is static and
hand-maintained — no election, no discovery. Each node is the sole authority for
its own sessions; the leader queries on demand (`/api/sessions` fans out and
unions) and forwards mutations (`?node=<id>` proxies verbatim).

```sh
# leader                              # follower
CLAUDE_LAUNCHER_ROLE=leader           CLAUDE_LAUNCHER_ROLE=follower
CLAUDE_LAUNCHER_NODE_ID=laptop        CLAUDE_LAUNCHER_NODE_ID=desktop
CLAUDE_LAUNCHER_FOLLOWERS=desktop|Desktop|http://desktop.ts.net:8922
```

With one node the host picker stays hidden.

## Security

**There is no auth.** Anyone who reaches the port can spawn a shell on the host.
Bind it to a private network — a tailnet, a VPN, localhost. Never the internet.

## API

`GET /healthz` · `/api/nodes` · `/api/shells` · `/api/models` · `/api/config` ·
`/api/browse?path=` · `POST /api/mkdir` · `GET|POST /api/sessions` ·
`DELETE /api/sessions/{id}` · `GET /api/sessions/{id}/log`

Host-specific routes take `?node=<id>`. `/ui/*` serves the htmx fragments.
