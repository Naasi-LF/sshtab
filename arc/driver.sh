# arc remote driver: fed to `bash -s` over ssh on every call, never installed.
# arc prepends ARC_CMD_B64=<the user's command, base64> and ARC_WINDOW=<name>.
# The reply format is documented in arc/protocol.py.

arc_dir=~/.arc/sessions/$ARC_WINDOW
mkdir -p "$arc_dir" || exit 1
arc_tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$arc_tmp"' EXIT
# A file, not an env var: env vars are capped at 128KB each.
printf '%s' "$ARC_CMD_B64" | base64 -d >"$arc_tmp/cmd" || exit 1

# The window's shell: restore its saved state, run the command, save the new state.
# The state is cwd, every shell variable (exported or not) and every function.
# All of our own names start with __arc_ so they are never saved as user state.
read -r -d '' arc_window_shell <<'EOF'
export -n __arc_dir __arc_cmdfile
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
  exit $__arc_rc' EXIT

eval "$(<"$__arc_cmdfile")"
EOF

# Once a window has saved state, start from an empty environment so that
# variables the user unset stay unset; a new window inherits the login env.
[ -f "$arc_dir/vars.sh" ] && arc_clean=1
# stdin is /dev/null so nothing can sit waiting for input.
env ${arc_clean:+-i} __arc_dir="$arc_dir" __arc_cmdfile="$arc_tmp/cmd" \
  "$BASH" -c "$arc_window_shell" </dev/null >"$arc_tmp/out" 2>"$arc_tmp/err"
arc_code=$?

arc_cwd=$HOME
[ -f "$arc_dir/cwd" ] && arc_cwd=$(<"$arc_dir/cwd")
printf 'ARC1 status=exit code=%d cwd=%s out=%d err=%d\n' \
  "$arc_code" "$(printf '%s' "$arc_cwd" | base64 -w0)" \
  "$(wc -c <"$arc_tmp/out")" "$(wc -c <"$arc_tmp/err")"
cat "$arc_tmp/out" "$arc_tmp/err"
