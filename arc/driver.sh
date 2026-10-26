# arc remote driver: fed to `bash -s` over ssh on every call, never installed.
# arc prepends ARC_ACTION (run or log) and ARC_WINDOW; for run also ARC_CMD_B64
# (the user's command, base64) and ARC_WAIT (seconds); for log also ARC_TAIL
# (a line count, or empty for "only new output").
# Window files: DESIGN.md 5.3. Reply format: arc/protocol.py.

arc_dir=~/.arc/sessions/$ARC_WINDOW
arc_tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$arc_tmp"' EXIT

# The window's shell: restore its saved state, run the command, save the new state.
# The state is cwd, every shell variable (exported or not) and every function.
# All of our own names start with __arc_ so they are never saved as user state.
read -r -d '' arc_window_shell <<'EOF'
export -n __arc_dir
if [ -f "$__arc_dir/vars.sh" ]; then
  . "$__arc_dir/vars.sh" 2>/dev/null
  . "$__arc_dir/funcs.sh" 2>/dev/null
fi
# Keep commands non-interactive and their output unbuffered.
export PAGER=cat GIT_PAGER=cat TERM=dumb DEBIAN_FRONTEND=noninteractive PYTHONUNBUFFERED=1

__arc_oldpwd=${OLDPWD-}
if [ -f "$__arc_dir/cwd" ]; then
  __arc_cwd=$(<"$__arc_dir/cwd")
  if ! cd -- "$__arc_cwd" 2>/dev/null; then
    echo "arc: $__arc_cwd no longer exists, starting in $HOME" >&2
    cd ~
  fi
else
  cd ~
fi
# Our cd must not clobber the user's `cd -` target.
[ -n "$__arc_oldpwd" ] && OLDPWD=$__arc_oldpwd

# EXIT trap so the state is saved even when the command itself calls `exit`.
# The exit file is written last: once it exists, the state is complete.
trap '__arc_rc=$?
  printf "%s" "$PWD" >"$__arc_dir/cwd.new"
  while IFS= read -r __arc_n; do
    case $__arc_n in
      __arc_*|BASH*|COMP_*|PWD|SHLVL|_|PPID|UID|EUID|GROUPS|FUNCNAME|DIRSTACK|PIPESTATUS|\
      LINENO|RANDOM|SRANDOM|SECONDS|HISTCMD|EPOCHSECONDS|EPOCHREALTIME|SHELLOPTS|\
      OPTIND|OPTERR|COLUMNS|LINES|SSH_CLIENT|SSH_CONNECTION|SSH_TTY|SSH_AUTH_SOCK) continue ;;
    esac
    declare -p "$__arc_n" 2>/dev/null
  done < <(compgen -v) >"$__arc_dir/vars.new"
  declare -f >"$__arc_dir/funcs.new"
  mv -f "$__arc_dir/vars.new" "$__arc_dir/vars.sh"
  mv -f "$__arc_dir/funcs.new" "$__arc_dir/funcs.sh"
  mv -f "$__arc_dir/cwd.new" "$__arc_dir/cwd"
  printf "%s" "$__arc_rc" >"$__arc_dir/exit.new"
  mv -f "$__arc_dir/exit.new" "$__arc_dir/exit"
  exit $__arc_rc' EXIT

eval "$(<"$__arc_dir/cmd")"
EOF

# Sets arc_state to exit, running, stopped (died without an exit file, e.g.
# SIGKILL) or idle (never ran anything).
arc_read_state() {
  if [ -f "$arc_dir/exit" ]; then
    arc_state=exit
  elif [ -f "$arc_dir/pid" ] && kill -0 "$(<"$arc_dir/pid")" 2>/dev/null; then
    arc_state=running
  elif [ -f "$arc_dir/pid" ]; then
    arc_state=stopped
  else
    arc_state=idle
  fi
}

# Collects into $arc_tmp the stdout and stderr the agent has not seen yet (or
# their last ARC_TAIL lines), and remembers how far the agent has now read.
arc_collect() {
  local name src off size out_off=0 err_off=0
  [ -f "$arc_dir/offset" ] && read -r out_off err_off <"$arc_dir/offset"
  for name in stdout stderr; do
    src=$arc_dir/$name
    : >"$arc_tmp/$name"
    [ -f "$src" ] || continue
    # Measure first: the command may still be writing.
    size=$(wc -c <"$src")
    if [ -n "${ARC_TAIL-}" ]; then
      head -c "$size" "$src" | tail -n "$ARC_TAIL" >"$arc_tmp/$name"
    else
      if [ "$name" = stdout ]; then off=$out_off; else off=$err_off; fi
      [ "$off" -le "$size" ] || off=0
      tail -c +"$((off + 1))" "$src" | head -c "$((size - off))" >"$arc_tmp/$name"
    fi
    if [ "$name" = stdout ]; then out_off=$size; else err_off=$size; fi
  done
  [ -d "$arc_dir" ] && printf '%s %s\n' "$out_off" "$err_off" >"$arc_dir/offset"
}

