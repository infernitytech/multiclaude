# MultiClaude

A self-hosted coordination server for a swarm of **Claude Code** machines working on the same projects.

- **Git hosting** over smart HTTP, with a built-in **Git LFS** server. A project can span **several repositories**, and each can be **mirrored to GitHub** (public or private), updated automatically after every merge.
- **Safe to expose to the internet**: HTTPS with automatic Let's Encrypt certificates, username/password logins (also for git), and per-machine tokens for runners that are enrolled with a single command.
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

**The image** is `ghcr.io/infernitytech/multiclaude` (`:latest` = newest release, `:X.Y.Z` pinned, `:latest-main` = every commit). To build your own instead, `make serve-image` serves it from your workstation and prints a command to paste into the TrueNAS web shell.

**Install**: create a dataset, e.g. `/mnt/tank/apps/multiclaude`, owned by the `apps` user (568). Then go to *Apps → Discover Apps → ⋮ → Install via YAML* and paste [`deploy/docker-compose.yml`](deploy/docker-compose.yml). Adjust the host path and `MC_DOMAIN` first.

**Update**: *Apps → multiclaude → ⋮ → Pull Image*. The database migrates itself, agents keep working through the short downtime, and **runners update themselves** to the server's version once their running sessions are done. Details and rollback: [deploy/TRUENAS.md](deploy/TRUENAS.md#7-updating).

Open `http://<nas>:8090` (LAN) or `https://<your domain>`. The first start prints a generated `admin` password in the app logs. **[deploy/TRUENAS.md](deploy/TRUENAS.md)** walks through the whole internet-facing setup: domain, port forwarding, certificates, users, machines.

| Server setting (flag / env) | Default | Meaning |
|---|---|---|
| `--addr` / `MC_ADDR` | `:8090` | Plain HTTP listener (`off` disables it) |
| `--domain` / `MC_DOMAIN` | – | Public name(s). Turns on HTTPS with Let's Encrypt |
| `--dns-provider`, `--dns-api-key` / `MC_DNS_PROVIDER`, `MC_DNS_API_KEY` | – | `cloudflare` or `ionos`: get the certificate via the DNS challenge, which works on any port. Without it the TLS-ALPN challenge is used, which needs external port 443 |
| `--dyndns` / `MC_DYNDNS=on` | off | Keep the domain's `A` record at the current public IP (IONOS) |
| `--acme-directory` / `MC_ACME_DIRECTORY` | Let's Encrypt | e.g. the Let's Encrypt staging URL for testing |
| `--tls-addr` / `MC_TLS_ADDR` | `:8443` | HTTPS listener |
| `--tls-cert`, `--tls-key` / `MC_TLS_CERT`, `MC_TLS_KEY` | – | Use your own certificate instead |
| `--acme-email` / `MC_ACME_EMAIL` | – | Contact address for Let's Encrypt |
| `MC_ADMIN_USER`, `MC_ADMIN_PASSWORD` | `admin`, random | First account, created only when there are no users |
| `--data` / `MC_DATA` | `./data` | Repositories, LFS objects, database, certificates |
| `MC_UPDATE_REPO` | `infernitytech/multiclaude` | GitHub repo checked for new releases (`off` disables) |

`multiclaude-server reset-password [user]` sets a new random password.

Other hosts work the same way: `docker run -d -p 8090:8090 -v /srv/multiclaude:/data multiclaude:latest`. To run it without Docker, use `make build && ./bin/multiclaude-server --data ./data`. That needs `git` and `git-lfs` on the PATH.

## 2. Add machines

Do this on every computer that should work for you, as the normal user that is logged into Claude Code. Copy the command from **Machines → Add a machine**, which includes the join token:

```sh
curl -fsSL https://<server>/install.sh | sh -s -- --token mcj_…
# options:
curl -fsSL https://<server>/install.sh | sh -s -- --token mcj_… --name gpu-box --roles worker --concurrency 4 --caps unity
```

The join token enrolls the machine once. The machine then gets its own token, stored in `~/.multiclaude/credentials.json`, which it uses for the API and for git. Removing a machine in the UI revokes it. Without the installer: `multiclaude-runner enroll --server URL --token mcj_…` and then `multiclaude-runner run`.

The script installs `~/.local/bin/multiclaude-runner` plus a systemd user service (Linux) or a LaunchAgent (macOS). On Linux, run `sudo loginctl enable-linger $USER` so the service keeps running when you're logged out.

Requirements on each machine: `git`, `git-lfs`, and Claude Code (`claude`), logged in. Run `multiclaude-runner doctor` to see what the runner detects and whether it's enrolled.

Then open **Machines** in the UI and **assign each machine to projects**. New machines stay idle until you do. You can also override the machine's settings there:

- roles (`worker`, `reviewer`)
- parallel sessions
- extra capability tags
- Claude model

You can pause a machine at any time.

| Runner flag | Default | Meaning |
|---|---|---|
| `--server` | from credentials | Server URL |
| `--token` | – | Join token (only needed to enroll) |
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

1. **Create a project.** It gets a primary git repository at `https://<server>/git/<slug>.git` (log in with your username and password), optionally imported from an existing URL.
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
6. **Questions.** When an agent needs a decision only a human can make, it calls `ask_human`:
   - The question shows up on the **Questions** page (sidebar badge, tab title), in the project chat, and as a ❓ on the issue card.
   - Answer with one of the suggested options or in your own words, or click **Dismiss** to let the agent decide.
   - By default the agent waits for up to 10 minutes, and can keep waiting longer.
   - If you answer later, the answer is injected into the running session. If that session has ended, the answer is added to the issue, and a blocked issue goes back to *To do*.
