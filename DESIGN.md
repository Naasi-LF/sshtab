# arc 设计文档（v0.1）

> `arc` 是暂定名，和 Phabricator 的 `arc` 重名，发布前需要改。

## 1. 目标

让运行在本地的 AI Agent（Claude Code 等）能方便、安全地操作远程 Linux 服务器：

- 服务器上**不用安装任何东西**，只需要能 SSH 登录，有 bash 和 coreutils
- 服务器上**不运行 AI 模型**
- Agent 只要会执行 bash 命令就能用，不依赖特定 Agent 或协议

### 为什么不直接用 `ssh host cmd`

| 裸 ssh 的问题 | arc 的做法 |
|---|---|
| 每条命令都是新 shell，`cd`、`source venv` 下一条就失效 | 窗口记住目录和环境变量 |
| 长命令要么一直卡住，要么得自己折腾 `nohup` | 30 秒没跑完就先返回，命令在窗口里继续跑 |
| 命令偷偷等待输入（翻页器、确认提示），一直卡死 | 关闭 stdin，强制非交互环境 |
| 一次输出几十 MB，撑爆 Agent 的上下文 | 输出自动截断 |
| 引号嵌套很容易写错 | 命令经过编码后传输，不需要转义 |
| 每次都重新建立连接，比较慢 | 复用 SSH 连接（每条命令约 20ms） |

### v0.1 不做的事

交互式程序（vim、top、需要输密码的 sudo）、Windows 服务器、专门的文件编辑命令、MCP 接入、多主机批量操作。

---

## 2. 核心概念：窗口

**窗口 = 服务器上的一个终端标签页。** 这是 arc 唯一的概念。

- 每个窗口有自己的**当前目录**和**环境变量**
- 窗口写成 `主机/窗口名`，例如 `pdvm/train`；只写 `pdvm` 等于 `pdvm/main`
- 第一次使用某个窗口名时自动新建；新窗口从家目录开始
- 窗口一直存在，直到 `close` 为止（服务器重启后也还在）
- 一个窗口同一时间只能跑一条命令；正在跑的窗口是"忙碌"的，就像真终端一样
- 长命令不需要"后台"的概念：没跑完的命令就留在它的窗口里继续跑

### 窗口记住什么

| 记住 | 不记住 |
|---|---|
| 当前目录（包括 `cd -` 要回到的目录） | alias（非交互 shell 里本来就不展开） |
| 所有 shell 变量，不管有没有 export，也包括数组 | `set -e` 这类 shell 选项 |
| `unset` 掉的变量（之后也保持 unset） | 用 `&` 放到后台的进程 |
| 所有 shell 函数（比如 venv 的 `deactivate`） | 被 SIGKILL 杀掉的命令所做的改动 |

所以 `source .venv/bin/activate` 之后，venv 会一直有效，`deactivate` 也能正常用，和真终端一样。

主机名直接使用 `~/.ssh/config` 里的 Host 别名，所以跳板机、密钥等配置都沿用 SSH 自己的。

窗口名规则：字母、数字、`_`、`-`，最长 32 个字符。

---

## 3. 命令

```text
arc run      <主机[/窗口]> '<命令>' [--wait 30s]   # 在窗口里执行
arc log      <主机[/窗口]> [--tail N]              # 看窗口的输出
arc kill     <主机[/窗口]>                         # 相当于按 Ctrl-C
arc sessions <主机>                                # 列出所有窗口
arc close    <主机[/窗口]> [--force]               # 关闭窗口
```

### 3.1 `arc run`

在窗口里执行命令，最多等 `--wait`（默认 30 秒）：

- **跑完了**：返回全部输出和退出码
- **没跑完**：返回目前为止的输出，命令继续在窗口里跑
- **窗口正忙**：直接报错，不执行

