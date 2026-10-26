# sshtab remote driver: fed to `bash -s` over ssh on every call, never installed.
# sshtab prepends SSHTAB_ACTION (run, log, kill, list or close), SSHTAB_WINDOW (except
# for list) and, depending on the action: SSHTAB_CMD_B64 (the user's command,
# base64), SSHTAB_WAIT (seconds), SSHTAB_TAIL (a line count, or empty for "only new
# output"), SSHTAB_FORCE (1 or empty). Optional: SSHTAB_KEEP_HEAD and SSHTAB_KEEP_TAIL,
# the bytes kept from each end of an output stream that is too long.
# Window files: DESIGN.md 5.3. Reply format: protocol.go.

sshtab_sessions=~/.sshtab/sessions
sshtab_dir=$sshtab_sessions/${SSHTAB_WINDOW-}
sshtab_tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$sshtab_tmp"' EXIT
: >"$sshtab_tmp/stdout"
: >"$sshtab_tmp/stderr"
: "${SSHTAB_KEEP_HEAD:=10240}" "${SSHTAB_KEEP_TAIL:=20480}"
sshtab_signal= sshtab_cut_stdout= sshtab_cut_stderr=

# The window's shell: restore its saved state, run the command, save the new state.
# The state is cwd, every shell variable (exported or not) and every function.
# All of our own names start with __sshtab_ so they are never saved as user state.
read -r -d '' sshtab_window_shell <<'EOF'
export -n __sshtab_dir
if [ -f "$__sshtab_dir/vars.sh" ]; then
  . "$__sshtab_dir/vars.sh" 2>/dev/null
  . "$__sshtab_dir/funcs.sh" 2>/dev/null
fi
# Keep commands non-interactive and their output unbuffered.
export PAGER=cat GIT_PAGER=cat TERM=dumb DEBIAN_FRONTEND=noninteractive PYTHONUNBUFFERED=1

__sshtab_oldpwd=${OLDPWD-}
if [ -f "$__sshtab_dir/cwd" ]; then
  __sshtab_cwd=$(<"$__sshtab_dir/cwd")
  if ! cd -- "$__sshtab_cwd" 2>/dev/null; then
    echo "sshtab: $__sshtab_cwd no longer exists, starting in $HOME" >&2
    cd ~
  fi
else
  cd ~
fi
# Our cd must not clobber the user's `cd -` target.
[ -n "$__sshtab_oldpwd" ] && OLDPWD=$__sshtab_oldpwd

# EXIT trap so the state is saved even when the command itself calls `exit`.
# The exit file is written last: once it exists, the state is complete.
trap '__sshtab_rc=$?
  printf "%s" "$PWD" >"$__sshtab_dir/cwd.new"
  while IFS= read -r __sshtab_n; do
    case $__sshtab_n in
      __sshtab_*|BASH*|COMP_*|PWD|SHLVL|_|PPID|UID|EUID|GROUPS|FUNCNAME|DIRSTACK|PIPESTATUS|\
      LINENO|RANDOM|SRANDOM|SECONDS|HISTCMD|EPOCHSECONDS|EPOCHREALTIME|SHELLOPTS|\
      OPTIND|OPTERR|COLUMNS|LINES|SSH_CLIENT|SSH_CONNECTION|SSH_TTY|SSH_AUTH_SOCK) continue ;;
    esac
    declare -p "$__sshtab_n" 2>/dev/null
  done < <(compgen -v) >"$__sshtab_dir/vars.new"
  declare -f >"$__sshtab_dir/funcs.new"
  mv -f "$__sshtab_dir/vars.new" "$__sshtab_dir/vars.sh"
  mv -f "$__sshtab_dir/funcs.new" "$__sshtab_dir/funcs.sh"
  mv -f "$__sshtab_dir/cwd.new" "$__sshtab_dir/cwd"
  printf "%s" "$__sshtab_rc" >"$__sshtab_dir/exit.new"
  mv -f "$__sshtab_dir/exit.new" "$__sshtab_dir/exit"
  exit $__sshtab_rc' EXIT
