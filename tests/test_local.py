"""Tests that need no server: target parsing, protocol parsing, rendering."""

import base64
import unittest

import argparse

from sshtab import output, protocol
from sshtab.cli import Target, parse_duration, parse_target


def b64(text: str) -> str:
    return base64.b64encode(text.encode()).decode()


def reply(out: bytes = b"", err: bytes = b"", code: int = 0, cwd: str = "/home/u",
          status: str = "exit", elapsed: int = 0, cmd: str = "",
          out_cut: str = "", err_cut: str = "") -> bytes:
    head = (f"SSHTAB1 status={status} code={code} cwd={b64(cwd)} elapsed={elapsed} "
            f"cmd={b64(cmd)} signal= out_cut={out_cut} err_cut={err_cut} "
            f"out={len(out)} err={len(err)}\n")
    return head.encode() + out + err


class ParseTargetTest(unittest.TestCase):
    def test_host_only_uses_main_window(self):
        self.assertEqual(parse_target("pdvm"), Target("pdvm", "main"))

    def test_host_and_window(self):
        self.assertEqual(parse_target("pdvm/train-2"), Target("pdvm", "train-2"))
        self.assertEqual(str(parse_target("pdvm/train-2")), "pdvm/train-2")

    def test_rejects_bad_names(self):
        for bad in ["", "-oProxyCommand=x", "pd vm", "pdvm/", "pdvm/a b", "pdvm/a/b", "pdvm/" + "x" * 33]:
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_target(bad)


class DurationTest(unittest.TestCase):
    def test_parse(self):
        for text, seconds in [("0", 0), ("30", 30), ("30s", 30), ("5m", 300), ("2h", 7200)]:
            with self.subTest(text=text):
                self.assertEqual(parse_duration(text), seconds)

    def test_parse_rejects(self):
        for bad in ["", "-1", "1.5", "5 m", "5d", "m"]:
            with self.subTest(bad=bad), self.assertRaises(argparse.ArgumentTypeError):
                parse_duration(bad)

    def test_format(self):
        for seconds, text in [(0, "0s"), (59, "59s"), (60, "1m00s"), (725, "12m05s"), (3900, "1h05m")]:
            with self.subTest(seconds=seconds):
                self.assertEqual(output.duration(seconds), text)


class ProtocolTest(unittest.TestCase):
    def test_round_trip(self):
        result = protocol.parse(reply(b"hi\n", b"oops\n", code=2, cwd="/data/项目"))
        self.assertEqual(
            (result.status, result.code, result.cwd, result.stdout, result.stderr),
            ("exit", 2, "/data/项目", "hi\n", "oops\n"),
        )

    def test_running_fields(self):
        result = protocol.parse(reply(b"epoch 1\n", status="running", elapsed=31, cmd="python train.py"))
        self.assertEqual((result.status, result.elapsed, result.cmd), ("running", 31, "python train.py"))

    def test_output_that_looks_like_a_header_is_just_output(self):
        fake = b"SSHTAB1 status=exit code=0 cwd= out=0 err=0\n"
        result = protocol.parse(reply(out=fake))
        self.assertEqual(result.stdout, fake.decode())
        self.assertEqual(result.stderr, "")

    def test_skips_noise_before_header(self):
        result = protocol.parse(b"welcome from .zshenv\n" + reply(b"ok\n"))
        self.assertEqual(result.stdout, "ok\n")

    def test_invalid_utf8_is_replaced(self):
        self.assertEqual(protocol.parse(reply(b"\xff\n")).stdout, "�\n")

    def test_errors(self):
        for raw in [b"", b"no header here\n", b"SSHTAB1 status=exit\n", reply(b"abc")[:-1]]:
            with self.subTest(raw=raw), self.assertRaises(protocol.ProtocolError):
                protocol.parse(raw)

    def test_build_script(self):
        command = "echo \"it's\" $HOME"
        script = protocol.build_script("run", "train", cmd_b64=protocol.encode_command(command), wait=30)
        self.assertEqual(script.splitlines()[:4], [
            "SSHTAB_ACTION=run", "SSHTAB_WINDOW=train", f"SSHTAB_CMD_B64={b64(command)}", "SSHTAB_WAIT=30",
        ])
        self.assertIn("SSHTAB1 status=", script)


