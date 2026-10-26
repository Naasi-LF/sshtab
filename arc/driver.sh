# arc remote driver: fed to `bash -s` over ssh on every call, never installed.
# arc prepends ARC_CMD_B64=<the user's command, base64>.
# The reply format is documented in arc/protocol.py.

# Keep commands non-interactive and their output unbuffered.
export PAGER=cat GIT_PAGER=cat TERM=dumb DEBIAN_FRONTEND=noninteractive PYTHONUNBUFFERED=1

arc_cmd=$(printf '%s' "$ARC_CMD_B64" | base64 -d) || exit 1
arc_tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$arc_tmp"' EXIT

cd ~ || exit 1
# stdin is /dev/null so nothing can sit waiting for input.
bash -c "$arc_cmd" </dev/null >"$arc_tmp/out" 2>"$arc_tmp/err"
arc_code=$?

printf 'ARC1 status=exit code=%d cwd=%s out=%d err=%d\n' \
  "$arc_code" "$(printf '%s' "$PWD" | base64 -w0)" \
  "$(wc -c <"$arc_tmp/out")" "$(wc -c <"$arc_tmp/err")"
cat "$arc_tmp/out" "$arc_tmp/err"
