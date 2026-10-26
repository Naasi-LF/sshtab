"""Command-line entry point: parses `arc <action> <host[/window]> ...`."""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass

from arc import output, protocol, ssh

DEFAULT_WINDOW = "main"
WINDOW_RE = re.compile(r"[A-Za-z0-9_-]{1,32}")

# arc's own failures (bad arguments, connection errors) exit 255, like ssh.
EXIT_ARC_ERROR = 255


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
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        target = parse_target(args.target)
    except ValueError as exc:
        return _fail(str(exc))
    return cmd_run(target, args.command)


def cmd_run(target: Target, command: str) -> int:
    if command == "-":
        command = sys.stdin.read()
    proc = ssh.run_script(target.host, protocol.build_script(command))
    try:
        result = protocol.parse(proc.stdout)
    except protocol.ProtocolError:
        detail = proc.stderr.decode(errors="replace").strip()
        return _fail(f"cannot run on {target.host}: {detail or 'no response from remote driver'}")
    sys.stdout.write(output.render(result, str(target)))
    return result.code


def _fail(message: str) -> int:
    print(f"error: {message}", file=sys.stderr)
    return EXIT_ARC_ERROR
