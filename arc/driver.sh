# arc remote driver: fed to `bash -s` over ssh on every call, never installed.
# arc prepends ARC_ACTION (run, log, kill, list or close), ARC_WINDOW (except
# for list) and, depending on the action: ARC_CMD_B64 (the user's command,
# base64), ARC_WAIT (seconds), ARC_TAIL (a line count, or empty for "only new
# output"), ARC_FORCE (1 or empty). Optional: ARC_KEEP_HEAD and ARC_KEEP_TAIL,
# the bytes kept from each end of an output stream that is too long.
# Window files: DESIGN.md 5.3. Reply format: arc/protocol.py.

arc_sessions=~/.arc/sessions
arc_dir=$arc_sessions/${ARC_WINDOW-}
arc_tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$arc_tmp"' EXIT
: >"$arc_tmp/stdout"
: >"$arc_tmp/stderr"
: "${ARC_KEEP_HEAD:=10240}" "${ARC_KEEP_TAIL:=20480}"
arc_signal= arc_cut_stdout= arc_cut_stderr=

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
# Like a terminal: Ctrl-C ends the command with 130, SIGTERM with 143.
trap 'exit 130' INT
trap 'exit 143' TERM

eval "$(<"$__arc_dir/cmd")"
EOF

# Sets arc_starttime to when pid $1 started (clock ticks since boot, field 22
# of /proc/<pid>/stat), or to empty if there is no such process. Never forks.
arc_read_starttime() {
  local stat fields
  arc_starttime=
  read -r stat 2>/dev/null <"/proc/$1/stat" || return 1
  # Fields after the "(command name)", which may itself contain spaces.
  read -r -a fields <<<"${stat##*) }"
  arc_starttime=${fields[19]}
}

# Is pid $1 still this window's command, and not an unrelated process that
# reused the pid? A reused pid has a different start time.
arc_is_ours() {
  kill -0 "$1" 2>/dev/null || return 1
  [ -d /proc/self ] || return 0
  arc_read_starttime "$1" && [ "$arc_starttime" = "$arc_pidstart" ]
}

# Sets arc_state (exit, running, stopped or idle; DESIGN.md 5.5) and, from the
# window's files, arc_pid, arc_code, arc_cwd, arc_elapsed and arc_cmdline.
arc_inspect() {
  local now
  arc_pid= arc_pidstart= arc_code=0 arc_cwd=$HOME arc_elapsed=0 arc_cmdline=
  [ -f "$arc_dir/pid" ] && read -r arc_pid arc_pidstart <"$arc_dir/pid"
  if [ -f "$arc_dir/exit" ]; then
    arc_state=exit
    arc_code=$(<"$arc_dir/exit")
  elif [ -n "$arc_pid" ] && arc_is_ours "$arc_pid"; then
    arc_state=running
  elif [ -n "$arc_pid" ]; then
    arc_state=stopped
  else
    arc_state=idle
  fi
  [ -f "$arc_dir/cwd" ] && arc_cwd=$(<"$arc_dir/cwd")
  if [ -f "$arc_dir/cmd" ]; then
    arc_cmdline=$(<"$arc_dir/cmd")
    arc_cmdline=${arc_cmdline%%$'\n'*}
    arc_cmdline=${arc_cmdline:0:200}
  fi
  if [ -f "$arc_dir/started" ]; then
    printf -v now '%(%s)T' -1
    arc_elapsed=$((now - $(<"$arc_dir/started")))
  fi
}

# Appends bytes [$2, $2 + $3) of file $1 to file $4.
arc_slice() { tail -c +"$(($2 + 1))" "$1" | head -c "$3" >>"$4"; }

# Writes bytes [$2, $3) of file $1 to $arc_tmp/$4 (stdout or stderr). If there
# are too many, keeps only the first ARC_KEEP_HEAD and last ARC_KEEP_TAIL bytes
# and sets arc_cut_$4 to "<head bytes>:<omitted bytes>:<omitted newlines>".
# Cutting here, on the server, means huge output never crosses the network.
arc_emit() {
  local src=$1 start=$2 end=$3 name=$4 omitted lines
  local dest=$arc_tmp/$name
  : >"$dest"
  printf -v "arc_cut_$name" '%s' ''
  if [ $((end - start)) -le $((ARC_KEEP_HEAD + ARC_KEEP_TAIL)) ]; then
    arc_slice "$src" "$start" $((end - start)) "$dest"
    return
  fi
  omitted=$((end - start - ARC_KEEP_HEAD - ARC_KEEP_TAIL))
  arc_slice "$src" "$start" "$ARC_KEEP_HEAD" "$dest"
  arc_slice "$src" $((end - ARC_KEEP_TAIL)) "$ARC_KEEP_TAIL" "$dest"
  lines=$(tail -c +"$((start + ARC_KEEP_HEAD + 1))" "$src" | head -c "$omitted" | wc -l)
  printf -v "arc_cut_$name" '%s:%s:%s' "$ARC_KEEP_HEAD" "$omitted" "$lines"
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
      head -c "$size" "$src" | tail -n "$ARC_TAIL" >"$arc_tmp/$name.tail"
      arc_emit "$arc_tmp/$name.tail" 0 "$(wc -c <"$arc_tmp/$name.tail")" "$name"
    else
      if [ "$name" = stdout ]; then off=$out_off; else off=$err_off; fi
      [ "$off" -le "$size" ] || off=0
      arc_emit "$src" "$off" "$size" "$name"
    fi
    if [ "$name" = stdout ]; then out_off=$size; else err_off=$size; fi
  done
  [ -d "$arc_dir" ] && printf '%s %s\n' "$out_off" "$err_off" >"$arc_dir/offset"
}