# Like a terminal: Ctrl-C ends the command with 130, SIGTERM with 143.
trap 'exit 130' INT
trap 'exit 143' TERM

eval "$(<"$__sshtab_dir/cmd")"
EOF

# Sets sshtab_starttime to when pid $1 started (clock ticks since boot, field 22
# of /proc/<pid>/stat), or to empty if there is no such process. Never forks.
sshtab_read_starttime() {
  local stat fields
  sshtab_starttime=
  read -r stat 2>/dev/null <"/proc/$1/stat" || return 1
  # Fields after the "(command name)", which may itself contain spaces.
  read -r -a fields <<<"${stat##*) }"
  sshtab_starttime=${fields[19]}
}

# Is pid $1 still this window's command, and not an unrelated process that
# reused the pid? A reused pid has a different start time.
sshtab_is_ours() {
  kill -0 "$1" 2>/dev/null || return 1
  [ -d /proc/self ] || return 0
  sshtab_read_starttime "$1" && [ "$sshtab_starttime" = "$sshtab_pidstart" ]
}

# Sets sshtab_state (exit, running, stopped or idle; DESIGN.md 5.5) and, from the
# window's files, sshtab_pid, sshtab_code, sshtab_cwd, sshtab_elapsed and sshtab_cmdline.
sshtab_inspect() {
  local now
  sshtab_pid= sshtab_pidstart= sshtab_code=0 sshtab_cwd=$HOME sshtab_elapsed=0 sshtab_cmdline=
  [ -f "$sshtab_dir/pid" ] && read -r sshtab_pid sshtab_pidstart <"$sshtab_dir/pid"
  if [ -f "$sshtab_dir/exit" ]; then
    sshtab_state=exit
    sshtab_code=$(<"$sshtab_dir/exit")
  elif [ -n "$sshtab_pid" ] && sshtab_is_ours "$sshtab_pid"; then
    sshtab_state=running
  elif [ -n "$sshtab_pid" ]; then
    sshtab_state=stopped
  else
    sshtab_state=idle
  fi
  [ -f "$sshtab_dir/cwd" ] && sshtab_cwd=$(<"$sshtab_dir/cwd")
  if [ -f "$sshtab_dir/cmd" ]; then
    sshtab_cmdline=$(<"$sshtab_dir/cmd")
    sshtab_cmdline=${sshtab_cmdline%%$'\n'*}
    sshtab_cmdline=${sshtab_cmdline:0:200}
  fi
  if [ -f "$sshtab_dir/started" ]; then
    printf -v now '%(%s)T' -1
    sshtab_elapsed=$((now - $(<"$sshtab_dir/started")))
  fi
}

# Appends bytes [$2, $2 + $3) of file $1 to file $4.
sshtab_slice() { tail -c +"$(($2 + 1))" "$1" | head -c "$3" >>"$4"; }

# Writes bytes [$2, $3) of file $1 to $sshtab_tmp/$4 (stdout or stderr). If there
# are too many, keeps only the first SSHTAB_KEEP_HEAD and last SSHTAB_KEEP_TAIL bytes
# and sets sshtab_cut_$4 to "<head bytes>:<omitted bytes>:<omitted newlines>".
# Cutting here, on the server, means huge output never crosses the network.
sshtab_emit() {
  local src=$1 start=$2 end=$3 name=$4 omitted lines
  local dest=$sshtab_tmp/$name
  : >"$dest"
  printf -v "sshtab_cut_$name" '%s' ''
  if [ $((end - start)) -le $((SSHTAB_KEEP_HEAD + SSHTAB_KEEP_TAIL)) ]; then
    sshtab_slice "$src" "$start" $((end - start)) "$dest"
    return
  fi
  omitted=$((end - start - SSHTAB_KEEP_HEAD - SSHTAB_KEEP_TAIL))
  sshtab_slice "$src" "$start" "$SSHTAB_KEEP_HEAD" "$dest"
  sshtab_slice "$src" $((end - SSHTAB_KEEP_TAIL)) "$SSHTAB_KEEP_TAIL" "$dest"
  lines=$(tail -c +"$((start + SSHTAB_KEEP_HEAD + 1))" "$src" | head -c "$omitted" | wc -l)
  printf -v "sshtab_cut_$name" '%s:%s:%s' "$SSHTAB_KEEP_HEAD" "$omitted" "$lines"
}

