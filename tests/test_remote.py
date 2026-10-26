"""End-to-end tests against a real server, `pdvm` unless ARC_TEST_HOST says otherwise.

Skipped when the host is unreachable.
"""

import os
import subprocess
import sys
import time
import unittest
import uuid
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


@unittest.skipUnless(host_reachable(), f"{HOST} is not reachable over ssh")
class WindowStateTest(unittest.TestCase):
    """Each test gets fresh, uniquely named windows that are deleted afterwards."""

    def setUp(self):
        self.cleanup = []

    def tearDown(self):
        # Explicit paths, no globs: the login shell may be zsh, which aborts on no match.
        quoted = " ".join(f"'{p}'" if p.startswith("/") else p for p in self.cleanup)
        subprocess.run(["ssh", HOST, f"rm -rf {quoted}"], check=True)

    def window(self) -> str:
        name = f"t-{uuid.uuid4().hex[:8]}"
        self.cleanup.append(f"~/.arc/sessions/{name}")
        return f"{HOST}/{name}"

    def tmp_path(self, suffix: str = "") -> str:
        path = f"/tmp/arc-test-{uuid.uuid4().hex[:8]}{suffix}"
        self.cleanup.append(path)
        return path

    def run_in(self, target: str, command: str) -> tuple:
        """Returns (header, output lines) and fails the test on a nonzero exit."""
        proc = arc("run", target, command)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        header, *lines = proc.stdout.splitlines()
        return header, lines

    def test_window_name_shows_in_header(self):
        w = self.window()
        self.assertEqual(self.run_in(w, "true")[0], f"[exit 0 · {w} · {self.run_in(w, 'pwd')[1][0]}]")

    def test_cd_is_remembered(self):
        w = self.window()
        header, _ = self.run_in(w, "cd /tmp")
        self.assertEqual(header, f"[exit 0 · {w} · /tmp]")
        self.assertEqual(self.run_in(w, "pwd")[1], ["/tmp"])

    def test_windows_are_independent(self):
        a, b = self.window(), self.window()
        self.run_in(a, "cd /tmp && export ONLY_A=1")
        self.assertEqual(self.run_in(b, "pwd; echo ${ONLY_A-unset}")[1][1], "unset")
        self.assertNotEqual(self.run_in(b, "pwd")[1], ["/tmp"])

    def test_new_window_starts_at_home(self):
        home = self.run_in(self.window(), 'echo "$HOME"')[1][0]
        self.assertEqual(self.run_in(self.window(), "pwd")[1], [home])

    def test_variables_and_functions_are_remembered(self):
        w = self.window()
        self.run_in(w, "plain=1; export EXPORTED=2; declare -a arr=(a 'b c'); greet() { echo hi $1; }")
        _, lines = self.run_in(w, 'echo "$plain $EXPORTED ${arr[1]}"; greet bob; sh -c "echo \\$EXPORTED"')
        self.assertEqual(lines, ["1 2 b c", "hi bob", "2"])

    def test_unset_is_remembered(self):
        w = self.window()
        self.run_in(w, "unset HOME_BACKUP LANG; export LANG")
        self.assertEqual(self.run_in(w, 'echo "${LANG-unset}"')[1], ["unset"])

    def test_state_is_saved_even_when_command_exits(self):
        w = self.window()
        proc = arc("run", w, "cd /tmp && exit 3")
        self.assertEqual(proc.returncode, 3)
        self.assertIn("· /tmp]", proc.stdout.splitlines()[0])
        self.assertEqual(self.run_in(w, "pwd")[1], ["/tmp"])

    def test_cd_dash_returns_to_previous_directory(self):
        w = self.window()
        self.run_in(w, "cd /tmp")
        self.run_in(w, "cd /etc")
        self.assertEqual(self.run_in(w, "cd - >/dev/null; pwd")[1], ["/tmp"])

    def test_directory_with_spaces_and_unicode(self):
        w = self.window()
        d = self.tmp_path(" 测试")
        header, _ = self.run_in(w, f"mkdir -p '{d}' && cd '{d}'")
        self.assertTrue(header.endswith(f"· {d}]"), header)
        self.assertEqual(self.run_in(w, "pwd")[1], [d])

    def test_deleted_directory_falls_back_to_home(self):
        w = self.window()
        d = self.tmp_path()
        self.run_in(w, f"mkdir -p {d} && cd {d}")
        self.run_in(self.window(), f"rm -rf {d}")
        proc = arc("run", w, "pwd")
        self.assertIn("no longer exists", proc.stdout)
        self.assertNotIn(d, proc.stdout.splitlines()[0])

    def test_forced_non_interactive_settings_win(self):
        w = self.window()
        self.run_in(w, "export PAGER=less")
        self.assertEqual(self.run_in(w, "echo $PAGER")[1], ["cat"])

    def test_venv_activate_and_deactivate(self):
        w = self.window()
        venv = self.tmp_path("-venv")
        self.run_in(w, f"python3 -m venv --without-pip {venv} && source {venv}/bin/activate")
        _, lines = self.run_in(w, "echo $VIRTUAL_ENV; command -v python")
        self.assertEqual(lines, [venv, f"{venv}/bin/python"])
        self.run_in(w, "deactivate")
        _, lines = self.run_in(w, 'echo "${VIRTUAL_ENV-none}"; echo "$PATH"')
        self.assertEqual(lines[0], "none")
        self.assertNotIn(venv, lines[1])


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