arc_b64() { printf '%s' "$1" | base64 -w0; }

# Prints the reply: a header with status $1 and the values from arc_inspect,
# then whatever stdout and stderr were collected into $arc_tmp.
arc_reply() {
  printf 'ARC1 status=%s code=%d cwd=%s elapsed=%d cmd=%s signal=%s out_cut=%s err_cut=%s out=%d err=%d\n' \
    "$1" "$arc_code" "$(arc_b64 "$arc_cwd")" "$arc_elapsed" "$(arc_b64 "$arc_cmdline")" \
    "$arc_signal" "$arc_cut_stdout" "$arc_cut_stderr" \
    "$(wc -c <"$arc_tmp/stdout")" "$(wc -c <"$arc_tmp/stderr")"
  cat "$arc_tmp/stdout" "$arc_tmp/stderr"
}

# Sleeps 20ms without forking: `read -t` on a fifo that nobody writes to.
arc_nap() {
  if [ ! -p "$arc_tmp/tick" ]; then
    mkfifo "$arc_tmp/tick" && exec 8<>"$arc_tmp/tick"
  fi
  read -r -t 0.02 -u 8
}

# Re-checks "$2" every 20ms while it succeeds, for at most $1 seconds.
arc_while() {
  local ticks=$(($1 * 50))
  while [ "$ticks" -gt 0 ] && "$2"; do
    arc_nap
    ticks=$((ticks - 1))
  done
}

arc_unfinished() { [ ! -f "$arc_dir/exit" ] && kill -0 "$arc_pid" 2>/dev/null; }
arc_group_alive() { kill -0 -- "-$arc_pid" 2>/dev/null; }

# Ctrl-C, then SIGTERM, then SIGKILL to the command's whole process group, each
# time giving it 5 seconds to exit. Sets arc_signal to the last one sent.
arc_stop() {
  local sig
  for sig in INT TERM KILL; do
    arc_group_alive || return
    kill -s "$sig" -- "-$arc_pid" 2>/dev/null
    arc_signal=$sig
    arc_while 5 arc_group_alive
  done
}

# Takes the window's lock; one start, close or similar at a time per window.
# flock is released even if we get killed.
arc_lock() {
  exec 9>"$arc_dir/lock"
  flock -w 5 9
}

arc_run() {
  mkdir -p "$arc_dir" || exit 1
  arc_lock || { arc_inspect; arc_reply busy; return; }
  arc_inspect
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
  # Job control (set -m) gives the command a process group of its own, so it
  # can be stopped as a whole. Without it, bash would start the background
  # command with SIGINT ignored, and Ctrl-C (`arc kill`) could not reach it.
  # Output goes to files and stdin is /dev/null, so the command holds nothing
  # of the ssh connection and keeps running after ssh disconnects.
  set -m
  env ${arc_clean:+-i} __arc_dir="$arc_dir" "$BASH" -c "$arc_window_shell" \
    </dev/null >"$arc_dir/stdout" 2>"$arc_dir/stderr" 9>&- &
  arc_pid=$!
  set +m
  arc_read_starttime "$arc_pid"
  arc_pidstart=$arc_starttime
  echo "$arc_pid $arc_pidstart" >"$arc_dir/pid"
  exec 9>&-

  arc_while "$ARC_WAIT" arc_unfinished
  arc_inspect
  arc_collect
  arc_reply "$arc_state"
}

arc_log() {
  arc_inspect
  arc_collect
  arc_reply "$arc_state"
}

arc_kill() {
  arc_inspect
  [ "$arc_state" = running ] && arc_stop
  arc_inspect
  arc_collect
  arc_reply "$arc_state"
}

# One line per window: name, state, exit code, elapsed seconds, cwd (base64)
# and command (base64), separated by tabs.
arc_list() {
  local d
  for d in "$arc_sessions"/*/; do
    [ -d "$d" ] || continue
    arc_dir=${d%/}
    arc_inspect
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${arc_dir##*/}" "$arc_state" "$arc_code" \
      "$arc_elapsed" "$(arc_b64 "$arc_cwd")" "$(arc_b64 "$arc_cmdline")"
  done >"$arc_tmp/stdout"
  arc_reply list
}

arc_close() {
  if [ ! -d "$arc_dir" ]; then
    arc_inspect
    arc_reply missing
    return
  fi
  arc_lock || { arc_inspect; arc_reply busy; return; }
  arc_inspect
  if [ "$arc_state" = running ]; then
    if [ -z "${ARC_FORCE-}" ]; then
      arc_reply busy
      return
    fi
    arc_stop
    arc_inspect
    if [ "$arc_state" = running ]; then
      arc_reply busy
      return
    fi
  fi
  rm -rf "$arc_dir"
  arc_reply closed
}

case $ARC_ACTION in
  run) arc_run ;;
  log) arc_log ;;
  kill) arc_kill ;;
  list) arc_list ;;
  close) arc_close ;;
  *) echo "arc driver: unknown action $ARC_ACTION" >&2; exit 1 ;;
esac