# Collects into $sshtab_tmp the stdout and stderr the agent has not seen yet (or
# their last SSHTAB_TAIL lines), and remembers how far the agent has now read.
sshtab_collect() {
  local name src off size out_off=0 err_off=0
  [ -f "$sshtab_dir/offset" ] && read -r out_off err_off <"$sshtab_dir/offset"
  for name in stdout stderr; do
    src=$sshtab_dir/$name
    : >"$sshtab_tmp/$name"
    [ -f "$src" ] || continue
    # Measure first: the command may still be writing.
    size=$(wc -c <"$src")
    if [ -n "${SSHTAB_TAIL-}" ]; then
      head -c "$size" "$src" | tail -n "$SSHTAB_TAIL" >"$sshtab_tmp/$name.tail"
      sshtab_emit "$sshtab_tmp/$name.tail" 0 "$(wc -c <"$sshtab_tmp/$name.tail")" "$name"
    else
      if [ "$name" = stdout ]; then off=$out_off; else off=$err_off; fi
      [ "$off" -le "$size" ] || off=0
      sshtab_emit "$src" "$off" "$size" "$name"
    fi
    if [ "$name" = stdout ]; then out_off=$size; else err_off=$size; fi
  done
  [ -d "$sshtab_dir" ] && printf '%s %s\n' "$out_off" "$err_off" >"$sshtab_dir/offset"
}

sshtab_b64() { printf '%s' "$1" | base64 -w0; }

# Prints the reply: a header with status $1 and the values from sshtab_inspect,
# then whatever stdout and stderr were collected into $sshtab_tmp.
sshtab_reply() {
  printf 'SSHTAB1 status=%s code=%d cwd=%s elapsed=%d cmd=%s signal=%s out_cut=%s err_cut=%s out=%d err=%d\n' \
    "$1" "$sshtab_code" "$(sshtab_b64 "$sshtab_cwd")" "$sshtab_elapsed" "$(sshtab_b64 "$sshtab_cmdline")" \
    "$sshtab_signal" "$sshtab_cut_stdout" "$sshtab_cut_stderr" \
    "$(wc -c <"$sshtab_tmp/stdout")" "$(wc -c <"$sshtab_tmp/stderr")"
  cat "$sshtab_tmp/stdout" "$sshtab_tmp/stderr"
}

# Sleeps 20ms without forking: `read -t` on a fifo that nobody writes to.
sshtab_nap() {
  if [ ! -p "$sshtab_tmp/tick" ]; then
    mkfifo "$sshtab_tmp/tick" && exec 8<>"$sshtab_tmp/tick"
  fi
  read -r -t 0.02 -u 8
}

# Re-checks "$2" every 20ms while it succeeds, for at most $1 seconds.
sshtab_while() {
  local ticks=$(($1 * 50))
  while [ "$ticks" -gt 0 ] && "$2"; do
    sshtab_nap
    ticks=$((ticks - 1))
  done
}

sshtab_unfinished() { [ ! -f "$sshtab_dir/exit" ] && kill -0 "$sshtab_pid" 2>/dev/null; }
sshtab_group_alive() { kill -0 -- "-$sshtab_pid" 2>/dev/null; }

# Ctrl-C, then SIGTERM, then SIGKILL to the command's whole process group, each
# time giving it 5 seconds to exit. Sets sshtab_signal to the last one sent.
sshtab_stop() {
  local sig
  for sig in INT TERM KILL; do
    sshtab_group_alive || return
    kill -s "$sig" -- "-$sshtab_pid" 2>/dev/null
    sshtab_signal=$sig
    sshtab_while 5 sshtab_group_alive
  done
}

