"""End-to-end tests against a real server, `pdvm` unless ARC_TEST_HOST says otherwise.

Skipped when the host is unreachable.
"""

import os
import subprocess
import sys
import time
import unittest
from pathlib import Path

HOST = os.environ.get("ARC_TEST_HOST", "pdvm")
REPO = Path(__file__).resolve().parent.parent


def host_reachable() -> bool:
    probe = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", HOST, "true"]
    return subprocess.run(probe, capture_output=True).returncode == 0


def arc(*args: str, stdin: str = "") -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "arc", *args],
        cwd=REPO, input=stdin, capture_output=True, text=True,
    )


@unittest.skipUnless(host_reachable(), f"{HOST} is not reachable over ssh")
class RunTest(unittest.TestCase):
    def test_separates_stdout_and_stderr_and_keeps_exit_code(self):
        proc = arc("run", HOST, "echo hi; ls /nope")
        self.assertEqual(proc.returncode, 2)
        header, rest = proc.stdout.split("\n", 1)
        self.assertRegex(header, rf"^\[exit 2 · {HOST}/main · /.+\]$")
        out, err = rest.split("--- stderr ---\n")
        self.assertEqual(out, "hi\n")
        self.assertIn("/nope", err)

    def test_exit_code_passthrough(self):
        self.assertEqual(arc("run", HOST, "exit 7").returncode, 7)

    def test_quotes_and_dollars_need_no_escaping(self):
        proc = arc("run", HOST, """echo "it's" '$HOME' "$HOME" `echo tick` | tr a-z A-Z""")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        line = proc.stdout.splitlines()[1]
        self.assertTrue(line.startswith("IT'S $HOME /HOME/"), line)
        self.assertTrue(line.endswith(" TICK"), line)

    def test_multiline_command_from_stdin(self):
        script = "for i in 1 2 3; do\n  echo line $i\ndone\n"
        proc = arc("run", HOST, "-", stdin=script)
        self.assertEqual(proc.stdout.splitlines()[1:], ["line 1", "line 2", "line 3"])

    def test_stdin_is_closed_so_nothing_hangs(self):
        start = time.monotonic()
        proc = arc("run", HOST, "cat; read x; echo done")
        self.assertLess(time.monotonic() - start, 10)
        self.assertEqual(proc.stdout.splitlines()[1:], ["done"])

    def test_non_interactive_environment(self):
        proc = arc("run", HOST, "echo $PAGER $GIT_PAGER $TERM $PYTHONUNBUFFERED")
        self.assertEqual(proc.stdout.splitlines()[1], "cat cat dumb 1")

    def test_binary_output_does_not_crash(self):
        proc = arc("run", HOST, r"printf '\377\000ok'")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("ok", proc.stdout)

    def test_window_name_shows_in_header(self):
        proc = arc("run", f"{HOST}/train", "true")
        self.assertIn(f"· {HOST}/train ·", proc.stdout)


class ErrorTest(unittest.TestCase):
    def test_bad_window_name(self):
        proc = arc("run", "pdvm/bad name", "true")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("invalid window name", proc.stderr)

    def test_missing_arguments(self):
        self.assertEqual(arc("run", "pdvm").returncode, 255)

    def test_unknown_host(self):
        proc = arc("run", "no-such-host.invalid", "true")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("cannot run on no-such-host.invalid", proc.stderr)


if __name__ == "__main__":
    unittest.main()
