"""What the agent sees: a status header line, then the raw output."""

from __future__ import annotations

import unicodedata

from arc.protocol import Result


def header(status: str, target: str, cwd: str) -> str:
    return f"[{status} · {target} · {cwd}]"


def status_label(result: Result) -> str:
    if result.status == "exit":
        return f"exit {result.code}"
    if result.status == "running":
        return f"running {duration(result.elapsed)}"
    return result.status


def duration(seconds: int) -> str:
    if seconds < 60:
        return f"{seconds}s"
    if seconds < 3600:
        return f"{seconds // 60}m{seconds % 60:02d}s"
    return f"{seconds // 3600}h{seconds % 3600 // 60:02d}m"


def render(result: Result, target: str, empty_note: str = "") -> str:
    stdout, stderr = _as_terminal(result.stdout), _as_terminal(result.stderr)
    parts = [header(status_label(result), target, result.cwd) + "\n"]
    if stdout:
        parts.append(_with_newline(stdout))
    if stderr:
        parts.append("--- stderr ---\n")
        parts.append(_with_newline(stderr))
    if not stdout and not stderr and empty_note:
        parts.append(empty_note + "\n")
    if result.truncated:
        window = target.split("/", 1)[1]
        files = " and ".join(f"~/.arc/sessions/{window}/{name}" for name in result.truncated)
        parts.append(
            f"hint: output truncated. In full: {files} on the server, "
            "until this window runs another command\n"
        )
    if result.status == "running":
        parts.append(f"hint: still running. `arc log {target}` to check, `arc kill {target}` to stop\n")
    return "".join(parts)


def render_windows(windows: list[tuple[str, Result]]) -> str:
    if not windows:
        return "(no windows)\n"
    rows = [("WINDOW", "STATUS", "DIR", "COMMAND")]
    rows += [(name, status_label(r), r.cwd, r.cmd) for name, r in windows]
    widths = [max(_width(row[i]) for row in rows) for i in range(len(rows[0]))]
    return "".join(
        "  ".join(cell + " " * (width - _width(cell)) for cell, width in zip(row, widths)).rstrip()
        + "\n"
        for row in rows
    )


def _width(text: str) -> int:
    """Terminal columns: CJK and other wide characters take two."""
    return sum(2 if unicodedata.east_asian_width(c) in "WF" else 1 for c in text)


def _as_terminal(text: str) -> str:
    """Show `\\r` redraws (tqdm, curl progress) as a terminal would: only the last one."""
    return "\n".join(line.rstrip("\r").rsplit("\r", 1)[-1] for line in text.split("\n"))


def _with_newline(text: str) -> str:
    return text if text.endswith("\n") else text + "\n"
