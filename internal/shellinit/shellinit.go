// Package shellinit generates the shell integration that `mu setup shell-init`
// prints for `eval "$(mu setup shell-init)"` at shell startup. It emits a per-node
// dispatcher plus one thin wrapper per configured node, all driven by the same
// config.toml the engine reads — so adding a node in the config makes its command
// appear, with no hand-maintained alias codegen.
package shellinit

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	shellassets "github.com/mayhl/mayhl_utils"
	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/doctor"
	"github.com/mayhl/mayhl_utils/internal/modules"
)

// The dispatcher: bare `<node>` connects (interactive ssh), `<node> push|pull`
// transfer via the engine, and `<node> <anycmd>` runs that command over ssh.
//
// A handful of words are RESERVED for the mu verbs that take a --node anyway, so the node
// name reads as the target it already is: `hpc1 shell` is `mu job shell -N hpc1`, and
// likewise sub/tunnel (mu job) and queues/usage/storage (mu hpc). The cost is that those
// words can no longer name a remote command — `hpc1 usage` will not run a program called
// `usage` — so `<node> exec <cmd>` (or `--`) forces remote-exec for anything, reserved or
// not. The bare `<node> <anycmd>` fallback stays: it is the older idiom and the muscle
// memory. FUTURE: if the reserved set keeps growing, make `exec` the ONLY way to run an
// arbitrary command and drop the fallback — the shadowing is what forces the choice.
//
// A leading pure-integer arg pins a numbered login node — `<node> N` rewrites the
// target's node segment to `<node>NN` (zero-padded, per the 01,02,… convention),
// then the rest of the grammar composes (bare → connect, else → remote-exec).
// push/pull stay node-level (mu cp resolves its own target). It leans on the shell
// framework's seam helpers (mu_auth, mu_ssh_login, $MU_SSH).
//
// The numeric guard uses a portable `case *[!0-9]*` test (no extglob) so it works
// in both bash and zsh. `<node> -h|--help` calls back into mu (`mu setup node-help`) so the
// grammar renders through the house help panels — generated shell can't reach the renderer,
// and a printf was the one help text in mu that didn't look like mu.
//
// Remote-exec runs `bash -lc` (login shell) so HPC modules/scheduler load from
// /etc/profile.d — which `zsh -l` does NOT source. The joined args are passed
// through printf %q so the remote outer shell unescapes exactly once and bash -lc
// parses the verbatim command line — quotes/parens/$ in args survive, and globs
// expand REMOTELY, not at the remote outer-shell layer (a bare "\"$*\"" wrapper
// double-expanded and broke on embedded quotes). That login profile spews a
// benign dbus/X11 error over non-interactive ssh; it's filtered from stderr via
// MU_SSH_STDERR_FILTER (default drops that message; process substitution keeps the
// command's exit code and lets real errors through). TEMPORARY workaround for the
// cluster's /etc/profile.d — see the dbus-filter note.
//
// No leading underscore on these two, and don't add one: to zsh a `_name` function is a
// COMPLETION function, and tooling that snapshots-and-replays a shell filters them out on that
// convention — which dropped the helper while keeping the dispatchers that call it, leaving
// every node shortcut a `command not found`. They're plain helpers; they take the plain `mu_`
// prefix the rest of the layer uses.

// nodeVerb is one node-targeted capability: its bare `<node> <verb>` name and the `mu` command
// it runs. classAVerbs is the SINGLE source that generates all three invocation surfaces — the
// m-door `m<verb>`, the `<node> <verb>` dispatcher arm, and the binary's own -N — so the three
// can never drift.
type nodeVerb struct{ verb, path string }

