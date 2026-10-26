"""What the agent sees: a status header line, then the raw output."""

from __future__ import annotations

from arc.protocol import Result


def header(status: str, target: str, cwd: str) -> str:
    return f"[{status} · {target} · {cwd}]"


def render(result: Result, target: str) -> str:
    parts = [header(f"{result.status} {result.code}", target, result.cwd) + "\n"]
    if result.stdout:
        parts.append(_with_newline(result.stdout))
    if result.stderr:
        parts.append("--- stderr ---\n")
        parts.append(_with_newline(result.stderr))
    return "".join(parts)


def _with_newline(text: str) -> str:
    return text if text.endswith("\n") else text + "\n"
