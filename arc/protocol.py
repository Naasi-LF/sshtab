"""Wire format between arc and the remote driver (arc/driver.sh).

arc sends the driver on ssh's stdin, with its parameters prepended as shell
assignments; the user's command is base64 so it never needs quoting. The
driver answers with one header line followed by raw stdout and stderr bytes:

    ARC1 status=running code=0 cwd=<base64> elapsed=31 cmd=<base64> signal= out=<n bytes> err=<n bytes>\\n<stdout><stderr>

status is a window state (exit, running, stopped, idle) or an action outcome
(busy, closed, missing, list). The streams are delimited by their lengths,
not by markers, so any output is safe. For `list`, stdout holds one line per
window; see parse_windows.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass
from importlib import resources

MAGIC = b"ARC1 "


class ProtocolError(Exception):
    pass


@dataclass
class Result:
    status: str
    code: int
    cwd: str
    stdout: str
    stderr: str
    elapsed: int = 0  # seconds since the window's last command started
    cmd: str = ""  # first line of that command
    signal: str = ""  # last signal `kill` sent (INT, TERM or KILL), if any


def build_script(action: str, window: str = "", **params: str | int) -> str:
    """Values are pasted into bash unquoted: only validated names, numbers and base64."""
    lines = [f"ARC_ACTION={action}", f"ARC_WINDOW={window}"]
    lines += [f"ARC_{key.upper()}={value}" for key, value in params.items()]
    driver = resources.files("arc").joinpath("driver.sh").read_text()
    return "\n".join(lines) + "\n" + driver


def encode_command(command: str) -> str:
    return base64.b64encode(command.encode()).decode()


def parse(raw: bytes) -> Result:
    # Server startup files (e.g. a chatty .zshenv) may print before the driver runs.
    if raw.startswith(MAGIC):
        start = 0
    else:
        start = raw.find(b"\n" + MAGIC) + 1
        if start == 0:
            raise ProtocolError("no header from remote driver")

    header, newline, body = raw[start:].partition(b"\n")
    try:
        if not newline:
            raise ValueError("missing newline after header")
        fields = dict(item.split("=", 1) for item in header.decode().split()[1:])
        out_len, err_len = int(fields["out"]), int(fields["err"])
        result = Result(
            status=fields["status"],
            code=int(fields["code"]),
            cwd=_unb64(fields["cwd"]),
            stdout=_text(body[:out_len]),
            stderr=_text(body[out_len:out_len + err_len]),
            elapsed=int(fields.get("elapsed", 0)),
            cmd=_unb64(fields.get("cmd", "")),
            signal=fields.get("signal", ""),
        )
    except (KeyError, ValueError) as exc:
        raise ProtocolError(f"bad header {header!r}: {exc}") from exc
    if len(body) < out_len + err_len:
        raise ProtocolError("response ended early")
    return result


def parse_windows(stdout: str) -> list[tuple[str, Result]]:
    """The `list` reply: one tab-separated line per window, as written by driver.sh."""
    windows = []
    for line in stdout.splitlines():
        try:
            name, state, code, elapsed, cwd, cmd = line.split("\t")
            windows.append((name, Result(
                state, int(code), _unb64(cwd), "", "", elapsed=int(elapsed), cmd=_unb64(cmd),
            )))
        except ValueError as exc:
            raise ProtocolError(f"bad window line {line!r}: {exc}") from exc
    return windows


def _unb64(value: str) -> str:
    return _text(base64.b64decode(value))


def _text(data: bytes) -> str:
    return data.decode(errors="replace")
