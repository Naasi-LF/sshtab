"""End-to-end tests against a real server, `pdvm` unless ARC_TEST_HOST says otherwise.

Skipped when the host is unreachable.
"""

import os
import re
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
class WindowTestCase(unittest.TestCase):
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

    def wait_until_done(self, target: str, timeout: float = 15) -> list:
        """Polls `arc log` until the command finishes; returns every log reply."""
        replies = []
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            proc = arc("log", target)
            replies.append(proc)
            if proc.returncode != 75:
                return replies
            time.sleep(0.3)
        self.fail(f"{target} still running after {timeout}s")


class WindowStateTest(WindowTestCase):
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


class LongCommandTest(WindowTestCase):
    def test_returns_after_wait_and_keeps_running(self):
        w = self.window()
        start = time.monotonic()
        proc = arc("run", w, "echo started; sleep 3; echo finished", "--wait", "1")
        self.assertLess(time.monotonic() - start, 2.5)
        self.assertEqual(proc.returncode, 75)
        header, *lines = proc.stdout.splitlines()
        self.assertRegex(header, rf"^\[running \ds · {w} · .+\]$")
        self.assertEqual(lines, [
            "started", f"hint: still running. `arc log {w}` to check, `arc kill {w}` to stop",
        ])

        final = self.wait_until_done(w)[-1]
        self.assertEqual(final.returncode, 0)
        self.assertEqual(final.stdout.splitlines()[1:], ["finished"])

    def test_log_shows_only_new_output(self):
        w = self.window()
        # Output already in run's own reply counts as seen, so count across all replies.
        replies = [arc("run", w, "echo a; sleep 1.5; echo b >&2", "--wait", "0")]
        replies += self.wait_until_done(w)
        seen = "".join(r.stdout.split("\n", 1)[1] for r in replies)
        self.assertEqual(seen.count("a\n"), 1)
        self.assertEqual(seen.count("--- stderr ---\nb\n"), 1)
        self.assertEqual(arc("log", w).stdout.splitlines()[1:], ["(no new output)"])

    def test_log_tail_shows_last_lines_again(self):
        w = self.window()
        self.run_in(w, "seq 1 100")
        self.assertEqual(arc("log", w, "--tail", "3").stdout.splitlines()[1:], ["98", "99", "100"])
        self.assertEqual(arc("log", w, "--tail", "1").stdout.splitlines()[1:], ["100"])

    def test_busy_window_refuses_new_commands(self):
        w = self.window()
        arc("run", w, "sleep 2", "--wait", "0")
        proc = arc("run", w, "echo should-not-run")
        self.assertEqual(proc.returncode, 255)
        self.assertIn(f"{w} is busy (sleep 2,", proc.stderr)
        self.assertIn("use another window", proc.stderr)
        self.wait_until_done(w)
        self.assertEqual(self.run_in(w, "echo free")[1], ["free"])

    def test_wait_zero_returns_at_once_and_survives_disconnect(self):
        w = self.window()
        start = time.monotonic()
        self.assertEqual(arc("run", w, "sleep 1; echo survived", "--wait", "0").returncode, 75)
        self.assertLess(time.monotonic() - start, 1)
        self.assertIn("survived", self.wait_until_done(w)[-1].stdout)

    def test_state_is_saved_when_a_detached_command_finishes(self):
        w = self.window()
        arc("run", w, "sleep 1; cd /tmp; export DONE=yes", "--wait", "0")
        self.wait_until_done(w)
        self.assertEqual(self.run_in(w, "pwd; echo $DONE")[1], ["/tmp", "yes"])

    def test_quick_commands_return_quickly(self):
        w = self.window()
        self.run_in(w, "true")
        start = time.monotonic()
        self.run_in(w, "true")
        self.assertLess(time.monotonic() - start, 1)

    def test_log_on_unused_window_does_not_create_it(self):
        name = f"t-{uuid.uuid4().hex[:8]}"
        proc = arc("log", f"{HOST}/{name}")
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(proc.stdout.splitlines()[1:], ["(no new output)"])
        self.assertIn("[idle ·", proc.stdout)
        exists = subprocess.run(["ssh", HOST, f"test -e ~/.arc/sessions/{name}"])
        self.assertNotEqual(exists.returncode, 0)

    def test_progress_bars_show_last_redraw(self):
        w = self.window()
        self.assertEqual(self.run_in(w, r"printf '10%%\r50%%\r100%%\n'")[1], ["100%"])


