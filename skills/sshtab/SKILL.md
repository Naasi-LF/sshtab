---
name: sshtab
description: Work on a remote Linux server over SSH with sshtab, a remote shell built for agents. Windows keep their directory, environment variables and activated venv between commands, long commands keep running in their window, and output is kept small. Use it whenever commands, files or jobs (builds, servers, GPU training) live on a remote host, instead of plain `ssh host cmd`.
---

# sshtab: a remote shell for agents

Every command runs on the server in a **window**: a terminal tab that stays open between your calls.
Write it as `host/window` (`gpu1/train`). `host` alone means `host/main`. `host` is a name from `~/.ssh/config`.

```bash
sshtab run  gpu1/train 'cd ~/proj && source .venv/bin/activate'   # remembered by gpu1/train
sshtab run  gpu1/train 'python train.py'                          # still in ~/proj, venv active
sshtab log  gpu1/train                                            # new output since you last looked
sshtab kill gpu1/train                                            # Ctrl-C
sshtab sessions gpu1                                              # list windows
sshtab close gpu1/train                                           # delete the window
```

## Read the status header

The first line of every reply tells you what happened, where, and in which directory:

```text
[exit 1 · gpu1/train · /home/me/proj]
...stdout...
--- stderr ---
...stderr...
hint: ...
```

| Status | Meaning | exit code of `sshtab run` / `sshtab log` |
|---|---|---|
| `exit N` | finished with exit code N | N |
| `running 2m05s` | still running in its window | 75 |
| `stopped` | died without an exit code (SIGKILL) | 137 |
| `idle` | the window never ran anything | 0 |

`sshtab kill`, `sshtab sessions` and `sshtab close` exit 0 when they succeed; the header still shows the command's own result (e.g. `exit 130` after Ctrl-C).
sshtab's own errors (cannot connect, window busy, bad arguments) exit 255 with `error:` on stderr.

stdout comes first, then stderr after `--- stderr ---`; they are not interleaved. If their relative order matters, run the command with `2>&1`.
`hint:` lines tell you the next command to use; follow them.

## Windows remember state

- The directory, every shell variable (exported or not), `unset`s and shell functions persist, so `source .venv/bin/activate`, `export CUDA_VISIBLE_DEVICES=1` and `cd` work as in a terminal. Do them once per window, not before every command.
- Windows are independent. Use one per job: e.g. `gpu1/train` for the training run, `gpu1/main` for looking around, `gpu1/tb` for TensorBoard.
- A new window starts in the home directory with a fresh login environment.
- Not remembered: aliases, `set -e`-style shell options, and anything from a command that had to be SIGKILLed.

## Long commands

`sshtab run` waits at most 30 seconds. If the command is not done by then, it **keeps running in its window** and you get the output so far, `[running …]` and exit code 75. It also survives ssh disconnecting.

- Check on it with `sshtab log host/window`: it shows only output you have not seen yet (`(no new output)` if none), plus the status. Repeat until the exit code is no longer 75.
- To block longer or shorter in one call, use `--wait` with seconds, minutes or hours (`90`, `90s`, `10m`, `2h`): `sshtab run gpu1/main 'pip install -r requirements.txt' --wait 10m`. `--wait 0` starts the command and returns at once; a short wait like `--wait 5s` is a good way to start a long job and see its first lines.
- `sshtab log host/window --tail 50` shows the last 50 lines whether seen or not.
- A busy window refuses new commands (`error: … is busy`). Use another window meanwhile; do not wait for it to free up just to run `ls`.
- `sshtab sessions host` shows every window's status (`running 12m05s` for busy ones), directory and last command: check it before starting work on a host someone may already be using.
- Do not wrap commands in `nohup …&`, `tmux` or `screen`: a window already is a background terminal. Do not poll with `sleep` loops on the server; poll with `sshtab log`.

## Stopping

`sshtab kill host/window` sends Ctrl-C to the whole process group, then SIGTERM after 5 seconds, then SIGKILL after 5 more. It shows the final output and which signal worked. Python gets a `KeyboardInterrupt`, so training scripts can save a checkpoint. Afterwards the window is free again with its state intact.

## Big output

When stdout or stderr is over 30KB, you get its first ~10KB and last ~20KB with a marker like `... [75.2 MB omitted, 9,995,172 lines] ...` in between. The full output stays in the file the hint names (`~/.sshtab/sessions/<window>/stdout`) until that window runs its next command. Read parts of it from **another** window:

```bash
sshtab run gpu1/main 'sed -n 5000,5100p ~/.sshtab/sessions/train/stdout'
```

Better still, avoid big output: `| tail -n 50`, `| grep -m 20 ERROR`, `head`, `wc -l`.

Progress bars that redraw with `\r` (tqdm, curl) show only their latest state.

## Commands must not need a terminal

stdin is `/dev/null` and there is no TTY, so anything interactive gets EOF at once instead of hanging:

- No `vim`, `nano`, `less`, `top`, `htop`, `python` REPL. Use `cat -n`, `sed -n 1,80p`, `top -bn1`, `nvidia-smi`, `python -c` or a script.
- `PAGER=cat`, `GIT_PAGER=cat` and `DEBIAN_FRONTEND=noninteractive` are set; still pass `-y` to installers.
- `sudo` works only without a password; use `sudo -n` so it fails fast instead of asking.
- Messages follow the server's locale and may not be in English. If you need to match on tool output, prefix the command with `LC_ALL=C.UTF-8`.

## Files live on the server

Paths in `sshtab run` are server paths. Your local files are not there, and the server's files are not local: do not use your local Read/Edit tools on server paths.

- Read: `sshtab run gpu1 'cat -n ~/proj/train.py'` or `sed -n 100,160p` for a range.
- Search: `sshtab run gpu1 'grep -rn "lr" ~/proj/configs'`.
- Write a whole file with a quoted heredoc, sent on stdin (`-`) so nothing needs escaping:

```bash
sshtab run gpu1/main - <<'CMD'
cat > ~/proj/config.json <<'EOF'
{"lr": 0.001, "epochs": 20}
EOF
CMD
```

- Small edits: `sed -i 's/^lr = .*/lr = 3e-4/' train.py`, or a short `python3 - <<'EOF' … EOF` that replaces an exact string. Check the result with `grep -n` or `sed -n` afterwards.
- Copy files with `scp` or `rsync` from your own shell, not through sshtab.

## Quoting

Put the command in single quotes: `sshtab run gpu1 'echo "$HOME"'` expands `$HOME` on the server. For anything with single quotes or several lines, use `-` and a quoted heredoc as above.
