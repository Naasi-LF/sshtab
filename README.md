# sshtab

**A remote shell for AI agents.** Your agent (Claude Code, Codex, …) stays on your machine and works on a Linux server over SSH, in terminal tabs that stay open between commands. Nothing to install on the server, no model running there.

[中文说明](README.zh-CN.md)

```text
$ sshtab run pdvm/train 'cd ~/sshtab-demo && source .venv/bin/activate'
[exit 0 · pdvm/train · /home/parallels/sshtab-demo]

$ sshtab run pdvm/train 'python train.py' --wait 3s
[running 3s · pdvm/train · /home/parallels/sshtab-demo]
training: lr=1.5 epochs=40
epoch 1/40 loss=100
epoch 2/40 loss=400
epoch 3/40 loss=1600
--- stderr ---
epoch 4 [####      ]
hint: still running. `sshtab log pdvm/train` to check, `sshtab kill pdvm/train` to stop

$ sshtab kill pdvm/train
[exit 130 · pdvm/train · /home/parallels/sshtab-demo]
--- stderr ---
epoch 4 [#####     ]Traceback (most recent call last):
  File "/home/parallels/sshtab-demo/train.py", line 17, in <module>
    time.sleep(0.1)
KeyboardInterrupt
stopped with SIGINT
```

The loss was blowing up, so the agent hit Ctrl-C. The window still remembers the directory and the venv, so the next `sshtab run pdvm/train 'python train.py'` just works.

## Why not just `ssh host cmd`?

| With plain ssh | With sshtab |
|---|---|
| Every command starts a fresh shell: `cd` and `source .venv/bin/activate` are gone by the next one | Each window remembers its directory, variables, functions and venv |
| A long command blocks the agent, or needs `nohup`/`tmux` juggling | After 30s you get the output so far; the command keeps running in its window |
| Pagers, prompts and `read` hang forever | stdin is `/dev/null`, pagers are off: nothing can wait for input |
| One `cat` of a log can flood the agent's context | Output over 30KB is cut **on the server** to its head and tail |
| Nested quotes are easy to get wrong | Commands are sent encoded: quote once, as in your own terminal |
| A new connection per command | One reused connection, no handshake per command: about 0.05s on a nearby server, otherwise mostly the network's latency |
| `scp`/`rsync` paths start from the home directory, not where you `cd`'d | `push`/`pull` paths start from the window's directory, over the same connection |

## Install

sshtab is a single binary for macOS and Linux (Intel or ARM). On your machine it only needs OpenSSH; on the server, bash and key-based SSH access.

```bash
curl -fsSL https://raw.githubusercontent.com/Naasi-LF/sshtab/main/install.sh | sh
```