class TruncationTest(WindowTestCase):
    MARKER = re.compile(r"^\.\.\. \[[\d.]+ (?:bytes|KB|MB) omitted, ([\d,]+) lines\] \.\.\.$")

    def test_huge_output_is_cut_on_the_server(self):
        w = self.window()
        start = time.monotonic()
        proc = arc("run", w, "seq 1 10000000")  # 75 MB
        self.assertLess(time.monotonic() - start, 5)
        self.assertEqual(proc.returncode, 0)
        self.assertLess(len(proc.stdout), 40_000)
        _, *lines = proc.stdout.splitlines()
        self.assertEqual(lines[-1], f"hint: output truncated. In full: ~/.arc/sessions/{w.split('/')[1]}"
                                    "/stdout on the server, until this window runs another command")
        shown = lines[:-1]
        [marker_at] = [i for i, line in enumerate(shown) if self.MARKER.match(line)]
        head, tail = shown[:marker_at], shown[marker_at + 1:]
        omitted = int(self.MARKER.match(shown[marker_at]).group(1).replace(",", ""))
        # Whole lines only, and nothing lost: the counts add up to all 10M lines.
        self.assertEqual(head[:2], ["1", "2"])
        self.assertEqual(tail[-1], "10000000")
        self.assertEqual(int(head[-1]) + 1 + omitted, int(tail[0]))
        self.assertEqual(len(head) + omitted + len(tail), 10_000_000)

    def test_full_output_stays_on_the_server(self):
        w = self.window()
        arc("run", w, "seq 1 100000")
        path = f"~/.arc/sessions/{w.split('/')[1]}/stdout"
        _, lines = self.run_in(self.window(), f"sed -n 50000p {path}; wc -l < {path}")
        self.assertEqual(lines, ["50000", "100000"])

    def test_streams_are_cut_independently(self):
        w = self.window()
        proc = arc("run", w, "echo small; seq 1 100000 >&2")
        out, err = proc.stdout.split("--- stderr ---\n")
        self.assertEqual(out.splitlines()[1:], ["small"])
        self.assertTrue(any(self.MARKER.match(line) for line in err.splitlines()))
        self.assertIn("/stderr on the server", err)

    def test_log_tail_is_cut_too(self):
        w = self.window()
        arc("run", w, "seq 1 100000")
        lines = arc("log", w, "--tail", "50000").stdout.splitlines()
        self.assertEqual(lines[1], "50001")
        self.assertTrue(any(self.MARKER.match(line) for line in lines))

    def test_output_under_the_limit_is_untouched(self):
        w = self.window()
        proc = arc("run", w, "seq 1 5000")  # 24 KB
        self.assertEqual(proc.stdout.splitlines()[1:], [str(i) for i in range(1, 5001)])


def remote_processes(pattern: str) -> list:
    # "[s]leep" matches "sleep" but not this pgrep's own shell command line.
    pattern = f"[{pattern[0]}]{pattern[1:]}"
    proc = subprocess.run(["ssh", HOST, f"pgrep -f '{pattern}' || true"], capture_output=True, text=True)
    return proc.stdout.split()