```text
$ arc run pdvm/train 'cd /data/proj && source .venv/bin/activate'
[exit 0 · pdvm/train · /data/proj]

$ arc run pdvm/train 'python train.py'
[running 30s · pdvm/train · /data/proj]
epoch 1/10 step 20 loss=2.71
hint: still running. `arc log pdvm/train` to check, `arc kill pdvm/train` to stop

$ arc run pdvm/train 'ls'
error: pdvm/train is busy (python train.py, 12m)
hint: use another window, e.g. `arc run pdvm/main 'ls'`
```

命令字符串当作 bash 脚本执行，所以 `&&`、管道、多行脚本都可以用。多行命令可以通过 stdin 传入：

```text
$ arc run pdvm/train - <<'EOF'
for f in cfg/*.yaml; do echo "$f"; done
EOF
```

### 3.2 `arc log`

显示窗口里**上次查看之后新增的**输出，并带上当前状态。反复调用时不会读到重复内容。

```text
$ arc log pdvm/train
[running 12m · pdvm/train · /data/proj]
epoch 3/10 step 900 loss=1.84

$ arc log pdvm/train            # 之后没有新输出
[running 12m · pdvm/train · /data/proj]
(no new output)

$ arc log pdvm/train --tail 100  # 不管看过没有，显示最后 100 行
```

命令跑完后，`log` 会显示它的退出码和剩下的输出，窗口随之恢复空闲。`run` 自己返回的输出也算"看过"，之后的 `log` 不会重复显示。

对一个从没用过的窗口 `log`，会显示 `idle` 和 `(no new output)`，但不会因此新建这个窗口。

`log` 的退出码规则和 `run` 一样（见第 4 节），所以 Agent 可以反复调用 `arc log`，直到退出码不再是 75。

### 3.3 `arc kill`

相当于在那个窗口里按 Ctrl-C。依次发送：SIGINT，等 5 秒；SIGTERM，等 5 秒；SIGKILL。信号发给整个进程组，命令启动的子进程也会一起停掉。一旦进程组里的进程全部退出，就不再发送后面的信号。

```text
$ arc kill pdvm/train
[exit 130 · pdvm/train · /data/proj]
saving checkpoint...
--- stderr ---
Traceback (most recent call last):
  ...
KeyboardInterrupt
stopped with SIGINT
```

- 和 `log` 一样，会带上还没看过的输出，Agent 能看到命令最后打印了什么。
- 最后一行写明最终用了哪个信号。只有到了 SIGKILL 这一步，状态才会是 `stopped`，命令所做的改动（cd 等）也不会保存。
- python 收到 SIGINT 会抛出 `KeyboardInterrupt`，训练脚本可以借此保存 checkpoint。
- 命令里用 `&` 放到后台的子进程会忽略 SIGINT（真终端里也是这样），它们会在 5 秒后被 SIGTERM 停掉。
- 窗口里没有在跑的命令时，显示 `(nothing was running)`。
- 只要窗口最终停下来了，`kill` 的退出码就是 0，命令自己的退出码看状态头。

### 3.4 `arc sessions`

```text
$ arc sessions pdvm
WINDOW  STATUS          DIR              COMMAND
main    exit 0          /home/parallels  ls
train   running 12m05s  /data/proj       python train.py
logs    exit 0          /var/log         tail -n 100 syslog
```

状态和状态头里的写法一样，COMMAND 是正在跑或最后一次跑的命令的第一行。中文路径按终端里的显示宽度对齐。

### 3.5 `arc close`

删除窗口及其状态，再用同一个名字就会得到一个全新的窗口。窗口正忙时会拒绝，并提示先 `kill` 或者加 `--force`；加 `--force` 会先 `kill` 再关闭。关闭一个不存在的窗口不算错误，会提示 `does not exist`，退出码为 0。

---

## 4. 输出格式

**第一行是状态头**，后面是原始输出：

```text
[<状态> · <主机/窗口> · <当前目录>]
...stdout...
--- stderr ---
...stderr...           ← 没有 stderr 时这部分不出现
hint: ...              ← 只在需要提示下一步时出现
```

状态有这几种：

