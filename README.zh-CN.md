# sshtab

**给 AI Agent 用的远程 shell。** Agent（Claude Code、Codex 等）留在你自己的电脑上，通过 SSH 操作远程 Linux 服务器。在服务器上开的"终端标签页"会一直保留，不会每条命令执行完就关掉。服务器上不用安装任何东西，也不用跑任何模型。

[English](README.md)

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

loss 在发散，所以 Agent 按了 Ctrl-C。窗口仍然记得当前目录和已激活的 venv，所以下一条 `sshtab run pdvm/train 'python train.py'` 直接就能跑。

## 为什么不直接用 `ssh host cmd`？

| 用裸 ssh | 用 sshtab |
|---|---|
| 每条命令都是新的 shell，`cd` 和 `source .venv/bin/activate` 到下一条就失效了 | 每个窗口记住自己的目录、变量、函数和 venv |
| 长命令会一直卡住 Agent，或者得自己折腾 `nohup`、`tmux` | 30 秒后先返回已有的输出，命令在窗口里继续跑 |
| 翻页器、确认提示、`read` 会一直卡死 | stdin 接 `/dev/null`，翻页器全部关掉，没有东西能等待输入 |
| 一次 `cat` 日志就可能撑爆 Agent 的上下文 | 超过 30KB 的输出在**服务器端**截断，只保留开头和结尾 |
| 引号嵌套很容易写错 | 命令经过编码后传输，像在自己终端里一样只需要加一层引号 |
| 每条命令都重新建立连接 | 复用同一个连接，每条命令约 0.1 秒 |
| `scp`、`rsync` 的路径从家目录算起，和你 `cd` 到的地方对不上 | `push`、`pull` 的路径从窗口的当前目录算起，走的也是同一个连接 |

## 安装

sshtab 是一个独立的二进制文件，支持 macOS 和 Linux（Intel 和 ARM 都可以）。本机只需要 OpenSSH；服务器上只需要 bash，并且能用密钥登录。

```bash
curl -fsSL https://raw.githubusercontent.com/Naasi-LF/sshtab/main/install.sh | sh
```