class KillTest(WindowTestCase):
    def test_ctrl_c_stops_command_and_keeps_state(self):
        w = self.window()
        arc("run", w, "cd /tmp; sleep 41.25", "--wait", "0")
        start = time.monotonic()
        proc = arc("kill", w)
        self.assertLess(time.monotonic() - start, 3)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout.splitlines(), [f"[exit 130 · {w} · /tmp]", "stopped with SIGINT"])
        self.assertEqual(remote_processes("sleep 41.25"), [])
        self.assertEqual(self.run_in(w, "pwd")[1], ["/tmp"])

    def test_python_gets_keyboard_interrupt(self):
        w = self.window()
        script = "import time\ntry:\n    time.sleep(30)\nexcept KeyboardInterrupt:\n    print('saving checkpoint')\n    raise\n"
        arc("run", w, f"python3 -c \"{script}\"", "--wait", "1")
        proc = arc("kill", w)
        self.assertIn("saving checkpoint", proc.stdout)
        self.assertIn("KeyboardInterrupt", proc.stdout)

    def test_escalates_to_sigkill(self):
        w = self.window()
        arc("run", w, "trap '' INT TERM; sleep 42.25", "--wait", "0")
        proc = arc("kill", w)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.stdout.splitlines()[0][:9], "[stopped ")
        self.assertIn("stopped with SIGKILL", proc.stdout)
        self.assertEqual(remote_processes("sleep 42.25"), [])

    def test_stops_child_processes_too(self):
        w = self.window()
        arc("run", w, "sleep 43.25 & sleep 44.25; wait", "--wait", "0")
        self.assertEqual(arc("kill", w).returncode, 0)
        self.assertEqual(remote_processes("sleep 4[34].25"), [])

    def test_nothing_running(self):
        w = self.window()
        self.run_in(w, "true")
        proc = arc("kill", w)
        self.assertEqual(proc.returncode, 0)
        self.assertEqual(proc.stdout.splitlines()[1:], ["(nothing was running)"])

    def test_waiting_run_returns_when_killed(self):
        w = self.window()
        waiting = subprocess.Popen(
            [sys.executable, "-m", "arc", "run", w, "sleep 45.25", "--wait", "20"],
            cwd=REPO, stdout=subprocess.PIPE, text=True,
        )
        time.sleep(1)
        arc("kill", w)
        out, _ = waiting.communicate(timeout=5)
        self.assertEqual(waiting.returncode, 130)
        self.assertTrue(out.startswith(f"[exit 130 · {w} ·"), out)


class SessionsAndCloseTest(WindowTestCase):
    def sessions(self) -> dict:
        proc = arc("sessions", HOST)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        return {line.split()[0]: line for line in proc.stdout.splitlines()[1:]}

    def test_sessions_lists_windows_with_status(self):
        idle, busy = self.window(), self.window()
        self.run_in(idle, "cd /tmp")
        arc("run", busy, "sleep 2", "--wait", "0")
        listing = self.sessions()
        self.assertRegex(listing[idle.split("/")[1]], r"exit 0\s+/tmp\s+cd /tmp$")
        self.assertRegex(listing[busy.split("/")[1]], r"running \ds\s+/\S+\s+sleep 2$")
        self.wait_until_done(busy)

    def test_close_deletes_window(self):
        w = self.window()
        self.run_in(w, "cd /tmp; export GONE=1")
        proc = arc("close", w)
        self.assertEqual((proc.returncode, proc.stdout), (0, f"closed {w}\n"))
        self.assertNotIn(w.split("/")[1], self.sessions())
        # Using the name again starts a fresh window.
        self.assertEqual(self.run_in(w, "echo ${GONE-unset}")[1], ["unset"])
        self.assertNotEqual(self.run_in(w, "pwd")[1], ["/tmp"])

    def test_close_refuses_running_window_unless_forced(self):
        w = self.window()
        arc("run", w, "sleep 46.25", "--wait", "0")
        proc = arc("close", w)
        self.assertEqual(proc.returncode, 255)
        self.assertIn("--force", proc.stderr)
        self.assertEqual(arc("close", w, "--force").stdout, f"closed {w}\n")
        self.assertEqual(remote_processes("sleep 46.25"), [])

    def test_close_missing_window(self):
        proc = arc("close", f"{HOST}/t-never-existed")
        self.assertEqual(proc.returncode, 0)
        self.assertIn("does not exist", proc.stdout)


class ErrorTest(unittest.TestCase):
    def test_bad_window_name(self):
        proc = arc("run", "pdvm/bad name", "true")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("invalid window name", proc.stderr)

    def test_missing_arguments(self):
        self.assertEqual(arc("run", "pdvm").returncode, 255)

    def test_sessions_takes_a_host(self):
        proc = arc("sessions", "pdvm/main")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("arc sessions pdvm", proc.stderr)

    def test_bad_wait(self):
        proc = arc("run", "pdvm", "true", "--wait", "soon")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("invalid duration", proc.stderr)

    def test_unknown_host(self):
        proc = arc("run", "no-such-host.invalid", "true")
        self.assertEqual(proc.returncode, 255)
        self.assertIn("cannot run on no-such-host.invalid", proc.stderr)


if __name__ == "__main__":
    unittest.main()