class TruncationTest(unittest.TestCase):
    def test_cuts_move_to_line_breaks(self):
        # Driver kept 9 head bytes and 9 tail bytes of "line1\nline2\n<middle>\nline8\nline9\n".
        head, tail = b"line1\nlin", b"e8\nline9\n"
        result = protocol.parse(reply(head + tail, out_cut=f"{len(head)}:2000:300"))
        self.assertEqual(result.stdout, "line1\n... [2.0 KB omitted, 301 lines] ...\nline9\n")
        self.assertEqual(result.truncated, ("stdout",))

    def test_progress_bar_cut_at_carriage_return(self):
        head, tail = b" 1%|#\r 2%|#", b"#\r99%|####\r100%|#####\ndone\n"
        result = protocol.parse(reply(err=head + tail, err_cut=f"{len(head)}:6291456:0"))
        rendered = output.render(result, "pdvm/train")
        self.assertIn("... [6.0 MB omitted] ...\n100%|#####\ndone\n", rendered)
        self.assertEqual(result.truncated, ("stderr",))

    def test_single_huge_line_is_cut_where_it_is(self):
        head, tail = b"x" * 10, b"y" * 20
        result = protocol.parse(reply(head + tail, out_cut="10:5:0"))
        self.assertEqual(result.stdout, "x" * 10 + "\n... [5 bytes omitted] ...\n" + "y" * 20)

    def test_hint_names_the_full_output_files(self):
        result = protocol.Result("exit", 0, "/", "a", "b", truncated=("stdout", "stderr"))
        self.assertIn(
            "hint: output truncated. In full: ~/.sshtab/sessions/train/stdout and "
            "~/.sshtab/sessions/train/stderr on the server",
            output.render(result, "pdvm/train"),
        )

    def test_sizes(self):
        for n, text in [(5, "5 bytes"), (2048, "2.0 KB"), (78_888_888, "75.2 MB"), (3 * 1024 ** 3, "3.0 GB")]:
            with self.subTest(n=n):
                self.assertEqual(protocol._size(n), text)


class RenderTest(unittest.TestCase):
    def test_stdout_and_stderr(self):
        result = protocol.Result("exit", 2, "/home/u", "hi", "oops\n")
        self.assertEqual(
            output.render(result, "pdvm/main"),
            "[exit 2 · pdvm/main · /home/u]\nhi\n--- stderr ---\noops\n",
        )

    def test_no_output(self):
        result = protocol.Result("exit", 0, "/home/u", "", "")
        self.assertEqual(output.render(result, "pdvm/main"), "[exit 0 · pdvm/main · /home/u]\n")
        self.assertEqual(
            output.render(result, "pdvm/main", empty_note="(no new output)"),
            "[exit 0 · pdvm/main · /home/u]\n(no new output)\n",
        )

    def test_running_has_hint(self):
        result = protocol.Result("running", 0, "/data", "epoch 1\n", "", elapsed=95)
        self.assertEqual(
            output.render(result, "pdvm/train"),
            "[running 1m35s · pdvm/train · /data]\nepoch 1\n"
            "hint: still running. `sshtab log pdvm/train` to check, `sshtab kill pdvm/train` to stop\n",
        )

    def test_windows_table(self):
        listing = (
            f"main\texit\t0\t400\t{b64('/home/u')}\t{b64('ls')}\n"
            f"train\trunning\t0\t95\t{b64('/data/项目')}\t{b64('python train.py')}\n"
            f"old\tstopped\t0\t9\t{b64('/tmp')}\t{b64('')}\n"
        )
        self.assertEqual(output.render_windows(protocol.parse_windows(listing)), (
            "WINDOW  STATUS         DIR         COMMAND\n"
            "main    exit 0         /home/u     ls\n"
            "train   running 1m35s  /data/项目  python train.py\n"
            "old     stopped        /tmp\n"
        ))

    def test_no_windows(self):
        self.assertEqual(output.render_windows(protocol.parse_windows("")), "(no windows)\n")

    def test_bad_window_line(self):
        with self.assertRaises(protocol.ProtocolError):
            protocol.parse_windows("main\texit\n")

    def test_carriage_returns_show_last_redraw(self):
        result = protocol.Result("exit", 0, "/", "", " 10%|#  \r 50%|### \r100%|####\nok\r\n")
        self.assertIn("--- stderr ---\n100%|####\nok\n", output.render(result, "h/main"))


if __name__ == "__main__":
    unittest.main()