// classAVerbs lists the node-targeted verbs in canonical order, the queue/kill pair resolved to
// the site's idiom ([shell] queue_aliases): stat/del on pbs, queue/cancel on slurm, both on
// "both". The rest are idiom-neutral. Every entry's `mu` command takes --node (verified against
// the queueTargetCtx / -N surface), so `<node> <verb>` == `m<verb> -N <node>` == `mu <path> -N`.
func classAVerbs() []nodeVerb {
	var v []nodeVerb
	switch config.QueueAliases() {
	case "slurm":
		v = append(v, nodeVerb{"queue", "hpc queue"}, nodeVerb{"cancel", "hpc queue kill"})
	case "both":
		v = append(v, nodeVerb{"stat", "hpc queue"}, nodeVerb{"del", "hpc queue kill"},
			nodeVerb{"queue", "hpc queue"}, nodeVerb{"cancel", "hpc queue kill"})
	default: // pbs
		v = append(v, nodeVerb{"stat", "hpc queue"}, nodeVerb{"del", "hpc queue kill"})
	}
	return append(
		v,
		nodeVerb{"info", "hpc queue info"}, nodeVerb{"peek", "hpc queue peek"},
		nodeVerb{"hold", "hpc queue hold"}, nodeVerb{"rls", "hpc queue release"}, // rls ← qrls
		nodeVerb{"hist", "hpc queue hist"},
		nodeVerb{"queues", "hpc queues"}, nodeVerb{"storage", "hpc storage"}, nodeVerb{"usage", "hpc usage"},
		nodeVerb{"shell", "job shell"}, nodeVerb{"sub", "job sub"}, nodeVerb{"tunnel", "job tunnel"},
	)
}

// nodeDispatcher builds the `<node> <verb>` dispatcher. The node-targeted arms are GENERATED
// from classAVerbs; the surrounding arms are node-INTRINSIC and stay hand-written — cp push/pull,
// exec/-- forced remote-exec, a bare/numbered node → ssh login, -h → the house panel. No arm body
// contains a single quote (Generate emits this OUTSIDE the eval dance, but keep it clean anyway).
func nodeDispatcher() string {
	var arms strings.Builder
	for _, v := range classAVerbs() {
		fmt.Fprintf(&arms, "    %s) shift; mu %s --node \"$node\" \"$@\" ;;\n", v.verb, v.path)
	}
	return dispatcherHead + arms.String() + dispatcherTail
}

const dispatcherHead = `mu_node_help() {
  mu setup node-help "$1"
}
mu_node() {
  local node=$1 target=$2; shift 2
  case ${1:-} in
    -h|--help) mu_node_help "$node" ;;
    push) shift; mu cp push "$node" "$@" ;;
    pull) shift; mu cp pull "$node" "$@" ;;
`

const dispatcherTail = `    exec|--) shift; mu_auth && ${MU_SSH:-ssh} -q "$target" "bash -lc $(printf '%q' "$*")" 2> >(grep -vE "${MU_SSH_STDERR_FILTER:-dbus-update-activation-environment|^Cannot continue}" >&2) ;;
    "")   mu_auth && mu_ssh_login "$target" ;;
    *)
      case $1 in
        ''|*[!0-9]*) : ;;
        *) target="${target%%.*}$(printf '%02d' "$1").${target#*.}"; shift ;;
      esac
      if [ "$#" -eq 0 ]; then
        mu_auth && mu_ssh_login "$target"
      else
        mu_auth && ${MU_SSH:-ssh} -q "$target" "bash -lc $(printf '%q' "$*")" 2> >(grep -vE "${MU_SSH_STDERR_FILTER:-dbus-update-activation-environment|^Cannot continue}" >&2)
      fi
      ;;
  esac
}
`