这条命令会把 `sshtab` 装到 `~/.local/bin`（想换位置可以设置 `SSHTAB_INSTALL_DIR`）；如果这个目录还不在 PATH 里，它会告诉你怎么加。升级就再运行一次，卸载就删掉这个文件。也可以自己去 [Releases](https://github.com/Naasi-LF/sshtab/releases) 下载压缩包，或者用 `go install github.com/Naasi-LF/sshtab@latest` 从源码编译。

主机名就是你 `~/.ssh/config` 里的名字，跳板机、密钥、端口都照常生效。sshtab 永远不会要求输入密码。

### 教会你的 Agent

如果你用 Claude Code（也就是存在 `~/.claude` 目录），安装脚本会顺便把 sshtab 的 skill 装到 `~/.claude/skills/sshtab/SKILL.md`（不想装可以设置 `SSHTAB_NO_SKILL=1`）。它会告诉 Agent 窗口怎么用、长命令怎么处理、输出上限是多少，以及怎么修改服务器上的文件。其他 Agent 也可以直接把这份 [skills/sshtab/SKILL.md](skills/sshtab/SKILL.md) 当作使用说明。

## 命令

```text
sshtab run      host[/window] 'command' [--wait 30s]   在窗口里执行命令
sshtab log      host[/window] [--tail N]               查看还没看过的输出
sshtab kill     host[/window]                          Ctrl-C，然后 SIGTERM，再然后 SIGKILL
sshtab sessions host                                   列出所有窗口
sshtab close    host[/window] [--force]                删除窗口
sshtab push     host[/window] path... [--to DIR]       把本地文件传到窗口的当前目录
sshtab pull     host[/window] path... [--to DIR]       把窗口当前目录里的文件拿回本地
```

只写 `host` 时，指的是窗口 `host/main`。第一次用到某个窗口名时，会自动新建这个窗口。

### 窗口

窗口就是服务器上的一个终端标签页。每个窗口都有自己的当前目录、shell 变量（不管有没有 export）、函数和 `unset` 记录，所以激活 venv、设置 `CUDA_VISIBLE_DEVICES` 这类操作做一次就一直有效。像用终端标签页一样，一个任务开一个窗口：比如 `gpu1/train` 用来训练，`gpu1/main` 用来随便看看。

一个窗口同一时间只能跑一条命令。窗口正忙时会拒绝新命令，并提示你换一个窗口。

### 状态头

每次输出的第一行都会告诉你：发生了什么、在哪个窗口、在哪个目录。

| 状态头 | 含义 | `run` / `log` 的退出码 |
|---|---|---|
| `[exit 2 · gpu1/main · /data]` | 已结束，退出码为 2 | 2 |
| `[running 2m05s · gpu1/train · /data/proj]` | 还在窗口里跑 | 75 |
| `[stopped · …]` | 没有留下退出码就消失了（SIGKILL） | 137 |

sshtab 自己的错误（连接失败、窗口正忙、参数错误）返回 255，和 ssh 一致。

### 长命令

`sshtab run` 最多等 `--wait` 指定的时间（默认 30 秒，也可以写 `90`、`10m`、`2h`、`0`）。命令还没跑完时，你会先拿到目前为止的输出，命令继续在窗口里跑，ssh 断开了也不影响。之后用 `sshtab log` 只看新增的输出，Agent 可以一直轮询，直到退出码不再是 75。

`sshtab kill` 会对整个进程组发送 Ctrl-C，所以 python 会收到 `KeyboardInterrupt`，训练脚本可以借机保存 checkpoint。如果这样停不下来，会依次升级为 SIGTERM 和 SIGKILL，每次间隔 5 秒。

### 传文件

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

服务器上的路径从窗口的当前目录算起（也可以写 `~/…` 或绝对路径）；`--to` 可以指定别的目标目录，不存在会自动创建。文件和目录保留原来的名字，和 `cp -r` 一样，权限、修改时间和符号链接都会保留。窗口正忙时也能传，所以训练跑到一半时可以随时拉一份 checkpoint 回来。传输用的是 tar，走同一个 SSH 连接，服务器上只需要有 `tar`。不管服务器发回来什么，pull 都不会写到目标目录之外。每次传输都是完整复制：需要反复同步的大量文件，还是 `rsync` 更合适。

### 大量输出

stdout 和 stderr 各自超过 30KB 时，只保留开头约 10KB 和结尾约 20KB，截断点在换行处，中间放一行省略标记：

```text
$ sshtab run gpu1 'seq 1 10000000'
...
2269
... [75.2 MB omitted, 9,995,172 lines] ...
9997442
...
```

截断是在服务器上完成的，所以这 75MB 的输出只用半秒就返回了，实际只传回 31KB。完整输出会保存在服务器上的一个文件里，直到这个窗口运行下一条命令。用 `\r` 刷新的进度条（比如 tqdm）只显示最新的状态。

## 限制

- 只支持 Linux 服务器，需要 bash、coreutils、`tar` 和 `flock`（util-linux）。已在 Ubuntu 24.04 + bash 5.2 上测试过。
- 不支持交互式程序：不能用 `vim`、`top`，也不能输入密码。可以改用 `cat -n`、`top -bn1`、`sudo -n`。
- 不会记住 alias，也不会记住 `set -e` 这类 shell 选项。
- `conda activate` 还没有测试过。

## 工作原理

每次调用都通过共享的 ControlMaster 连接执行一次 `ssh host bash -s`，把一个小的 bash 驱动脚本从 stdin 发过去。驱动脚本把每个窗口的状态保存在服务器的 `~/.sshtab/sessions/<window>/` 里，在独立的进程组里启动命令，输出写入文件，最后返回一个按长度切分的状态头加输出内容。完整设计见 [DESIGN.md](DESIGN.md)。

## 开发

sshtab 用 Go 编写，只用了标准库，没有第三方依赖。服务器端的逻辑是一个 bash 脚本 [driver.sh](driver.sh)，打包在二进制文件里。

```bash
go build -o bin/sshtab .   # 编译
go test -short ./...       # 单元测试，不需要服务器
go test ./...              # 再加上连接真实服务器的端到端测试
```

端到端测试会先编译出二进制文件，再连接名为 `pdvm` 的 SSH 主机，也可以用 `SSHTAB_TEST_HOST` 指定其他主机；连不上时这些测试会自动跳过。测试会自己创建并删除所用的窗口。

发布新版本时，推送一个版本标签（`git tag v0.1.0 && git push origin v0.1.0`）。GitHub Actions 会用 GoReleaser 编译出四个平台的二进制文件，发布到 Releases 页面，`install.sh` 就是从那里下载的。

## 许可证

[MIT](LICENSE)