7. **Talk to one agent.** 💬 on a live board card or agent tile, or the message box in the issue panel, injects a message straight into that agent's session.
8. **Project instructions** (Settings) are appended to every agent's system prompt. Put build/test commands and conventions there.
9. **Repositories & mirrors** (Settings) — see below.
10. **Machines** shows each machine's Claude account and its live load, refreshed every 10 seconds: CPU, RAM, disk space over all local disks, and per-GPU utilisation and VRAM.
11. **Subagents.** Every agent is told to parallelize independent work with subagents and to pick cheaper models where they suffice: haiku for searching and summarizing, sonnet for routine implementation, the main model for design and integration.

### Machines with several GPUs

Every GPU is detected individually: model, memory, device number, architecture (Blackwell, Hopper, Ada Lovelace, CDNA3, …), compute capability (`sm_120`) or AMD target (`gfx942`), and driver. The runner also reports the CUDA version the driver supports, the CUDA toolkit (`nvcc`), the ROCm version and CPU instruction sets (AVX-512, AMX, SVE, …). Agents see all of this for their own machine and for every other machine. GPUs are handed out **exclusively per session**, so several GPU agents can run on one machine without fighting over a card:

| `requires` | Gets |
|---|---|
| `gpu` (or `cuda`, `nvidia`, `rocm`, `mps`) | 1 GPU |
| `gpus:N` | N GPUs on the same machine (DDP, model parallelism) |
| `vram:N` | only GPUs with at least N GB memory |
| `blackwell`, `hopper`, `ada`, `ampere`, `cdna3`, … | only GPUs of that architecture |
| `sm_90` / `sm>=80` | exact / minimum NVIDIA compute capability |
| `gfx942` | that AMD target |

- The session sees only its GPUs (`CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES`), and its prompt says which GPUs it has, how many the machine has, and how to use them (e.g. `torchrun --nproc_per_node=N`, Docker `--gpus "device=…"`).
- Agents are told which architectures to compile for (`TORCH_CUDA_ARCH_LIST`, `CMAKE_CUDA_ARCHITECTURES`, `nvcc -gencode …`, `--offload-arch`).
- The smallest suitable GPUs are used first, keeping big ones free for big jobs. An issue waits until enough GPUs are free.
- Sessions that didn't ask for a GPU only see GPUs that were idle when they started, and are told those aren't reserved for them.
- Agents know every machine's GPU count and are encouraged to split work into 1-GPU issues (e.g. sweeps) when possible.
- *Machines* shows each GPU with the issue using it; **reserve** takes a single GPU out of scheduling (e.g. for your own work). Set *Parallel agents* to at least the number of GPUs so all can be used.

### Several repositories

A project starts with one repository, `/git/<slug>.git`. Add more under *Settings → Repositories & mirrors* (empty or imported from a URL); each is served at `/git/<slug>/<name>.git` with LFS, and the *Code* tab has a repository picker.

- Agents get **all repositories checked out side by side** (`<workspace>/<repo>/`), with each repository's description and default branch in their prompt.
- An issue uses branch `issue-N` in every repository it changes. On approval the server merges **all of them together, or none**: if one conflicts, nothing is merged and the issue goes back with instructions.
- Runners older than this feature only check out the primary repository; they update themselves automatically once the server runs a release.

### GitHub mirrors

Each repository can have mirrors: one-way copies the server pushes to after every push and merge (and retries every 10 minutes). Agents are told about them and never push to them directly.

- **Target**: `owner/name` (GitHub) or any `https://…git` URL. MultiClaude can create the GitHub repository for you, private or public.
- **Credentials**: a server-wide GitHub token (*Users & account → GitHub*; only ever sent to github.com) or a token per mirror/import. Fine-grained tokens need *Contents: read & write*, plus *Administration: read & write* to create repositories.
- **Mode**:
  - *two-way sync* (default): before every push the server fetches the mirror's default branch. New commits there (e.g. pull requests merged on GitHub) are fast-forwarded or merged in, and announced in the project chat. If both sides conflict, the mirror's state is put on branch `upstream/<branch>` and a high-priority issue asks an agent to merge it; pushing resumes once that is merged.
  - *push only*: default branch + tags, fast-forward only; fails if the mirror has commits the server doesn't.
  - *push all (forced)*: every branch and tag, overwriting the mirror.
- **Taking over an existing GitHub project**: create the project with *Import existing git repository* = `owner/name` and a token, and leave *Keep it in sync both ways* checked. History, branches, tags and LFS objects are imported, the default branch is taken over, and the GitHub repository stays the public face of the project. Add further repositories (e.g. a private enterprise one) the same way under *Settings → Repositories & mirrors*.
- **Visibility** is detected; for **public** mirrors agents are warned that everything they commit becomes public.

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

Everything the UI does goes through a JSON API under `/api/…` (see `internal/server/server.go` for the routes), so you can script it with Basic auth, e.g.:

```sh
curl -u admin:PASSWORD -XPOST https://<server>/api/projects/my-game/issues -d '{"title":"Fix jump","priority":"high","requires":["gpu"]}'
```

## Limits & notes

- Releases: `make release V=v0.2.0` tags and pushes; GitHub Actions tests, builds the multi-arch image and creates the release.
- All accounts have full access (no roles). Agents can use their machine's token, so only enroll machines you trust.
- Runners support Linux and macOS. On Windows, use WSL.
- The cost shown is Claude Code's API-equivalent estimate. On subscriptions it's informational only.
