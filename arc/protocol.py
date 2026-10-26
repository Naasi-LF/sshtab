"""Wire format between arc and the remote driver (arc/driver.sh).

arc sends the driver on ssh's stdin, with its parameters prepended as shell
assignments; the user's command is base64 so it never needs quoting. The
driver answers with one header line followed by raw stdout and stderr bytes:

    ARC1 status=running code=0 cwd=<base64> elapsed=31 cmd=<base64> signal= out_cut= err_cut= out=<n bytes> err=<n bytes>\\n<stdout><stderr>

status is a window state (exit, running, stopped, idle) or an action outcome
(busy, closed, missing, list). The streams are delimited by their lengths,
not by markers, so any output is safe. For `list`, stdout holds one line per
window; see parse_windows.

A stream that was too long arrives as its first and last bytes only, and its
*_cut field says "<head bytes>:<omitted bytes>:<omitted newlines>".
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
    truncated: tuple = ()  # names of the streams whose middle was cut out


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
        stdout = _stream(body[:out_len], fields.get("out_cut", ""))
        stderr = _stream(body[out_len:out_len + err_len], fields.get("err_cut", ""))
        result = Result(
            status=fields["status"],
            code=int(fields["code"]),
            cwd=_unb64(fields["cwd"]),
            stdout=stdout,
            stderr=stderr,
            elapsed=int(fields.get("elapsed", 0)),
            cmd=_unb64(fields.get("cmd", "")),
            signal=fields.get("signal", ""),
            truncated=tuple(
                name for name, key in [("stdout", "out_cut"), ("stderr", "err_cut")] if fields.get(key)
            ),
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


def _stream(data: bytes, cut: str) -> str:
    """Decodes a stream; if the driver cut its middle out, marks where."""
    if not cut:
        return _text(data)
    head_len, omitted, lines = (int(n) for n in cut.split(":"))
    head, tail = data[:head_len], data[head_len:]
    # Move both cuts to line breaks, so no line or UTF-8 character is split.
    # \r counts as a break too: progress bars redraw with \r and rarely print \n.
    keep = max(head.rfind(b"\n"), head.rfind(b"\r")) + 1
    if keep:
        omitted += len(head) - keep
        head = head[:keep]
    breaks = [i for i in (tail.find(b"\n"), tail.find(b"\r")) if i >= 0]
    if breaks:
        dropped = tail[:min(breaks) + 1]
        omitted += len(dropped)
        lines += dropped.count(b"\n")
        tail = tail[len(dropped):]
    separator = "" if head.endswith((b"\n", b"\r")) or not head else "\n"
    count = f", {lines:,} lines" if lines else ""
    marker = f"... [{_size(omitted)} omitted{count}] ...\n"
    return _text(head) + separator + marker + _text(tail)


def _size(n: int) -> str:
    if n < 1024:
        return f"{n} bytes"
    size = n / 1024
    for unit in ("KB", "MB"):
        if size < 1024:
            return f"{size:.1f} {unit}"
        size /= 1024
    return f"{size:.1f} GB"


def _unb64(value: str) -> str:
    return _text(base64.b64decode(value))


def _text(data: bytes) -> str:
    return data.decode(errors="replace")