| 状态 | 含义 |
|---|---|
| `exit N` | 命令已结束，退出码为 N |
| `running 12m05s` | 还在跑，已经跑了 12 分 5 秒 |
| `stopped` | 命令没有留下退出码就消失了（被 SIGKILL 或意外终止） |
| `idle` | 窗口还没跑过任何命令（用于 `log`） |

状态头里的 `主机/窗口` 和输入时的写法完全一样，Agent 可以直接照抄。命令还在跑时，状态头里的目录是命令**开始时**的目录。

**进度条**：用 `\r` 反复重绘的进度条（tqdm、curl 等），每行只显示最后一次重绘的内容，和终端里看到的一样，不会出现几百份重复的进度。

**截断**：stdout 和 stderr 分别超过 30KB 时，保留开头约 10KB 和结尾约 20KB，中间替换成一行省略标记，最后附一条提示：

```text
$ arc run pdvm 'seq 1 10000000'
[exit 0 · pdvm/main · /home/parallels]
1
2
...
2269
... [75.2 MB omitted, 9,995,172 lines] ...
9997442
...
10000000
hint: output truncated. In full: ~/.arc/sessions/main/stdout on the server, until this window runs another command
```

- 截断在**服务器端**完成，大量输出不会经过网络传回来：上面 75MB 的输出 0.5 秒就返回了，只传回 31KB。
- 截断点会挪到换行处，所以不会切断一行，也不会切断 UTF-8 字符。`\r` 也算换行处，这样进度条（几乎只用 `\r`、很少输出 `\n`）也能干净地截断。
- 省略标记写明省略了多少字节和多少行（没有换行时只写字节数）。
- 完整输出一直保存在服务器上的窗口文件里，直到这个窗口运行下一条命令为止。需要看中间部分时，Agent 可以在**另一个窗口**里用 `sed -n` 读取。之所以要换窗口，是因为在同一个窗口里运行命令，会把这个文件覆盖掉。
- `run`、`log`、`kill` 和 `log --tail` 的输出都会按这个规则截断；`log` 每次只截断新增的那一段。

### `arc` 自己的退出码

| 退出码 | 含义 |
|---|---|
| 和远程命令相同 | 命令已结束，`&&` 可以照常使用 |
| 75 | 命令还在跑（EX_TEMPFAIL，"稍后再查"） |
| 137 | 命令没有留下退出码就消失了（`stopped`，按 SIGKILL 的惯例） |
| 0 | `log` 一个空闲窗口；`kill`、`sessions`、`close` 成功 |
| 255 | arc 自身的错误：连接失败、窗口正忙、参数错误等（和 ssh 一致） |

---

## 5. 内部实现

### 5.1 整体结构

```text
   本地                                  服务器
 ┌──────────┐   系统 ssh（连接复用）   ┌──────────────────────────┐
 │ arc      │ ───────────────────────▶ │ 驱动脚本 (bash)           │
 │ (Python) │                          │  ├ 检查 / 加锁            │
 │          │ ◀─────────────────────── │  ├ 启动命令（独立进程组） │
 │ 格式化    │   状态行 + 截断后的输出  │  ├ 等待最多 --wait        │
 └──────────┘                          │  └ 读输出、截断、返回      │
                                       │ ~/.arc/sessions/<窗口>/   │
                                       └──────────────────────────┘
```

- **不常驻**：每次调用 `arc` 都只是执行一次 ssh，执行完就退出。本地不保存任何状态。
- **所有状态都在服务器的 `~/.arc/` 里**：本地机器换了也不影响，ssh 断开也不影响。

### 5.2 SSH 连接

调用系统的 `ssh`，由 arc 自动加上这些参数：

```text
-o ControlMaster=auto
-o ControlPath=~/.arc/cm/%C
-o ControlPersist=10m
-o BatchMode=yes             # 绝不弹出密码提示，避免卡住
-o ServerAliveInterval=15
```

只支持密钥登录，arc 永远不接触密码。

### 5.3 服务器上的文件

