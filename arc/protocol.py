"""Wire format between arc and the remote driver (arc/driver.sh).

arc sends the driver on ssh's stdin, with the user's command prepended as
base64 so it never needs quoting. The driver answers with one header line
followed by the raw stdout and stderr bytes:

    ARC1 status=exit code=2 cwd=<base64> out=<n bytes> err=<n bytes>\\n<stdout><stderr>

The streams are delimited by their lengths, not by markers, so any output is safe.
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


def build_script(command: str) -> str:
    encoded = base64.b64encode(command.encode()).decode()
    driver = resources.files("arc").joinpath("driver.sh").read_text()
    return f"ARC_CMD_B64={encoded}\n{driver}"


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
            cwd=base64.b64decode(fields["cwd"]).decode(errors="replace"),
            stdout=_text(body[:out_len]),
            stderr=_text(body[out_len:out_len + err_len]),
        )
    except (KeyError, ValueError) as exc:
        raise ProtocolError(f"bad header {header!r}: {exc}") from exc
    if len(body) < out_len + err_len:
        raise ProtocolError("response ended early")
    return result


def _text(data: bytes) -> str:
    return data.decode(errors="replace")
