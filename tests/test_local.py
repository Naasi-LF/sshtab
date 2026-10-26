"""Tests that need no server: target parsing, protocol parsing, rendering."""

import base64
import unittest

from arc import output, protocol
from arc.cli import Target, parse_target


def reply(out: bytes = b"", err: bytes = b"", code: int = 0, cwd: str = "/home/u") -> bytes:
    cwd64 = base64.b64encode(cwd.encode()).decode()
    head = f"ARC1 status=exit code={code} cwd={cwd64} out={len(out)} err={len(err)}\n"
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


class ProtocolTest(unittest.TestCase):
    def test_round_trip(self):
        result = protocol.parse(reply(b"hi\n", b"oops\n", code=2, cwd="/data/项目"))
        self.assertEqual(
            (result.status, result.code, result.cwd, result.stdout, result.stderr),
            ("exit", 2, "/data/项目", "hi\n", "oops\n"),
        )

    def test_output_that_looks_like_a_header_is_just_output(self):
        fake = b"ARC1 status=exit code=0 cwd= out=0 err=0\n"
        result = protocol.parse(reply(out=fake))
        self.assertEqual(result.stdout, fake.decode())
        self.assertEqual(result.stderr, "")

    def test_skips_noise_before_header(self):
        result = protocol.parse(b"welcome from .zshenv\n" + reply(b"ok\n"))
        self.assertEqual(result.stdout, "ok\n")

    def test_invalid_utf8_is_replaced(self):
        self.assertEqual(protocol.parse(reply(b"\xff\n")).stdout, "�\n")

    def test_errors(self):
        for raw in [b"", b"no header here\n", b"ARC1 status=exit\n", reply(b"abc")[:-1]]:
            with self.subTest(raw=raw), self.assertRaises(protocol.ProtocolError):
                protocol.parse(raw)

    def test_build_script_encodes_command(self):
        script = protocol.build_script("echo \"it's\" $HOME", "train")
        first, second = script.splitlines()[:2]
        self.assertEqual(base64.b64decode(first.split("=", 1)[1]).decode(), "echo \"it's\" $HOME")
        self.assertEqual(second, "ARC_WINDOW=train")
        self.assertIn("ARC1 status=exit", script)


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


if __name__ == "__main__":
    unittest.main()
