"""Command-line entry point: parses `arc <action> <host[/window]> ...`."""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass

from arc import output, protocol, ssh

DEFAULT_WINDOW = "main"
WINDOW_RE = re.compile(r"[A-Za-z0-9_-]{1,32}")
DURATION_RE = re.compile(r"(\d+)([smh]?)")

# arc's own failures (bad arguments, connection errors, busy window) exit 255, like ssh.
EXIT_ARC_ERROR = 255
# The command is still running: "try again later" (sysexits EX_TEMPFAIL).
EXIT_RUNNING = 75
# The command died without reporting an exit code, as after SIGKILL.
EXIT_STOPPED = 137


@dataclass(frozen=True)
class Target:
    host: str
    window: str

    def __str__(self) -> str:
        return f"{self.host}/{self.window}"


def parse_target(text: str) -> Target:
    host, slash, window = text.partition("/")
    if not host or host.startswith("-") or any(c.isspace() for c in host):
        raise ValueError(f"invalid host: {host!r}")
    if not slash:
        window = DEFAULT_WINDOW
    elif not WINDOW_RE.fullmatch(window):
        raise ValueError(
            f"invalid window name: {window!r} "
            "(letters, digits, '_' and '-', at most 32 characters)"
        )
    return Target(host, window)


def parse_duration(text: str) -> int:
    """'30', '30s', '5m' or '2h' to seconds."""
    match = DURATION_RE.fullmatch(text)
    if not match:
        raise argparse.ArgumentTypeError(f"invalid duration: {text!r} (e.g. 30s, 5m, 2h)")
    number, unit = match.groups()
    return int(number) * {"": 1, "s": 1, "m": 60, "h": 3600}[unit]


def non_negative_int(text: str) -> int:
    if not text.isdigit():
        raise argparse.ArgumentTypeError(f"not a line count: {text!r}")
    return int(text)


class _Parser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        self.print_usage(sys.stderr)
        self.exit(EXIT_ARC_ERROR, f"arc: error: {message}\n")


def build_parser() -> argparse.ArgumentParser:
    parser = _Parser(prog="arc", description="Remote shell for AI agents.")
    actions = parser.add_subparsers(dest="action", required=True, parser_class=_Parser)

    run = actions.add_parser("run", help="run a command in a window")
    run.add_argument("target", help="host or host/window (default window: main)")
    run.add_argument("command", help="shell command, or - to read it from stdin")
    run.add_argument(
        "--wait", type=parse_duration, default=30, metavar="DURATION",
        help="return after this long even if the command is still running "
             "(it keeps running in the window; default 30s)",
    )

    log = actions.add_parser("log", help="show a window's output since you last looked")
    log.add_argument("target", help="host or host/window (default window: main)")
    log.add_argument(
        "--tail", type=non_negative_int, metavar="N",
        help="show the last N lines instead, whether seen before or not",
    )

    kill = actions.add_parser(
        "kill", help="stop a window's command: Ctrl-C, then SIGTERM, then SIGKILL",
    )
    kill.add_argument("target", help="host or host/window (default window: main)")

    sessions = actions.add_parser("sessions", help="list a host's windows")
    sessions.add_argument("target", metavar="host")

    close = actions.add_parser("close", help="delete a window and its state")
    close.add_argument("target", help="host or host/window (default window: main)")
    close.add_argument("--force", action="store_true", help="stop its command first if running")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        target = parse_target(args.target)
        if args.action == "sessions" and "/" in args.target:
            raise ValueError(f"sessions takes a host, not a window: try `arc sessions {target.host}`")
    except ValueError as exc:
        return _fail(str(exc))
    if args.action == "run":
        return cmd_run(target, args.command, args.wait)
    if args.action == "log":
        return cmd_log(target, args.tail)
    if args.action == "kill":
        return cmd_kill(target)
    if args.action == "sessions":
        return cmd_sessions(target.host)
    return cmd_close(target, args.force)


def cmd_run(target: Target, command: str, wait: int) -> int:
    if command == "-":
        command = sys.stdin.read()
    script = protocol.build_script(
        "run", target.window, cmd_b64=protocol.encode_command(command), wait=wait,
    )
    result = _call(target.host, script)
    if result is None:
        return EXIT_ARC_ERROR
    if result.status == "busy":
        other = "main" if target.window != DEFAULT_WINDOW else "main2"
        return _fail(
            f"{_busy(target, result)}\n"
            f"hint: use another window, e.g. `arc run {target.host}/{other} '...'`, "
            f"or `arc kill {target}` to stop it"
        )
    sys.stdout.write(output.render(result, str(target)))
    return _exit_code(result)


def cmd_log(target: Target, tail: int | None) -> int:
    script = protocol.build_script("log", target.window, tail="" if tail is None else tail)
    result = _call(target.host, script)
    if result is None:
        return EXIT_ARC_ERROR
    sys.stdout.write(output.render(result, str(target), empty_note="(no new output)"))
    return _exit_code(result)


def cmd_kill(target: Target) -> int:
    result = _call(target.host, protocol.build_script("kill", target.window))
    if result is None:
        return EXIT_ARC_ERROR
    if result.status == "running":
        return _fail(f"could not stop {target}: still running after SIGKILL")
    sys.stdout.write(output.render(result, str(target)))
    print(f"stopped with SIG{result.signal}" if result.signal else "(nothing was running)")
    return 0


def cmd_sessions(host: str) -> int:
    result = _call(host, protocol.build_script("list"))
    if result is None:
        return EXIT_ARC_ERROR
    try:
        windows = protocol.parse_windows(result.stdout)
    except protocol.ProtocolError as exc:
        return _fail(f"bad reply from {host}: {exc}")
    sys.stdout.write(output.render_windows(windows))
    return 0


def cmd_close(target: Target, force: bool) -> int:
    script = protocol.build_script("close", target.window, force="1" if force else "")
    result = _call(target.host, script)
    if result is None:
        return EXIT_ARC_ERROR
    if result.status == "busy":
        hint = "" if force else (
            f"\nhint: `arc kill {target}` first, or `arc close {target} --force`"
        )
        return _fail(_busy(target, result) + hint)
    if result.status == "missing":
        print(f"{target} does not exist (nothing to close)")
    else:
        print(f"closed {target}")
    return 0


def _busy(target: Target, result: protocol.Result) -> str:
    return f"{target} is busy ({result.cmd}, {output.duration(result.elapsed)})"


def _call(host: str, script: str) -> protocol.Result | None:
    proc = ssh.run_script(host, script)
    try:
        return protocol.parse(proc.stdout)
    except protocol.ProtocolError:
        detail = proc.stderr.decode(errors="replace").strip()
        _fail(f"cannot run on {host}: {detail or 'no response from remote driver'}")
        return None


def _exit_code(result: protocol.Result) -> int:
    if result.status == "exit":
        return result.code
    if result.status == "running":
        return EXIT_RUNNING
    if result.status == "stopped":
        return EXIT_STOPPED
    return 0


def _fail(message: str) -> int:
    print(f"error: {message}", file=sys.stderr)
    return EXIT_ARC_ERROR