```text
~/.arc/sessions/<窗口>/
  cwd          当前目录
  vars.sh      所有 shell 变量（逐个 `declare -p` 的输出，可以直接 source）
  funcs.sh     所有 shell 函数（`declare -f` 的输出）
  lock         `flock` 用的锁文件
  pid          正在跑或最后一次跑的命令的进程号（= 进程组号）和启动时间
  cmd          正在跑或最后一次跑的命令
  started      开始时间（Unix 秒）
  stdout       最后一条命令的 stdout
  stderr       最后一条命令的 stderr
  exit         退出码；命令结束时才写入
  offset       `log` 上次读到的位置（stdout 和 stderr 各一个）
```

### 5.4 `run` 的执行流程

1. **本地**：执行 `ssh <参数> <主机> 'bash -s'`，把驱动脚本从 ssh 的 stdin 发过去；用户命令以 base64 编码后放在脚本开头。这样用户命令里有什么引号都不需要转义，也不受命令行参数长度的限制（Linux 单个参数最长约 128KB）。显式使用 `bash`，是因为服务器上的登录 shell 可能是 zsh 或 fish。
2. **加锁**：`flock lock`，防止两个 `run` 同时在同一个窗口里启动命令。用 `flock` 而不是 `mkdir`，是因为持有锁的进程一旦意外死掉，锁会自动释放，窗口不会被永久锁死。锁只在"检查 + 启动"这一小段时间里持有，启动的命令不会继承它。
3. **检查是否忙碌**：存在 `pid` 并且进程还活着，就报"窗口正忙"并退出。
4. **启动命令**：打开作业控制（`set -m`）后用 `&` 在后台启动，stdin 接 `/dev/null`，stdout 和 stderr 分别写入文件。
   - 作业控制让命令有自己的进程组（进程组号 = `$!`），`kill` 时可以整组停掉。
   - 不打开作业控制的话，非交互式 bash 会让后台命令**忽略 SIGINT**（POSIX 规定），`kill` 发的 Ctrl-C 就完全无效。这也是没有用 `setsid` 的原因：在 `set -m` 下子进程已经是进程组组长，`setsid` 会再 fork 一次，`$!` 就不再是命令的进程号了。
   - 同时记录进程的启动时间（`/proc/<pid>/stat` 的第 22 个字段），用来分辨"还是这个命令"和"pid 被别的进程复用了"。只检查环境变量不够可靠：刚 fork 出来、还没 exec 的进程，环境变量还是父进程的。

   窗口的 shell 里依次执行：
   ```sh
   . vars.sh; . funcs.sh                    # 恢复变量和函数
   export PAGER=cat ...                     # 强制非交互设置（见 5.6）
   cd "$(<cwd)"                             # 恢复目录；目录被删了就回到家目录并提示
   trap '保存 cwd、vars.sh、funcs.sh' EXIT
   trap 'exit 130' INT; trap 'exit 143' TERM  # 和终端一样的退出码，并且会触发保存
   eval "$用户命令"                          # 执行
   ```
   - 用 `trap EXIT` 来保存状态，这样即使用户命令里写了 `exit 3`，状态也会保存。写入退出码的步骤也放在这里。
   - 窗口一旦有了保存的状态，就用 `env -i` 从空环境启动、完全靠 `vars.sh` 恢复，这样被 `unset` 的变量不会因为 ssh 登录环境里有它而又冒出来。新窗口则继承 ssh 的登录环境。
   - arc 自己用到的变量都以 `__arc_` 开头，保存时跳过；bash 内部的变量（`PPID`、`RANDOM`、`SHLVL` 等）和每次连接都会变的 `SSH_*` 也跳过。
   - 用户命令写在临时文件里传给窗口的 shell，而不是放在环境变量里，因为单个环境变量同样有约 128KB 的长度限制。
