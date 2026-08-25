# mayhl_utils

A portable shell + Go toolkit for HPC cluster workflows, run from a local macOS/Linux workstation or an HPC login node.

> **Note that `mu` is a single self-contained Go binary** and works under both bash and zsh. The binary emits its own shell layer (`mu shell-init`), so a machine needs only the `mu` binary plus a one-line rc hook; no source checkout is required. This is how it deploys to an HPC login node.

## Table of Contents
* [Install](#install)
* [Configuration](#config)
* [The mu CLI](#mu)
* [Invocation framework & front-doors](#doors)
* [Setup & lifecycle](#setup)
* [Modules](#modules)
* [Development](#dev)

## Install <a name='install'></a>

Two ways in.

**A fresh box, from a machine that already has `mu`:**

    you@laptop: mu setup onboard <node>

This cross-builds a Linux `mu`, pushes it together with your tracked `.config`, seeds a `config.toml`, and prints the rc hook to add. Only the binary and `.config` land on the target; no repository is cloned and no build is run there.

**Manual setup.** To wire `mu` into your shell, add one line to your `.zshrc` / `.bashrc` (or a file it sources):

    eval "$(mu setup --eval zsh)"      # zsh | bash | fish

That emits the full shell layer (per-node dispatchers, front-doors, tab-completion) at startup. `mu setup --eval` prints it; `mu shell-init` is the same without completion.

Then seed the config and fill it in:

    you@host: cp config.toml.example "$MU_ROOT/config.toml"
    you@host: $EDITOR "$MU_ROOT/config.toml"

`MU_ROOT` points at wherever `config.toml` and the `mu` binary live; typically a source checkout on a dev box, or `~/.config/mu` on a deployed box. `MU_SYSTEM` (local versus hpc) is auto-detected from `$BC_HOST`; set it explicitly only to override. Open a new shell and run `mu hpc nodes` to confirm.

> **NOTE (developers):** on a source checkout, `lib/launcher.sh` gives you `mu rebuild` + build-on-first-use; `make build` / `make build-linux` build the binary directly. See [Development](#dev).

## Configuration <a name='config'></a>

Everything lives in **`config.toml`** (gitignored; it holds your machine's identity, so copy it from the tracked `config.toml.example`). The Go engine reads it directly, and `mu shell-init` exports what the shell layer needs, so it is the single source of truth.

    hpc_user = "your_username"
    fleet    = ["alpha", "beta"]          # optional: the "active" cluster set (mstat --fleet)

    [ssh]
    ossh = "/path/to/ossh/bin/ssh"        # a Kerberos ssh build (if `ssh` is a shell alias); omit for plain ssh

    [sshfs]
    root = "~/hpc_sshfs"                   # local parent dir for sshfs mounts

    [[cluster]]
    name      = "alpha"
    domain    = "alpha.example.mil"
    nodes     = ["node1", "node2"]
    scheduler = "pbs"                      # pbs | slurm; sets the queue idiom

    [[cluster]]
    name         = "beta"
    domain       = "beta.example.mil"
    nodes        = ["node3"]
    scheduler    = "slurm"
    queue_flag   = "qos"                    # SLURM only: queues are QOS values (--qos=), not partitions (-p)
    submit_queue = { default = "standard" } # the queue a flagless submit uses; required on a qos site
    active       = false                    # optional: excluded from the fleet, still reachable via --all

The `[ssh]`/`[sshfs]` tables are per-machine seams (kept in place across `mu setup sync`); everything else is shared inventory. Transfer knobs (`[transfer] rsync_opts`, `ssh_transfer_opts`) have sensible defaults. On a workstation a Kerberos ticket is obtained automatically via `pkinit` when a node command needs it.

**Queue config (per cluster).** The `scheduler` field selects the dialect (PBS `qstat`/`qsub` versus SLURM `squeue`/`sbatch`). A SLURM site that implements its queues as QOS values rather than as partitions sets `queue_flag = "qos"`; mu then routes the queue name through `--qos=` rather than `-p`. Such a site keeps no usable scheduler-side default, so set `submit_queue = { default = "…" }`; otherwise `mu job sub`/`shell`/`tunnel`/`harness` fail with a clear message naming the fix (set `submit_queue`, or pass `-q`). Note that `[shell] queue_aliases`, one of `"pbs"`, `"slurm"`, or `"both"`, selects which queue front-door names the shell layer emits (see below).

## The mu CLI <a name='mu'></a>

`mu` is the engine. Add `-h` to any command for help.

| Command | Does |
|---------|------|
| `mu cp` | copy to/from nodes over rsync, with a live progress bar + summary |
| `mu tar` | create or extract an archive with a progress bar |
| `mu sshfs` | mount HPC dirs locally over sshfs (local only) |
| `mu ps` | list your local processes (`-i` = interactive picker) |
| `mu log` | view the event log (transfers, jobs, big ops) |
| `mu hpc` | cross-cluster info: `nodes`, `queue` (+ `info`/`peek`/`hold`/`release`/`hist`/`kill`), `queues`, `usage`, `storage`, `ticket` |
| `mu job` | interactive + batch jobs: `sub`, `shell`, `tunnel` (port-forward, with `reattach`), `harness` (drive a tmux compute/login pane from a script) |
| `mu project` | mirror a project tree to a cluster and track runs: `sync`, `status`, `runs` |
| `mu setup` | shell wiring + machine lifecycle (onboard / toolchain / sync) |
| `mu doctor` | environment health checks (`setup` / `fmt` / `git`) |
| `mu git` | read-only signwip/pushsigned previews (opt-in — see [Modules](#modules)) |

For example, call `mu cp` directly; the `push`/`pull` front-doors are exactly this:

    you@laptop: mu cp push node1 ./run42 /p/work/me/run42 -n --exclude '*.o'
    you@laptop: mu cp pull node1 /p/work/me/run42/out ./out

Useful `cp` options: `--dry-run`/`-n`, `--exclude PATTERN`, `--exclude-hidden`, `--delete`, `--bwlimit RATE`, `-v`.

## Invocation framework & front-doors <a name='doors'></a>

Every node-targeted capability is available in three equivalent forms; use whichever reads best:

    mu hpc queue -N node1      # canonical: the binary, full flags + completion
    mstat -N node1             # m-door: a short alias for that command
    node1 stat                 # node-first: the node leads, the verb follows

The shell layer generates the m-door and the node-first arm from a single table (`mu shell-init`), so the two cannot drift; that is, `m<verb> -N <node>`, `<node> <verb>`, and `mu <path> -N <node>` all resolve to the same command. Dropping the node targets the current login cluster (`mstat`, or `node1` alone for an ssh login).

Node resolution is shared across the three forms: an explicit node (`-N`, or the leading node word) wins; failing that, the current login cluster (`$MU_NODE`, else `$BC_HOST`); failing that, a listing piped on stdin; otherwise an error. Which forms a verb gets depends on how it relates to a node:

| Class | Node role | Forms | Verbs |
|-------|-----------|-------|-------|
| **node-targeted** | a filter (`-N`) | all three | `stat`·`del`, `info` `peek` `hold` `rls` `hist`, `queues` `storage` `usage`, `shell` `sub` `tunnel` |
| **node-intrinsic** | the object | node-first + binary | `push` `pull`, `exec` / `--`, ssh-login |
| **nodeless** | none | m-door + binary | `ps` `log` `config` |
| **cross-cluster** | all, aggregated | descriptive name | `hpcs` = `mu hpc nodes` |

> **Note that the queue verbs mirror the scheduler's own commands** (`[shell] queue_aliases`): on PBS, `qstat`/`qdel` become `stat`/`del` (`mstat`/`mdel`); on SLURM, `squeue`/`scancel` become `queue`/`cancel` (`mqueue`/`mcancel`); `"both"` emits all four. The neutral state verbs keep the PBS mnemonic across both idioms, i.e., `qhold→mhold`, `qrls→mrls`, `qsub→msub`.

### Per-node (`<node> <verb>`)

One dispatcher per configured node. For `node1`:

| Form | Does |
|------|------|
| `node1` | ssh login (Kerberos handled automatically); `node1 3` → login node **03** |
| `node1 push <src> <dst>` / `node1 pull …` | copy up / down (`mu cp`) |
| `node1 <cmd>` / `node1 exec <cmd>` | run `<cmd>` on node1 over ssh (login shell → modules/`PATH`); `exec` forces it for a reserved word |
| `node1 stat` · `node1 sub run.pbs` · `node1 shell` · `node1 hold <id>` | any node-targeted verb, routed to `mu … --node node1` |

> **Note that** you connect to a single HPC at a time; nested tunnels do not work.

### Queue (`m*` doors)

Scheduler-neutral; the list/cancel pair adapts to the idiom (`mstat`/`mdel` on PBS, `mqueue`/`mcancel` on SLURM):

| Command | Engine | Does |
|---------|--------|------|
| `mstat` | `mu hpc queue` | your jobs; `--all` cross-cluster, `-u <user>`, `-i` inspect |
| `mdel <id>` | `mu hpc queue kill` | cancel a job |
| `minfo` · `mpeek` · `mhold` · `mrls` · `mhist` | `mu hpc queue …` | info · peek out+err · hold · release · finished-history |
| `mqueues` · `musage` · `mstorage` | `mu hpc …` | queues / usage / storage for a cluster |
| `hpcs` | `mu hpc nodes` | all configured systems (cross-cluster overview) |

### Jobs (`m*` doors)

| Command | Engine | Does |
|---------|--------|------|
| `msub <script>` | `mu job sub` | submit a batch job (warns if over `[job] hours_warn`) |
| `mshell` | `mu job shell` | an interactive compute-node shell |
| `mtunnel <script>` | `mu job tunnel` | forward a compute-node port to your laptop; `mtunnel reattach <id>` reopens a dropped one |
| `mharness <id> <cmd>` | `mu job harness run` | run a command in a tmux compute pane opened by `mu job harness open` (`key <id> C-c` recovers a stuck pane) |
| `mlogin <cluster>` | `mu job harness login` | a login-node pane (internet egress) for compile/fetch |

### Process & log (local, always)

| Command | Engine | Does |
|---------|--------|------|
| `mps [mask]` | `mu ps` | list processes; `mps -i` = interactive picker |
| `mkill <sel>` | `mu ps kill` | signal by id/range/name (preview + confirm) |
| `mlog` | `mu log` | the event log |

**SSHFS** (local). Register a mount once, then use the `h*` shortcuts:

| Command | Engine form | Does |
|---------|-------------|------|
| `hadd <name> <node> <path>` | `mu sshfs add` | register a mount (name → node:path) |
| `hcd <name>` | `mu sshfs mount` + `cd` | mount (if needed) and cd in |
| `hmt <name>… \| @group \| --all` | `mu sshfs mount` | mount one/many/a group/all, no cd |
| `hls` | `mu sshfs list` | list mounts with live status + groups |
| `hset <name> [--node\|--path\|--ro\|--rw]` | `mu sshfs set` | repoint or swap ro↔rw (remounts if live) |
| `hum <name> \| --all` | `mu sshfs umount` | unmount one / all live |
| `hgroup` / `hungroup <group> <name>…` | `mu sshfs group` | add/remove mounts to a free-form group |

Every sshfs operation is timeout-bounded and **aborts on a fatal sshfs error** (e.g. a missing remote path) instead of hanging.

`mu sshfs guard install` adds a launchd agent that ticks every 30 s: it **parks** (unmounts) a mount the laptop's AV is traversing — scanner busy, that mount's fuse-t daemons busy, none of your processes in it — and remounts it after a minute (a mount parked 3× in 15 min stays down until you `hmt` it), and unmounts any mount unused for 30 min. It never runs pkinit; `hls` shows guard-parked mounts as `◌ parked (av|idle)`, `mu sshfs guard status` shows what it holds, and `mu sshfs guard -n -v` previews a tick with the samples.

**Tar** (both):

| Command | Engine form | Does |
|---------|-------------|------|
| `qtar <dir\|archive>` | `mu tar` | create → `.tar`, or extract |
| `gtar <dir\|archive>` | `mu tar -z` | create → `.tar.gz`, or extract |
| `bqtar` / `bgtar` | — | background, low-priority (`nice`) variants, logging to `.log` |

## Setup & lifecycle <a name='setup'></a>

`mu setup` covers shell wiring and the machine lifecycle:

| Command | Does |
|---------|------|
| `mu setup shell-init` / `--eval <shell>` | emit the shell layer (the rc hook) |
| `mu setup completion <shell>` | a standalone completion script |
| `mu setup onboard <node>` | cross-build + push `mu` and `.config` to a fresh box; seed `config.toml` |
| `mu setup toolchain` | install the dev toolchain (via mise) |
| `mu setup sync <node>` | push this machine's `config.toml` inventory (keeps the target's `[ssh]`/`[sshfs]`) |
| `mu setup sync <node> --dotfiles` | also git-reconcile the `.config` repo; `sync pull …` reverses either direction |

**`mu doctor`** reports health (each also reachable as `mu <module> doctor`):

| Command | Does |
|---------|------|
| `mu doctor` | built-in environment checks (tools, config, plugins) |
| `mu doctor setup` | shell wiring, toolchain, build freshness, repo drift |
| `mu doctor fmt` | the formatter / linter / debug / LSP matrix (mise-enforced vs editor) |
| `mu doctor git` | git on PATH + the `.config` git-workflow files (opt-in) |

## Modules <a name='modules'></a>

Newer features are opt-in via **`MU_MODULES`** (a space/comma list) in your rc; core commands are always on, and an unlisted module stays inert:

    export MU_MODULES='git fmt'

* **`git`**: `mu git` gives colored, read-only previews of the `signwip` / `pushsigned` / `reviewed` / `doctor` workflow (it never signs or pushes).
* **`fmt`**: turns on the mise formatter/linter enforcement tier (surfaced by `mu doctor fmt`; consumed by nvim).
* **`project`**: the project-mirror plane, providing `mu project sync`/`status`/`runs`, `mu archive`, and the `swap`/`mruns`/`archive` front-doors that navigate a mirrored tree.

## Development <a name='dev'></a>

The engine is pure Go; the `Makefile` wraps the common tasks.

| Command | Does |
|---------|------|
| `make build` / `make build-linux` | native binary / static `linux/amd64` for HPC deploy |
| `make test` | the hermetic suite (`go test ./...`) |
| `make fmt` / `make lint` | gofumpt / golangci-lint |

The default suite is hermetic, requiring no cluster, network, or ssh; the per-node dispatcher test drives the generated shell under both bash and zsh (skipping a shell that is not installed).

A separate **sandbox** rig runs a local Docker box that stands in for an HPC login node, for the end-to-end onboard / cp / queue / shell-layer tests:

    cd internal/integration/sandbox && docker compose up -d      # bring the box up
    go test -tags sandbox ./internal/integration/                # from the repo root; skips cleanly if the box is down

> **Note that** the shell library files (`lib/`, `platform/`, `shared/`) are embedded into the binary (`shellassets.go`) and emitted by `mu shell-init`, so editing them and rebuilding is all it takes to change the shell layer everywhere.
