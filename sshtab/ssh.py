"""Thin wrapper around the system ssh client, so ~/.ssh/config just works."""

from __future__ import annotations

import subprocess
from pathlib import Path

CONTROL_DIR = Path.home() / ".sshtab" / "cm"


def ssh_command(host: str) -> list[str]:
    return [
        "ssh",
        # Reuse one connection per host: ~20ms per command instead of a full handshake.
        "-o", "ControlMaster=auto",
        "-o", f"ControlPath={CONTROL_DIR}/%C",
        "-o", "ControlPersist=10m",
        # Never prompt for a password: a prompt would hang the agent.
        "-o", "BatchMode=yes",
        "-o", "ConnectTimeout=10",
        "-o", "ServerAliveInterval=15",
        "--", host,
        # Explicit bash: the user's login shell on the server may be zsh, fish, ...
        "bash -s",
    ]


def run_script(host: str, script: str) -> subprocess.CompletedProcess:
    """Feed `script` to bash on `host` and capture its raw output."""
    CONTROL_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
    return subprocess.run(ssh_command(host), input=script.encode(), capture_output=True)