5. **解锁**，然后**等待**：每 20 毫秒检查一次是否已经有 `exit` 文件、进程是否还活着，最多等 `--wait` 秒。等待用的是对一个没人写入的 fifo 做 `read -t 0.02`，这是 bash 的内置命令，不需要 fork，所以短命令几乎没有额外延迟。
6. **返回结果**：第一行输出一个机器可读的状态行，本地 arc 解析后再格式化成状态头；接着输出截断后的 stdout 和 stderr，并更新 `offset`。截断时驱动脚本只按字节切出开头和结尾两段，并统计中间部分的换行数；把截断点挪到换行处、生成省略标记，是本地 arc 做的事。

因为命令的输出写在文件里、stdin 是 `/dev/null`，它不占用 ssh 连接的任何东西，所以**等待期间 ssh 断开或者本地按了 Ctrl-C，都不会影响命令本身**。

### 5.5 状态判断（`log` / `sessions` 共用）

按顺序判断，第一个满足的条件决定状态：

| 条件 | 状态 |
|---|---|
| 有 `exit` 文件 | exit N |
| `pid` 存在，进程活着，并且启动时间和记录的一致 | running |
| `pid` 存在，进程已经没了 | stopped（被 SIGKILL 或意外终止） |
| 以上都不是 | idle |

`exit` 文件放在最前面判断：它是 `trap` 在状态全部保存好之后最后写入的，只要它存在，命令就一定已经结束了。

被 SIGKILL 杀掉时 `trap` 不会执行，所以窗口会保留**这条命令开始之前**的目录和环境变量。

### 5.6 默认环境变量

每个窗口都会强制设置下面这些变量，避免命令卡住或者输出不及时：

```sh
PAGER=cat  GIT_PAGER=cat  TERM=dumb
DEBIAN_FRONTEND=noninteractive
PYTHONUNBUFFERED=1      # 否则 python 的输出会被缓冲，log 里长时间看不到内容
```

---

## 6. 代码结构

只用 Python 标准库，没有第三方依赖，要求 Python ≥ 3.9。

```text
arc/
  cli.py        命令行解析，把 主机/窗口 拆开
  ssh.py        调用 ssh，加上连接复用参数
  driver.sh     服务器端驱动脚本（每次调用时编码后发送）
  protocol.py   生成驱动脚本的输入，解析返回的状态行
  output.py     格式化状态头和提示
tests/          集成测试，直接连接 pdvm
DESIGN.md
README.md
```

预计总共三四百行代码。

---

## 7. 开发计划

每一步都在 Parallels 虚拟机 `pdvm` 上验证。

| 步骤 | 内容 | 完成标准 |
|---|---|---|
| 1 | 基础 `run` | `arc run pdvm 'echo hi; ls /nope'` 能分开显示 stdout 和 stderr，退出码是 2 |
| 2 | 窗口状态 | 在 `pdvm/a` 里 `cd /tmp` 后，下一次执行 `pwd` 得到 `/tmp`，而 `pdvm/b` 不受影响；激活 venv 后 `which python` 指向 venv |
| 3 | 长命令 | `arc run pdvm 'sleep 60; echo done'` 30 秒后返回 `running`；之后 `log` 看到 `done` 和 `exit 0`；窗口正忙时 `run` 报错 |
| 4 | `kill` / `sessions` / `close` | `kill` 之后进程组里所有进程都消失；`sessions` 显示的状态都正确 |
| 5 | 截断 | `seq 1e7` 的结果被截断，并且很快返回 |
| 6 | 给 Agent 用 | 写一份 Claude Code 使用说明，让它自己在 pdvm 上完成一个"跑训练脚本、看日志、改参数"的小任务 |

---

## 8. 待定和待验证

- **名字**：`arc` 需要换掉。
- **conda**：conda 的初始化代码一般写在 `.bashrc` 里，而非交互式 shell 可能不会加载 `.bashrc`，所以新窗口里可能没有 `conda` 命令。由于窗口会记住函数，在窗口里执行一次 `eval "$(conda shell.bash hook)"` 之后应该就能一直用 `conda activate`。pdvm 上没有装 conda，需要找一台装了 conda 的机器验证。
- **退出码 75**：用来表示"还在跑"是否合适，等 Agent 实际用起来再看。