arc_b64() { printf '%s' "$1" | base64 -w0; }

# Prints the reply: one header line, then the collected stdout and stderr.
arc_reply() {
  local status=$1 code=0 cwd=$HOME elapsed=0 cmd= now
  [ -f "$arc_dir/cwd" ] && cwd=$(<"$arc_dir/cwd")
  [ -f "$arc_dir/exit" ] && code=$(<"$arc_dir/exit")
  if [ -f "$arc_dir/cmd" ]; then
    cmd=$(<"$arc_dir/cmd")
    cmd=${cmd%%$'\n'*}
    cmd=${cmd:0:200}
  fi
  if [ -f "$arc_dir/started" ]; then
    printf -v now '%(%s)T' -1
    elapsed=$((now - $(<"$arc_dir/started")))
  fi
  [ -f "$arc_tmp/stdout" ] || : >"$arc_tmp/stdout"
  [ -f "$arc_tmp/stderr" ] || : >"$arc_tmp/stderr"
  printf 'ARC1 status=%s code=%d cwd=%s elapsed=%d cmd=%s out=%d err=%d\n' \
    "$status" "$code" "$(arc_b64 "$cwd")" "$elapsed" "$(arc_b64 "$cmd")" \
    "$(wc -c <"$arc_tmp/stdout")" "$(wc -c <"$arc_tmp/stderr")"
  cat "$arc_tmp/stdout" "$arc_tmp/stderr"
}

# Waits up to $1 seconds for the command to finish. Polls every 20ms without
# forking: `read -t` on a fifo that nobody writes to is a builtin sleep.
arc_wait() {
  local ticks=$(($1 * 50)) pid
  pid=$(<"$arc_dir/pid")
  mkfifo "$arc_tmp/tick" && exec 8<>"$arc_tmp/tick"
  while [ ! -f "$arc_dir/exit" ] && kill -0 "$pid" 2>/dev/null && [ "$ticks" -gt 0 ]; do
    read -r -t 0.02 -u 8
    ticks=$((ticks - 1))
  done
  exec 8>&-
}

arc_run() {
  mkdir -p "$arc_dir" || exit 1
  # One start at a time per window. flock is released even if we get killed.
  exec 9>"$arc_dir/lock"
  flock -w 5 9 || { arc_reply busy; return; }
  arc_read_state
  if [ "$arc_state" = running ]; then
    arc_reply busy
    return
  fi

  rm -f "$arc_dir/exit" "$arc_dir/pid"
  printf '%s' "$ARC_CMD_B64" | base64 -d >"$arc_dir/cmd" || exit 1
  printf '%(%s)T' -1 >"$arc_dir/started"
  echo "0 0" >"$arc_dir/offset"
  # Once a window has saved state, start from an empty environment so that
  # variables the user unset stay unset; a new window inherits the login env.
  [ -f "$arc_dir/vars.sh" ] && arc_clean=1
  # setsid: a session and process group of its own, so the command survives
  # ssh disconnecting and can be stopped as a whole. With no job control, the
  # child is not a group leader, so setsid does not fork and $! is its pid.
  # stdin is /dev/null so nothing can sit waiting for input.
  setsid env ${arc_clean:+-i} __arc_dir="$arc_dir" "$BASH" -c "$arc_window_shell" \
    </dev/null >"$arc_dir/stdout" 2>"$arc_dir/stderr" 9>&- &
  echo $! >"$arc_dir/pid"
  exec 9>&-

  arc_wait "$ARC_WAIT"
  arc_read_state
  arc_collect
  arc_reply "$arc_state"
}

arc_log() {
  arc_read_state
  arc_collect
  arc_reply "$arc_state"
}

case $ARC_ACTION in
  run) arc_run ;;
  log) arc_log ;;
  *) echo "arc driver: unknown action $ARC_ACTION" >&2; exit 1 ;;
esac