This puts `sshtab` in `~/.local/bin` (set `SSHTAB_INSTALL_DIR` to change that) and tells you if that directory is not on your PATH yet. Run it again to upgrade; delete the file to uninstall. You can also download an archive from [Releases](https://github.com/Naasi-LF/sshtab/releases) yourself, or build from source with `go install github.com/Naasi-LF/sshtab@latest`.

Hosts are the names in your `~/.ssh/config`, so jump hosts, keys and ports work as usual. sshtab never asks for passwords.

### Teach your agent

The installer also installs the sshtab skill for the agents it finds: to `~/.claude/skills/sshtab/SKILL.md` if you use Claude Code (`~/.claude` exists), and to `~/.agents/skills/sshtab/SKILL.md` if you use Codex (`~/.codex` exists). Skip it with `SSHTAB_NO_SKILL=1`. The skill explains windows, long commands, output limits and how to edit files on the server. Other agents can be given the same file, [skills/sshtab/SKILL.md](skills/sshtab/SKILL.md), as instructions.

## Commands

```text
sshtab run      host[/window] 'command' [--wait 30s]   run a command in a window
sshtab log      host[/window] [--tail N]               output you have not seen yet
sshtab kill     host[/window]                          Ctrl-C, then SIGTERM, then SIGKILL
sshtab sessions host                                   list windows
sshtab close    host[/window] [--force]                delete a window
sshtab push     host[/window] path... [--to DIR]       copy local files into the window's directory
sshtab pull     host[/window] path... [--to DIR]       copy files from the window's directory to here
```

`host` alone means the window `host/main`. A window is created the first time you use its name.

### Windows

A window is a terminal tab on the server. Each one keeps its own working directory, shell variables (exported or not), functions and `unset`s, so activating a venv or exporting `CUDA_VISIBLE_DEVICES` works once and stays. Use one window per job, like tabs: `gpu1/train` for training, `gpu1/main` for looking around.

A new window starts in your home directory with the environment of a login shell: it reads `/etc/profile` and `~/.profile` (or `~/.bash_profile`), which plain `ssh host cmd` skips. So if `python3` or `nvcc` works after `ssh host`, it works in a new window too, e.g. on GPU hosts that set up conda and CUDA there.

A window runs one command at a time. A busy window refuses new commands and suggests another window.

### The status header

Every reply starts with one line that says what happened, where, and in which directory:

| Header | Meaning | Exit code of `run` / `log` |
|---|---|---|
| `[exit 2 · gpu1/main · /data]` | finished with exit code 2 | 2 |
| `[running 2m05s · gpu1/train · /data/proj]` | still running in its window | 75 |
| `[stopped · …]` | died without an exit code (SIGKILL) | 137 |

sshtab's own errors (connection, busy window, bad arguments) exit 255, like ssh.

### Long commands

`sshtab run` waits up to `--wait` (default 30s, also `90`, `10m`, `2h`, `0`). If the command is still going, you get its output so far and it keeps running, even if ssh disconnects. `sshtab log` then shows only new output, so an agent can poll until the exit code is no longer 75.

`sshtab kill` sends Ctrl-C to the whole process group, so Python gets a `KeyboardInterrupt` and a training script can save a checkpoint. If that does not work, it escalates to SIGTERM and then SIGKILL, 5 seconds apart.

### Moving files

```text
$ sshtab push gpu1/iris iris.csv train.py
[pushed 2 files, 6.6 KB · gpu1/iris · /home/me/iris]
  iris.csv
  train.py

$ sshtab run gpu1/iris 'python3 train.py'
$ sshtab pull gpu1/iris results
[pulled 2 files, 785 bytes · gpu1/iris · /home/me/iris]
  results/metrics.json
  results/model.json
```

Server paths are relative to the window's directory (`~/…` and absolute paths work too); `--to` picks another destination, created if needed. Files and directories keep their names, like `cp -r`, with permissions, modification times and symlinks. Transfers work while the window is busy, so you can pull a checkpoint mid-training. They are tar streams over the same SSH connection, so the server needs nothing but `tar`. A pull never writes outside its destination, whatever the server sends. Each transfer is a full copy: for big trees you update often, `rsync` is still the better tool.

### Big output

stdout and stderr are each cut to their first ~10KB and last ~20KB when longer than 30KB, at line breaks, with a marker in between:

```text
$ sshtab run gpu1 'seq 1 10000000'
...
2269
... [75.2 MB omitted, 9,995,172 lines] ...
9997442
...
```

This happens on the server, so that 75MB reply takes half a second and sends 31KB. The full output stays in a file on the server until the window's next command. Progress bars that redraw with `\r` (tqdm) show only their latest state.

## Limits

- Linux servers only, with bash, coreutils, `tar` and `flock` (util-linux). Tested on Ubuntu 24.04 with bash 5.2.
- No interactive programs: no `vim`, `top` or password prompts. Use `cat -n`, `top -bn1`, `sudo -n`.
- Aliases and shell options like `set -e` are not remembered between commands.
- `conda activate` has not been tested yet.

## How it works

Each call runs one `ssh host bash -s` over a shared ControlMaster connection and sends a small bash driver on stdin. The driver keeps each window's state in `~/.sshtab/sessions/<window>/` on the server, starts commands in their own process group with output going to files, and replies with a length-delimited header plus the output. The full design is in [DESIGN.md](DESIGN.md) (in Chinese).

## Development

sshtab is written in Go with no dependencies beyond the standard library. The server-side logic is a bash script, [driver.sh](driver.sh), embedded in the binary.

```bash
go build -o bin/sshtab .   # build
go test -short ./...       # unit tests, no server needed
go test ./...              # plus end-to-end tests against a real server
```

The end-to-end tests build the binary and run it against the SSH host `pdvm`, or the one named in `SSHTAB_TEST_HOST`; they are skipped if it is unreachable. They create and delete their own windows.

To release, push a version tag (`git tag v0.1.0 && git push origin v0.1.0`). GitHub Actions then builds the four binaries with GoReleaser and publishes them on the Releases page, where `install.sh` finds them.

## License

[MIT](LICENSE)