// Generate returns the shell code to eval at startup: the config as exports (so
// legacy shell consumers — status.sh, mu_kitty_bootstrap — work with config.env
// gone), then the per-node dispatchers. The current system ($MU_NODE, else
// $BC_HOST) is skipped — no self ssh/cp dispatcher.
// NodeDoorNames returns the `<node>` front-door command names (every configured node but self)
// — the commands the zsh _mu_node completer is registered on. Mirrors the door set Generate
// defines, so the completion can never target a door that does not exist.
func NodeDoorNames() []string {
	self := selfNode()
	var nodes []string
	for _, n := range config.NodeNames() {
		if n != self {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// NodeVerbs returns the `<node> <verb>` verbs for completion: the path-taking pull/push (hand-
// written dispatcher arms) plus the generated class-A verbs, in dispatcher order. The zsh
// completer offers these after a bare node and drives remote-path completion on pull/push.
func NodeVerbs() []string {
	verbs := []string{"pull", "push"}
	for _, v := range classAVerbs() {
		verbs = append(verbs, v.verb)
	}
	return verbs
}

func Generate() string {
	targets := config.NodeTargets()

	nodes := NodeDoorNames()

	var b strings.Builder
	b.WriteString("# mayhl_utils shell integration — generated by `mu setup shell-init` (do not edit)\n")
	b.WriteString(configExports())
	b.WriteString(miseEnv())
	b.WriteString(platformSeam())
	b.WriteString(sharedTooling())
	b.WriteString(clipTools())
	b.WriteString(doctorCheckup())
	b.WriteString(nodeDispatcher())
	// Clear any stale same-named aliases (e.g. a leftover bare-node ssh alias from
	// the old connect.sh codegen), then define the dispatchers inside a NESTED
	// eval. zsh parses a whole eval block up-front and refuses a function def over
	// a live alias — the nested eval defers parsing the defs until after the
	// unalias has executed. (No dispatcher body contains a single quote.)
	if len(nodes) > 0 {
		fmt.Fprintf(&b, "unalias %s 2>/dev/null || :\n", strings.Join(nodes, " "))
		b.WriteString("eval '\n")
		for _, n := range nodes {
			fmt.Fprintf(&b, "%s() { mu_node %s \"%s\" \"$@\"; }\n", n, n, targets[n])
		}
		b.WriteString("'\n")
	}
	b.WriteString(frontDoors())
	return b.String()
}

// frontDoors emits the short shell front-doors for the process, log, and queue planes:
// mps/mkill/mlog (process + event log — always) and the queue pair under the configured idiom
// (config.toml [shell] queue_aliases) — mstat/mdel (pbs, default), mqueue/mcancel
// (slurm), or both. Bare `mstat` runs the CURRENT cluster's scheduler locally (mu hpc
// queue, no --node); `<node> mstat` routes through the per-node dispatcher above. The
// queue pair is emitted only when a cluster is configured; mps/mkill always. Each uses
// the unalias + nested-eval dance the dispatchers use — zsh won't define a function
// over a live alias, and the nested eval defers parsing the def until the unalias runs
// (no body contains a single quote).
func frontDoors() string {
	type door struct{ name, body string }
	// Class C — nodeless local planes (m-door + binary; no `<node> <verb>` form).
	doors := []door{
		{"mps", `mu ps "$@"`},
		{"mkill", `mu ps kill "$@"`},
		{"mlog", `mu log "$@"`},
		{"mcfg", `mu config "$@"`},
	}
	if len(config.NodeNames()) > 0 {
		// Class A — GENERATED from classAVerbs, the SAME table nodeDispatcher() reads, so the
		// m-door `m<verb>` and the `<node> <verb>` arm can never drift. The idiom (queue_aliases)
		// resolves stat/del vs queue/cancel in that one place, so it flows through here for free.
		for _, v := range classAVerbs() {
			doors = append(doors, door{"m" + v.verb, "mu " + v.path + ` "$@"`})
		}
		// Curated exceptions the systematic pattern can't hold:
		//   mharness — `harness run` takes an id positional, not --node.
		//   mlogin   — `harness login` takes -N, but `<node> login` would collide with the
		//              bare-node ssh login, so it stays m-door-only.
		//   hpcs     — Class D: a cross-cluster overview (`mu hpc nodes` iterates ALL clusters,
		//              takes no --node), so the node-verb grammar doesn't apply; the descriptive
		//              name (plural of HPC = systems) is deliberate, not a prefix slip.
		doors = append(
			doors,
			door{"mharness", `mu job harness run "$@"`},
			door{"mlogin", `mu job harness login "$@"`},
			door{"hpcs", `mu hpc nodes "$@"`},
		)
	}
	// Project plane (project module): swap cds into a mirror (the one door that must be shell —
	// capture-then-cd so a failed resolve leaves the shell put); mruns lists runs; archive
	// shadows the site PST/TUSC binary (mu resolves the real one from PATH, so no recursion).
	if modules.Enabled("project") {
		doors = append(doors, door{"swap", `local d; d=$(mu path swap "$@") && cd "$d"`})
		if _, err := exec.LookPath("archive"); err == nil {
			doors = append(doors, door{"archive", `mu archive "$@"`})
		}
		doors = append(doors, door{"mruns", `mu project runs "$@"`})
	}

	names := make([]string, len(doors))
	for i, d := range doors {
		names[i] = d.name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unalias %s 2>/dev/null || :\n", strings.Join(names, " "))
	b.WriteString("eval '\n")
	for _, d := range doors {
		fmt.Fprintf(&b, "%s() { %s; }\n", d.name, d.body)
	}
	b.WriteString("'\n")
	return b.String()
}

// configExports mirrors config.toml back into the MU_* environment for the legacy
// shell code that still reads it (mu_auth, mu_status). The Go engine reads
// config.toml directly; this is the bridge that replaced config.env. MU_SSH stays
// a platform seam. TODO: retire this bridge once those shell consumers call `mu`.
func configExports() string {
	var b strings.Builder
	// Mode (local/hpc), derived from MU_SYSTEM/$BC_HOST. init.sh used to export this; the
	// binary owns it now so consumers (the .config web-search gate, mise MISE_ENV) still see
	// it once init.sh is retired. Unguarded — always set, cheap.
	sys := "local"
	if onHPC() {
		sys = "hpc"
	}
	fmt.Fprintf(&b, "export MU_SYSTEM=%q\n", sys)
	if u := config.HPCUser(); u != "" {
		fmt.Fprintf(&b, "export MU_HPC_UNAME=%q\n", u)
	}
	defs := config.ClusterDefs()
	names := make([]string, 0, len(defs))
	for _, c := range defs {
		names = append(names, c.Name)
		cu := strings.ToUpper(c.Name)
		fmt.Fprintf(&b, "export MU_CLUSTER_%s_DOMAIN=%q\n", cu, c.Domain)
		fmt.Fprintf(&b, "export MU_CLUSTER_%s_NODES=%q\n", cu, strings.Join(c.Nodes, " "))
	}
	fmt.Fprintf(&b, "export MU_CLUSTERS=%q\n", strings.Join(names, " "))
	fmt.Fprintf(&b, "export MU_HPC_RSYNC_OPTS=%q\n", config.RsyncOpts())
	fmt.Fprintf(&b, "export MU_SSH_TRANSFER_OPTS=%q\n", config.SSHTransferOpts())
	fmt.Fprintf(&b, "export MU_SSHFS_ROOT=%q\n", config.SSHFSRoot())
	// ossh binary path (machine-specific) → the platform seam reads it (MU_OSSH) to
	// override MU_SSH. Only emitted when configured; unset → the seam uses plain ssh.
	if p := config.OSSHPath(); p != "" {
		fmt.Fprintf(&b, "export MU_OSSH=%q\n", p)
	}
	return b.String()
}

// miseEnv composes MISE_ENV ahead of any mise activation, in portable sh so bash and
// zsh onboard targets get the same tiers (this lived zsh-only in .config's
// zsh.tools.zsh, which now keeps just the activation). Runtime conditionals, not
// generation-time: MU_TOOLCHAIN comes from a `module load mu-toolchain` and varies per
// shell. Comma-composes so a pre-set MISE_ENV survives. Emitted right after
// configExports so its MU_SYSTEM export is already in force.
//
//	hpc — the nvim toolchain stack (config.hpc.toml), HPC boxes only; SKIPPED when the
//	shared mu-toolchain module provides it (its modulefile setenvs MU_TOOLCHAIN), so a
//	per-user fmt opt-in never re-installs module-owned base+hpc.
//	fmt — opt-in formatter/lint/DAP enforcement via MU_MODULES containing fmt.
//	MU_MODULES is a space/comma list (the modules.Enabled contract) — normalize the
//	commas so `fmt git` and `git,fmt` both match.
func miseEnv() string {
	return `if [ -z "${MU_TOOLCHAIN:-}" ] && { [ -n "${BC_HOST:-}" ] || [ "${MU_SYSTEM:-}" = "hpc" ]; }; then
  export MISE_ENV="${MISE_ENV:+$MISE_ENV,}hpc"
fi
case " $(printf '%s' "${MU_MODULES:-}" | tr ',' ' ') " in *" fmt "*) export MISE_ENV="${MISE_ENV:+$MISE_ENV,}fmt" ;; esac
case " $(printf '%s' "${MU_MODULES:-}" | tr ',' ' ') " in *" cast "*) export MISE_ENV="${MISE_ENV:+$MISE_ENV,}cast" ;; esac
`
}

// clipTools emits the OSC 52 clipboard pair, portable sh (bash+zsh). The terminal
// itself carries the copy back over SSH — no X11, no xclip, works from any login
// node. mu_clip: args or stdin → local clipboard (tmux needs the DCS passthrough
// wrap). mu_paste: OSC 52 reads are terminal-gated for security, so it leans on
// kitty's kitten and errors elsewhere. pbcopy/pbpaste get defined only where the
// real ones don't exist (mac muscle memory on the clusters). MU_CLIP_TTY overrides
// the /dev/tty target (tests; no controlling terminal → fall back to stdout).
func clipTools() string {
	return `mu_clip() {
  _mu_b64=$({ if [ $# -gt 0 ]; then printf '%s' "$*"; else cat; fi; } | base64 | tr -d '\n')
  if [ -n "${TMUX:-}" ]; then
    _mu_osc=$(printf '\033Ptmux;\033\033]52;c;%s\a\033\\' "$_mu_b64")
  else
    _mu_osc=$(printf '\033]52;c;%s\a' "$_mu_b64")
  fi
  printf '%s' "$_mu_osc" > "${MU_CLIP_TTY:-/dev/tty}" 2>/dev/null || printf '%s' "$_mu_osc"
  unset _mu_b64 _mu_osc
}
mu_paste() {
  if command -v kitten > /dev/null 2>&1; then
    kitten clipboard --get-clipboard
  else
    echo "mu_paste: OSC 52 reads are terminal-gated — needs kitty's kitten" >&2
    return 1
  fi
}
command -v pbcopy > /dev/null 2>&1 || pbcopy() { mu_clip "$@"; }
command -v pbpaste > /dev/null 2>&1 || pbpaste() { mu_paste "$@"; }
`
}

// platformSeam emits the shell support libs (compat/log) and the connectivity seam
// (MU_SSH / mu_ssh_login / mu_auth) so the binary is self-sufficient: a box needs no
// mayhl_utils checkout for the node dispatchers (which call these helpers) to work. The
// blocks are the embedded lib/*.sh + platform/*.sh verbatim, each guarded so a dev box
// whose init.sh already sourced them is a no-op — and since the emitted text IS the
// embedded source, any redefinition is byte-identical. hpc vs local seam is picked by the
// detected mode; configExports() (which sets MU_OSSH) is emitted before this.
func platformSeam() string {
	seam := shellassets.PlatformLocalSH
	if onHPC() {
		seam = shellassets.PlatformHPCSH
	}
	var b strings.Builder
	// support libs (mu_log/mu_have/mu_indirect/…) — the seam + dispatchers depend on them.
	b.WriteString("command -v mu_log >/dev/null 2>&1 || {\n")
	b.WriteString(shellassets.CompatSH)
	b.WriteString(shellassets.LogSH)
	b.WriteString("}\n")
	// connectivity seam (MU_SSH + mu_ssh_login + mu_auth).
	b.WriteString("command -v mu_ssh_login >/dev/null 2>&1 || {\n")
	b.WriteString(seam)
	b.WriteString("}\n")
	return b.String()
}

// sharedTooling emits the portable shared helpers (tar shims qtar/gtar/bqtar/bgtar,
// status mu_status/mu_ctx, and gkill/qffmpeg/mytb/mu_run) so they exist on a binary-only
// box too. Guarded on mu_run (unique to utils.sh) so a dev box whose init.sh already
// sourced shared/*.sh is a no-op — the emitted text is the embedded source, so any
// redefinition is byte-identical. Depends on compat/log (platformSeam) + MU_* exports
// (configExports), both emitted above. Cosmetic aliases (vim/EDITOR) stay in .config.
func sharedTooling() string {
	var b strings.Builder
	b.WriteString("command -v mu_run >/dev/null 2>&1 || {\n")
	b.WriteString(shellassets.TarSH)
	b.WriteString(shellassets.StatusSH)
	b.WriteString(shellassets.UtilsSH)
	b.WriteString("}\n")
	return b.String()
}

// doctorCheckup emits the throttled health check: at most one background `mu doctor
// --checkup` per doctor.CheckupEvery, launched disowned so startup never waits on it.
// The fast path is builtin-only reads (`date +%s` is the one exec). A WARN/FAIL run
// leaves doctor.notice, shown at the NEXT shell start via zsh/sched (zsh) or a one-shot
// PROMPT_COMMAND (bash) — NOT inline during sourcing, which trips p10k's instant-prompt
// guard; the sched'd print lands after p10k restores the tty, so it escapes the capture
// buffer and prints cleanly above the prompt. A healthy run clears it. mu re-checks
// the stamp, so racing shells collapse to one run. Paths mirror doctor's Stamp/Notice.
// INTERACTIVE shells only (case $-): notices are for humans, and a non-interactive
// eval's stdout is often captured — `ssh host bash -lc …`, $(…) — where the notice
// would corrupt the captured output.
func doctorCheckup() string {
	return fmt.Sprintf(`case $- in *i*)
_mu_dc="${XDG_CACHE_HOME:-$HOME/.cache}/mayhl_utils"
if [ -r "$_mu_dc/doctor.notice" ]; then
  # Show the nag WITHOUT tripping Powerlevel10k's instant-prompt guard. p10k captures
  # stdout into a hidden buffer from its preamble through the WHOLE precmd chain, and
  # restores the real tty only from a zsh/sched task that fires after precmd — so
  # neither an inline cat nor a precmd escapes the buffer. zsh: sched the print from a
  # precmd, which runs AFTER p10k's teardown-scheduler precmd, so our sched entry is
  # queued after p10k's; p10k restores the tty first, then we print straight to it (no
  # capture, no warning). bash has no instant prompt, so a PROMPT_COMMAND one-shot does.
  # The notice file persists until a healthy checkup clears it, so it re-nags each shell.
  _mu_doctor_notice() {
    local _f="${XDG_CACHE_HOME:-$HOME/.cache}/mayhl_utils/doctor.notice"
    [ -r "$_f" ] && cat "$_f"
  }
  if [ -n "${ZSH_VERSION:-}" ]; then
    zmodload zsh/sched 2>/dev/null
    autoload -Uz add-zsh-hook
    _mu_doctor_notice_arm() {
      sched +0 _mu_doctor_notice
      add-zsh-hook -d precmd _mu_doctor_notice_arm
      unset -f _mu_doctor_notice_arm
    }
    add-zsh-hook precmd _mu_doctor_notice_arm
  else
    _mu_doctor_notice_once() {
      _mu_doctor_notice
      PROMPT_COMMAND="${PROMPT_COMMAND//_mu_doctor_notice_once;/}"
      unset -f _mu_doctor_notice_once
    }
    PROMPT_COMMAND="_mu_doctor_notice_once;${PROMPT_COMMAND}"
  fi
fi
_mu_dt=0
[ -r "$_mu_dc/doctor.stamp" ] && read -r _mu_dt < "$_mu_dc/doctor.stamp" || :
case $_mu_dt in ''|*[!0-9]*) _mu_dt=0 ;; esac
if [ $(( $(date +%%s) - _mu_dt )) -ge %d ]; then
  (mu doctor --checkup >/dev/null 2>&1 &)
fi
unset _mu_dc _mu_dt
;; esac
`, int(doctor.CheckupEvery/time.Second))
}

// onHPC reports whether shell-init should emit the HPC seam: an explicit MU_SYSTEM wins,
// else $BC_HOST (set on HPC login/compute nodes) marks HPC. Mirrors init.sh's derivation.
func onHPC() bool {
	if s := os.Getenv("MU_SYSTEM"); s != "" {
		return s == "hpc"
	}
	return os.Getenv("BC_HOST") != ""
}

// selfNode is the node this shell is running on, if any (compute/login nodes set
// $BC_HOST reliably; $MU_NODE is an explicit override).
func selfNode() string {
	if n := os.Getenv("MU_NODE"); n != "" {
		return n
	}
	return os.Getenv("BC_HOST")
}
