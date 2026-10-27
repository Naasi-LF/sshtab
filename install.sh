#!/bin/sh
# Installs the latest sshtab release:
#   curl -fsSL https://raw.githubusercontent.com/Naasi-LF/sshtab/main/install.sh | sh
#
# SSHTAB_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
# SSHTAB_NO_SKILL=1   do not install the skill for Claude Code / Codex
# SSHTAB_ARCHIVE_URL  download this archive instead (mirrors, testing)
set -eu

repo=Naasi-LF/sshtab
bin_dir=${SSHTAB_INSTALL_DIR:-$HOME/.local/bin}

fail() {
  echo "sshtab install: $*" >&2
  exit 1
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in
  darwin | linux) ;;
  *) fail "unsupported OS: $os (sshtab runs on macOS and Linux)" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU: $(uname -m)" ;;
esac
command -v ssh >/dev/null || fail "ssh not found; sshtab uses your OpenSSH client"

url=${SSHTAB_ARCHIVE_URL:-https://github.com/$repo/releases/latest/download/sshtab_${os}_${arch}.tar.gz}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $url"
# Download first, then unpack: sh has no pipefail, so `curl | tar` would hide a failed download.
curl -fsSL "$url" -o "$tmp/sshtab.tar.gz" || fail "download failed: $url"
tar -xzf "$tmp/sshtab.tar.gz" -C "$tmp" || fail "not a valid archive: $url"
[ -f "$tmp/sshtab" ] || fail "the archive has no sshtab binary"

mkdir -p "$bin_dir"
cp "$tmp/sshtab" "$bin_dir/sshtab.new"
chmod 755 "$bin_dir/sshtab.new"
mv -f "$bin_dir/sshtab.new" "$bin_dir/sshtab"
echo "Installed $("$bin_dir/sshtab" --version) to $bin_dir/sshtab"

# install_skill <agent> <skills dir under ~>: copy SKILL.md to ~/<dir>/sshtab/.
install_skill() {
  mkdir -p "$HOME/$2/sshtab"
  cp "$tmp/skills/sshtab/SKILL.md" "$HOME/$2/sshtab/SKILL.md"
  echo "Installed the $1 skill to ~/$2/sshtab/SKILL.md"
}

if [ -z "${SSHTAB_NO_SKILL-}" ] && [ -f "$tmp/skills/sshtab/SKILL.md" ]; then
  if [ -d "$HOME/.claude" ]; then install_skill "Claude Code" .claude/skills; fi
  # Codex reads user skills from ~/.agents/skills, not ~/.codex.
  if [ -d "$HOME/.codex" ]; then install_skill Codex .agents/skills; fi
fi

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *)
    echo
    echo "$bin_dir is not in your PATH. Add this line to your shell profile (~/.zshrc or ~/.bashrc):"
    echo "  export PATH=\"$bin_dir:\$PATH\""
    ;;
esac
