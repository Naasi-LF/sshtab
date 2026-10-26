"""What the agent sees: a status header line, then the raw output."""

from __future__ import annotations

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
    if result.status == "running":
        parts.append(f"hint: still running. `arc log {target}` to check\n")
    return "".join(parts)


def _as_terminal(text: str) -> str:
    """Show `\\r` redraws (tqdm, curl progress) as a terminal would: only the last one."""
    return "\n".join(line.rstrip("\r").rsplit("\r", 1)[-1] for line in text.split("\n"))


def _with_newline(text: str) -> str:
    return text if text.endswith("\n") else text + "\n"
