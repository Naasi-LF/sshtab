package main

import (
	"testing"
)

func TestFormatDuration(t *testing.T) {
	for seconds, want := range map[int]string{0: "0s", 59: "59s", 60: "1m00s", 725: "12m05s", 3900: "1h05m"} {
		assertEqual(t, formatDuration(seconds), want)
	}
}

func TestRenderStdoutAndStderr(t *testing.T) {
	r := result{status: "exit", code: 2, cwd: "/home/u", stdout: "hi", stderr: "oops\n"}
	assertEqual(t, render(r, "pdvm/main", ""), "[exit 2 · pdvm/main · /home/u]\nhi\n--- stderr ---\noops\n")
}

func TestRenderNoOutput(t *testing.T) {
	r := result{status: "exit", cwd: "/home/u"}
	assertEqual(t, render(r, "pdvm/main", ""), "[exit 0 · pdvm/main · /home/u]\n")
	assertEqual(t, render(r, "pdvm/main", "(no new output)"), "[exit 0 · pdvm/main · /home/u]\n(no new output)\n")
}

func TestRenderRunningHasHint(t *testing.T) {
	r := result{status: "running", cwd: "/data", stdout: "epoch 1\n", elapsed: 95}
	assertEqual(t, render(r, "pdvm/train", ""), "[running 1m35s · pdvm/train · /data]\nepoch 1\n"+
		"hint: still running. `sshtab log pdvm/train` to check, `sshtab kill pdvm/train` to stop\n")
}

func TestRenderTruncationHintNamesFiles(t *testing.T) {
	r := result{status: "exit", cwd: "/", stdout: "a", stderr: "b", truncated: []string{"stdout", "stderr"}}
	assertContains(t, render(r, "pdvm/train", ""), "hint: output truncated. In full: "+
		"~/.sshtab/sessions/train/stdout and ~/.sshtab/sessions/train/stderr on the server")
}

func TestCarriageReturnsShowLastRedraw(t *testing.T) {
	r := result{status: "exit", cwd: "/", stderr: " 10%|#  \r 50%|### \r100%|####\nok\r\n"}
	assertContains(t, render(r, "h/main", ""), "--- stderr ---\n100%|####\nok\n")
}

func TestWindowsTableAlignsWideCharacters(t *testing.T) {
	windows := []window{
		{"main", result{status: "exit", cwd: "/home/u", elapsed: 400, cmd: "ls"}},
		{"train", result{status: "running", cwd: "/data/项目", elapsed: 95, cmd: "python train.py"}},
		{"old", result{status: "stopped", cwd: "/tmp", elapsed: 9}},
	}
	assertEqual(t, renderWindows(windows), ""+
		"WINDOW  STATUS         DIR         COMMAND\n"+
		"main    exit 0         /home/u     ls\n"+
		"train   running 1m35s  /data/项目  python train.py\n"+
		"old     stopped        /tmp\n")
}

func TestNoWindows(t *testing.T) {
	assertEqual(t, renderWindows(nil), "(no windows)\n")
}

func TestDisplayWidth(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "项目": 4, "a测b": 4, "ｆｕｌｌ": 8, "": 0} {
		assertEqual(t, displayWidth(s), want)
	}
}
