# MultiClaude

A self-hosted coordination server for a swarm of **Claude Code** machines working on the same projects.

- **Git hosting** over smart HTTP, with a built-in **Git LFS** server. No authentication; it's meant for your LAN.
- A **project board** per repository: backlog / to do / in progress / review / blocked / done, with priorities, labels, required capabilities and dependencies. It has quick-add and drag & drop.
- **Machines** run a small daemon (`multiclaude-runner`). The runner detects the local Claude Code installation (logged into that machine's own subscription) and its hardware: CPU, RAM, GPUs and installed toolchains. It then claims and works on issues automatically.
- **Coordination**: the server matches issues to machines by capability. An issue with `requires: gpu` goes only to GPU machines, and those machines take it before generic work. Machines with the *reviewer* role review finished work. Approved branches are merged into `main` on the server.
- **Chat with the swarm**: message all machines on a project or a single one. Messages are injected live into running Claude sessions, or start a chat session on idle machines (with memory across messages). Agents answer back in the chat.
- **Agents coordinate too**: every Claude session gets an MCP toolset (`create_issue`, `list_issues`, `comment_issue`, `post_chat`, `read_chat`, `list_agents`, `complete_work`, `review_verdict`, …). Agents can split work into new issues for other machines, ask humans questions, and see what others are doing. Inside its own assignment, each session can also fan out to subagents.
- **Live run view**: watch any agent's tool calls and output as they happen, send it a message, or cancel it.

```
                 ┌───────────────────── NAS (Docker) ──────────────────────┐
  browser ──────▶│ web UI · board · chat · git (smart HTTP) · LFS · SQLite │
                 └──────▲─────────────────▲──────────────────▲─────────────┘
         long-poll+HTTP │                 │                  │  git clone/push
               ┌────────┴──────┐  ┌───────┴───────┐  ┌───────┴───────┐
               │ runner        │  │ runner        │  │ runner        │
               │ gpu-box       │  │ laptop        │  │ build-server  │
               │ caps: gpu,cuda│  │ caps: macos   │  │ role: reviewer│
               │ claude ×4     │  │ claude ×2     │  │ claude ×2     │
               └───────────────┘  └───────────────┘  └───────────────┘
```

## 1. Run the server on TrueNAS SCALE

The server is a single container. It needs one persistent volume, mounted at `/data`. That volume holds the repositories, LFS objects and the SQLite database.

**Get the image onto the NAS** (pick one):

- *Registry*: push this repo to GitHub. The included workflow (`.github/workflows/image.yml`) publishes `ghcr.io/<you>/multiclaude:latest`.
- *Manual*: `make image-tar` on your workstation, copy `multiclaude.tar` to the NAS, then run `sudo docker load -i multiclaude.tar`.

**Install**: create a dataset, e.g. `/mnt/tank/apps/multiclaude`, owned by the `apps` user (568). Then go to *Apps → Discover Apps → ⋮ → Install via YAML* and paste [`deploy/docker-compose.yml`](deploy/docker-compose.yml). Adjust the image name and host path first.

Open `http://<nas>:8080`.

Other hosts work the same way: `docker run -d -p 8080:8080 -v /srv/multiclaude:/data multiclaude:latest`. To run it without Docker, use `make build && ./bin/multiclaude-server --data ./data`. That needs `git` and `git-lfs` on the PATH.

## 2. Add machines

Do this on every computer that should work for you, as the normal user that is logged into Claude Code:

```sh
curl -fsSL http://<nas>:8080/install.sh | sh
# options:
curl -fsSL http://<nas>:8080/install.sh | sh -s -- --name gpu-box --roles worker --concurrency 4 --caps unity
```

The script installs `~/.local/bin/multiclaude-runner` plus a systemd user service (Linux) or a LaunchAgent (macOS). On Linux, run `sudo loginctl enable-linger $USER` so the service keeps running when you're logged out.

Requirements on each machine: `git`, `git-lfs`, and Claude Code (`claude`), logged in. Run `multiclaude-runner doctor --server http://<nas>:8080` to see what the runner detects.

Then open **Machines** in the UI and **assign each machine to projects**. New machines stay idle until you do. You can also override the machine's settings there:

- roles (`worker`, `reviewer`)
- parallel sessions
- extra capability tags
- Claude model

You can pause a machine at any time.

| Runner flag | Default | Meaning |
|---|---|---|
| `--server` | – | Server URL |
| `--name` | hostname | Display name |
| `--roles` | `worker,reviewer` | `worker` implements issues, `reviewer` reviews them |
| `--concurrency` | 2 | Parallel Claude Code sessions (can be overridden in the UI) |
| `--caps` | – | Extra capability tags in addition to the auto-detected ones |
| `--claude` | auto | Path to the `claude` CLI |
| `--permission-mode` | `bypassPermissions` | Passed to Claude Code. Agents run unattended, so use machines/accounts where that's acceptable. |
| `--workdir` | `~/.multiclaude` | Workspaces (one reusable checkout per parallel slot) |

Auto-detected capabilities include:

- OS and architecture: `linux`, `macos`, `amd64`, …
- GPUs: `gpu`, `cuda`/`nvidia`, `rocm`, `mps`
- `bigmem` (64 GB RAM or more)
- installed tools: `docker`, `python3`, `cargo`, `node`, `go`, `dotnet`, `cmake`, `ffmpeg`, `blender`, …

## 3. Work with it

1. **Create a project.** It gets a git repository at `http://<nas>:8080/git/<slug>.git`, optionally imported from an existing URL.
2. **Add issues.** Use the quick-add box (`n` focuses it). Syntax: `Train the model !high +gpu #ml`, where `!urgent|!high|!medium|!low` sets the priority, `+cap` adds a required capability, and `#label` adds a label. Open an issue to edit its description, dependencies, etc.
3. **Machines claim issues** from *To do*, by priority and board order:
   - Each issue is worked on in branch `issue-N`, then moves to *Review*.
   - A reviewer machine checks the branch, then approves or requests changes.
   - On approval the server merges into `main`. If the merge conflicts, the issue goes back to *To do* with instructions to merge `main` in.
   - Failed attempts are retried. After 3 failures the issue is *blocked* until you look at it.
4. **Chat.** For example: *"Plan a save-game system and create issues for it"*, or *"gpu-box: stop what you're doing and look at #12"*. Comments on an issue are also delivered live to the agent working on it.
5. **Review mode** per project:
   - `agent`: reviewer machines review.
   - `human`: you click **Approve & merge** in the issue.
   - `none`: work is merged as soon as the worker is done.
6. **Project instructions** (Settings) are appended to every agent's system prompt. Put build/test commands and conventions there.

Moving an issue out of *In progress*/*Review* on the board cancels the agent working on it.

## How it works

- **Server** (`cmd/multiclaude-server`, Go, single static binary plus an embedded UI):
  - SQLite in WAL mode.
  - Git through `git http-backend`.
  - LFS via the batch API with basic transfers, stored under `/data/lfs`.
  - Merges are computed in the bare repo with `git merge-tree --write-tree`, with no worktree needed.
  - The UI receives live updates over Server-Sent Events.
- **Runner** (`cmd/multiclaude-runner`):
  1. Long-polls the server for commands (`assign`, `message`, `cancel`).
  2. For each assignment, prepares a reusable checkout.
  3. Runs `claude -p --input-format stream-json --output-format stream-json …`, which keeps stdin open so messages can be injected into the live session.
  4. Streams every event to the server, then pushes the branch.
  5. Registers itself as an MCP server for that session (`multiclaude-runner mcp`), which forwards tool calls to the server.
- **Prompts** are built on the server (`internal/server/prompts.go`), so changing agent behaviour never requires updating the machines.
- If a machine disappears for 90s, or its runner restarts, its work is released back to the queue.

### API

Everything the UI does goes through a JSON API under `/api/…` (see `internal/server/server.go` for the routes), so you can script it, e.g.:

```sh
curl -XPOST http://nas:8080/api/projects/my-game/issues -d '{"title":"Fix jump","priority":"high","requires":["gpu"]}'
```

## Limits & notes

- No authentication or TLS. Keep it on a trusted network, or put a reverse proxy with auth in front of it.
- Runners support Linux and macOS. On Windows, use WSL.
- The cost shown is Claude Code's API-equivalent estimate. On subscriptions it's informational only.