# Takes the window's lock; one start, close or similar at a time per window.
# flock is released even if we get killed.
sshtab_lock() {
  exec 9>"$sshtab_dir/lock"
  flock -w 5 9
}

sshtab_run() {
  mkdir -p "$sshtab_dir" || exit 1
  sshtab_lock || { sshtab_inspect; sshtab_reply busy; return; }
  sshtab_inspect
  if [ "$sshtab_state" = running ]; then
    sshtab_reply busy
    return
  fi

  rm -f "$sshtab_dir/exit" "$sshtab_dir/pid"
  printf '%s' "$SSHTAB_CMD_B64" | base64 -d >"$sshtab_dir/cmd" || exit 1
  printf '%(%s)T' -1 >"$sshtab_dir/started"
  echo "0 0" >"$sshtab_dir/offset"
  # Once a window has saved state, start from an empty environment so that
  # variables the user unset stay unset; a new window inherits the login env.
  [ -f "$sshtab_dir/vars.sh" ] && sshtab_clean=1
  # Job control (set -m) gives the command a process group of its own, so it
  # can be stopped as a whole. Without it, bash would start the background
  # command with SIGINT ignored, and Ctrl-C (`sshtab kill`) could not reach it.
  # Output goes to files and stdin is /dev/null, so the command holds nothing
  # of the ssh connection and keeps running after ssh disconnects.
  set -m
  env ${sshtab_clean:+-i} __sshtab_dir="$sshtab_dir" "$BASH" -c "$sshtab_window_shell" \
    </dev/null >"$sshtab_dir/stdout" 2>"$sshtab_dir/stderr" 9>&- &
  sshtab_pid=$!
  set +m
  sshtab_read_starttime "$sshtab_pid"
  sshtab_pidstart=$sshtab_starttime
  echo "$sshtab_pid $sshtab_pidstart" >"$sshtab_dir/pid"
  exec 9>&-

  sshtab_while "$SSHTAB_WAIT" sshtab_unfinished
  sshtab_inspect
  sshtab_collect
  sshtab_reply "$sshtab_state"
}

sshtab_log() {
  sshtab_inspect
  sshtab_collect
  sshtab_reply "$sshtab_state"
}

sshtab_kill() {
  sshtab_inspect
  [ "$sshtab_state" = running ] && sshtab_stop
  sshtab_inspect
  sshtab_collect
  sshtab_reply "$sshtab_state"
}

# One line per window: name, state, exit code, elapsed seconds, cwd (base64)
# and command (base64), separated by tabs.
sshtab_list() {
  local d
  for d in "$sshtab_sessions"/*/; do
    [ -d "$d" ] || continue
    sshtab_dir=${d%/}
    sshtab_inspect
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${sshtab_dir##*/}" "$sshtab_state" "$sshtab_code" \
      "$sshtab_elapsed" "$(sshtab_b64 "$sshtab_cwd")" "$(sshtab_b64 "$sshtab_cmdline")"
  done >"$sshtab_tmp/stdout"
  sshtab_reply list
}

sshtab_close() {
  if [ ! -d "$sshtab_dir" ]; then
    sshtab_inspect
    sshtab_reply missing
    return
  fi
  sshtab_lock || { sshtab_inspect; sshtab_reply busy; return; }
  sshtab_inspect
  if [ "$sshtab_state" = running ]; then
    if [ -z "${SSHTAB_FORCE-}" ]; then
      sshtab_reply busy
      return
    fi
    sshtab_stop
    sshtab_inspect
    if [ "$sshtab_state" = running ]; then
      sshtab_reply busy
      return
    fi
  fi
  rm -rf "$sshtab_dir"
  sshtab_reply closed
}

case $SSHTAB_ACTION in
  run) sshtab_run ;;
  log) sshtab_log ;;
  kill) sshtab_kill ;;
  list) sshtab_list ;;
  close) sshtab_close ;;
  *) echo "sshtab driver: unknown action $SSHTAB_ACTION" >&2; exit 1 ;;
esac
