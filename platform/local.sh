#!/usr/bin/env sh
# platform/local.sh — LOCAL workstation (e.g. macOS laptop).
#
# SLICE 1 scope: the connectivity SEAM only (MU_SSH + mu_auth + Kerberos PATH).
# Later slices add the local-only tools here: sshfs mount/hcd, pyenv, kitty was
# dropped. connect.sh consumes MU_SSH/mu_auth and never branches on platform.

# Kerberos client tools + the `ossh` OpenSSH build on PATH.
# Prepend only once so re-sourcing .zshrc does not bloat PATH.
case ":$PATH:" in
  *:/usr/local/ossh/bin:*) ;;
  *) export PATH="/usr/local/krb5/bin:/usr/local/ossh/bin:$PATH" ;;
esac
export KRB5_CONFIG="${KRB5_CONFIG:-/etc/krb5.conf}"

# ssh binary: plain `ssh` by default; the Kerberos `ossh` build overrides it when
# configured. `ossh` is opt-in via config.toml ([ssh] ossh) → exported as MU_OSSH by
# `mu shell-init` (which runs before this seam). No `command -v ossh` probe: a box
# without ossh configured just uses ssh. Authoritative — this module owns the seam.
if [ -n "${MU_OSSH}" ] && [ -x "${MU_OSSH}" ]; then
  MU_SSH="${MU_OSSH}"
else
  MU_SSH=ssh
fi
export MU_SSH

# Interactive-login ssh: inside kitty, wrap with the ssh kitten (shell
# integration / clipboard / terminfo over ssh). It execs the `ssh` on PATH,
# which is the ossh build, so Kerberos is preserved. File transfers (rsync -e)
# keep the plain MU_SSH — the kitten's session setup is pointless there.
# A function (not a string var) so the multi-word command survives unquoted
# use without relying on word-splitting — zsh doesn't split ${var} by default.
if [ -n "${KITTY_WINDOW_ID}" ] && mu_have kitty; then
  mu_ssh_login() { kitty +kitten ssh "$@"; }
else
  mu_ssh_login() { "$MU_SSH" "$@"; }
fi

# auth hook: obtain a Kerberos ticket if none is present for the HPC user.
mu_auth() {
  if ! klist 2> /dev/null | grep -q "${MU_HPC_UNAME}"; then
    mu_log "INFO" "No Kerberos ticket for ${MU_HPC_UNAME}; running pkinit..."
    pkinit "${MU_HPC_UNAME}"
  fi
}

# pyenv (Python version management) when installed.
if mu_is_macos; then
  export PYENV_ROOT="${HOME}/.pyenv"
  case ":$PATH:" in
    *":${PYENV_ROOT}/bin:"*) ;;
    *) [ -d "${PYENV_ROOT}/bin" ] && export PATH="${PYENV_ROOT}/bin:$PATH" ;;
  esac
  mu_have pyenv && eval "$(pyenv init -)"
fi

# One-time-per-HPC setup: push kitty's terminfo to the HPC systems so kitty
# renders correctly over ssh (parallels mu_py_bootstrap). Local-only (kitty is
# your terminal). One push per node — each HPC system has its own $HOME.
# Driven by MU_CLUSTERS.
mu_kitty_bootstrap() {
  mu_have kitty || {
    mu_log "ERROR" "kitty not found"
    return 1
  }
  mu_auth
  local c cu domain nodes node target
  for c in $(echo "$MU_CLUSTERS"); do
    cu=$(printf '%s' "$c" | tr '[:lower:]' '[:upper:]')
    domain=$(mu_indirect "MU_CLUSTER_${cu}_DOMAIN")
    nodes=$(mu_indirect "MU_CLUSTER_${cu}_NODES")
    [ -n "$domain" ] || continue
    for node in $(echo "$nodes"); do
      target="${MU_HPC_UNAME}@${node}.${domain}"
      mu_log "INFO" "kitty terminfo -> ${target}"
      if timeout 15 kitty +kitten ssh "$target" true; then
        mu_log "OK" "${node}: terminfo updated"
      else
        mu_log "ERROR" "${node}: terminfo push failed"
      fi
    done
  done
}

# --- sshfs mounts (local-only; `mu sshfs` is the engine, only hcd mounts) -----
# The mount name is the only handle; the local dir is the tool's business.
# hcd [flags] <name>   mount (if needed) + cd into it;  no arg -> list.
# Flags (e.g. -v) pass through to `mu sshfs mount`; the non-flag arg is the name.
# An @group mounts the group but stays put — there's no single dir to cd into.
hcd() {
  [ $# -eq 0 ] && {
    mu sshfs list
    return
  }
  mu sshfs mount "$@" || return
  local a name=
  for a in "$@"; do case "$a" in -* | @*) ;; *) name=$a ;; esac done
  [ -n "$name" ] && cd "$(mu sshfs path "$name")"
}
hmt() { mu sshfs mount "$@"; }        # mount, no cd; hmt <name>… / hmt @group / hmt --all
hadd() { mu sshfs add "$@"; }         # hadd <name> <node> <remote-path>
hset() { mu sshfs set "$@"; }         # hset <name> [--node|--path|--ro|--rw]
hum() { mu sshfs umount "$@"; }       # unmount; hum <name>… / hum @group / hum --all
hgroup() { mu sshfs group "$@"; }     # hgroup <group> <name>…  add mounts to a group
hungroup() { mu sshfs ungroup "$@"; } # hungroup <group> <name>…  remove
hgroups() { mu sshfs groups "$@"; }   # list groups; hgroups rename <old> <new> / rm <group>
alias hls='mu sshfs list'             # table with live status
