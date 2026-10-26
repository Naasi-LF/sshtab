package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeServer answers every call with reply and records the scripts it got.
type fakeServer struct {
	reply   sshReply
	err     error
	host    string
	scripts []string
}

func (f *fakeServer) call(host, script string) (sshReply, error) {
	f.host = host
	f.scripts = append(f.scripts, script)
	return f.reply, f.err
}

type cliRun struct {
	code           int
	stdout, stderr string
	server         *fakeServer
}

func runCLI(t *testing.T, server *fakeServer, stdin string, args ...string) cliRun {
	t.Helper()
	if server == nil {
		server = &fakeServer{reply: sshReply{stdout: fakeReply(replyOpts{})}}
	}
	var stdout, stderr bytes.Buffer
	c := &cli{stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr, call: server.call}
	code := c.main(args)
	return cliRun{code, stdout.String(), stderr.String(), server}
}

func replying(o replyOpts) *fakeServer {
	return &fakeServer{reply: sshReply{stdout: fakeReply(o)}}
}

// scriptParams returns the SSHTAB_* assignments that start a driver script.
func scriptParams(script string) map[string]string {
	params := map[string]string{}
	for _, line := range strings.Split(script, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "SSHTAB_") {
			break
		}
		params[key] = value
	}
	return params
}

func TestParseTarget(t *testing.T) {
	for text, want := range map[string]target{"pdvm": {"pdvm", "main"}, "pdvm/train-2": {"pdvm", "train-2"}} {
		got, err := parseTarget(text)
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, got, want)
	}
	assertEqual(t, target{"pdvm", "train-2"}.String(), "pdvm/train-2")
}

func TestParseTargetRejectsBadNames(t *testing.T) {
	for _, bad := range []string{"", "-oProxyCommand=x", "pd vm", "pdvm/", "pdvm/a b", "pdvm/a/b", "pdvm/" + strings.Repeat("x", 33)} {
		if _, err := parseTarget(bad); err == nil {
			t.Errorf("parseTarget(%q) did not fail", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for text, want := range map[string]int{"0": 0, "30": 30, "30s": 30, "5m": 300, "2h": 7200} {
		got, err := parseDuration(text)
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, got, want)
	}
	for _, bad := range []string{"", "-1", "1.5", "5 m", "5d", "m", "99999999999999999999"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) did not fail", bad)
		}
	}
}

func TestRunSendsCommandAndWait(t *testing.T) {
	command := `echo "it's" $HOME`
	for _, args := range [][]string{
		{"run", "pdvm/train", command, "--wait", "5m"},
		{"run", "--wait=5m", "pdvm/train", command},
	} {
		r := runCLI(t, nil, "", args...)
		assertEqual(t, r.code, 0)
		assertEqual(t, r.server.host, "pdvm")
		params := scriptParams(r.server.scripts[0])
		assertEqual(t, params, map[string]string{
			"SSHTAB_ACTION": "run", "SSHTAB_WINDOW": "train", "SSHTAB_CMD_B64": b64(command), "SSHTAB_WAIT": "300",
		})
	}
}

func TestRunDefaults(t *testing.T) {
	r := runCLI(t, nil, "", "run", "pdvm", "ls")
	params := scriptParams(r.server.scripts[0])
	assertEqual(t, []string{params["SSHTAB_WINDOW"], params["SSHTAB_WAIT"]}, []string{"main", "30"})
	assertEqual(t, r.stdout, "[exit 0 · pdvm/main · /home/u]\n")
}

func TestRunCommandFromStdin(t *testing.T) {
	script := "for i in 1 2; do\n  echo $i\ndone\n"
	r := runCLI(t, nil, script, "run", "pdvm", "-")
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_CMD_B64"], b64(script))
}

func TestDoubleDashAllowsCommandsStartingWithDash(t *testing.T) {
	r := runCLI(t, nil, "", "run", "pdvm", "--", "--version")
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_CMD_B64"], b64("--version"))
}

func TestExitCodes(t *testing.T) {
	for _, c := range []struct {
		reply replyOpts
		want  int
	}{
		{replyOpts{code: 3}, 3},
		{replyOpts{status: "running"}, exitRunning},
		{replyOpts{status: "stopped"}, exitStopped},
		{replyOpts{status: "idle"}, 0},
	} {
		assertEqual(t, runCLI(t, replying(c.reply), "", "run", "pdvm", "x").code, c.want)
		assertEqual(t, runCLI(t, replying(c.reply), "", "log", "pdvm").code, c.want)
	}
}

func TestBusyWindow(t *testing.T) {
	r := runCLI(t, replying(replyOpts{status: "busy", cmd: "sleep 30", elapsed: 75}), "", "run", "pdvm/train", "ls")
	assertEqual(t, r.code, exitError)
	assertEqual(t, r.stderr, "error: pdvm/train is busy (sleep 30, 1m15s)\n"+
		"hint: use another window, e.g. `sshtab run pdvm/main '...'`, or `sshtab kill pdvm/train` to stop it\n")
	r = runCLI(t, replying(replyOpts{status: "busy"}), "", "run", "pdvm", "ls")
	assertContains(t, r.stderr, "`sshtab run pdvm/main2 '...'`")
}

func TestLogTail(t *testing.T) {
	r := runCLI(t, nil, "", "log", "pdvm/a", "--tail", "50")
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_TAIL"], "50")
	r = runCLI(t, nil, "", "log", "pdvm/a")
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_TAIL"], "")
	assertEqual(t, r.stdout, "[exit 0 · pdvm/a · /home/u]\n(no new output)\n")
	assertEqual(t, runCLI(t, nil, "", "log", "pdvm", "--tail", "x").code, exitError)
}

func TestKill(t *testing.T) {
	r := runCLI(t, replying(replyOpts{code: 130, signal: "INT", err: "KeyboardInterrupt\n"}), "", "kill", "pdvm/t")
	assertEqual(t, r.code, 0)
	assertEqual(t, r.stdout, "[exit 130 · pdvm/t · /home/u]\n--- stderr ---\nKeyboardInterrupt\nstopped with SIGINT\n")
	r = runCLI(t, nil, "", "kill", "pdvm/t")
	assertEqual(t, r.stdout, "[exit 0 · pdvm/t · /home/u]\n(nothing was running)\n")
	r = runCLI(t, replying(replyOpts{status: "running", signal: "KILL"}), "", "kill", "pdvm/t")
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "could not stop pdvm/t")
}

func TestSessions(t *testing.T) {
	listing := "main\texit\t0\t1\t" + b64("/home/u") + "\t" + b64("ls") + "\n"
	r := runCLI(t, replying(replyOpts{status: "list", out: listing}), "", "sessions", "pdvm")
	assertEqual(t, r.code, 0)
	assertEqual(t, r.stdout, "WINDOW  STATUS  DIR      COMMAND\nmain    exit 0  /home/u  ls\n")
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_ACTION"], "list")

	r = runCLI(t, nil, "", "sessions", "pdvm/main")
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "try `sshtab sessions pdvm`")
}

func TestClose(t *testing.T) {
	r := runCLI(t, replying(replyOpts{status: "closed"}), "", "close", "pdvm/a", "--force")
	assertEqual(t, []any{r.code, r.stdout}, []any{0, "closed pdvm/a\n"})
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_FORCE"], "1")

	r = runCLI(t, replying(replyOpts{status: "missing"}), "", "close", "pdvm/a")
	assertEqual(t, []any{r.code, r.stdout}, []any{0, "pdvm/a does not exist (nothing to close)\n"})
	assertEqual(t, scriptParams(r.server.scripts[0])["SSHTAB_FORCE"], "")

	r = runCLI(t, replying(replyOpts{status: "busy", cmd: "sleep 9"}), "", "close", "pdvm/a")
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "hint: `sshtab kill pdvm/a` first, or `sshtab close pdvm/a --force`")
}

func TestArgumentErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "usage: sshtab <command>"},
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"run", "pdvm"}, "missing argument: command"},
		{[]string{"run", "pdvm", "a", "b"}, `unexpected argument: "b"`},
		{[]string{"run", "pdvm", "true", "--wait", "soon"}, "invalid duration"},
		{[]string{"run", "pdvm", "true", "--wait"}, "option --wait needs a value"},
		{[]string{"run", "pdvm", "true", "-x"}, "unrecognized option: -x"},
		{[]string{"close", "pdvm", "--force=yes"}, "option --force takes no value"},
		{[]string{"run", "pdvm/bad name", "true"}, "invalid window name"},
		{[]string{"run", "-oProxyCommand=x", "true"}, "unrecognized option"},
	} {
		r := runCLI(t, nil, "", c.args...)
		if r.code != exitError || !strings.Contains(r.stderr, c.want) {
			t.Errorf("sshtab %q: code %d, stderr %q; want 255 and %q", c.args, r.code, r.stderr, c.want)
		}
		if len(r.server.scripts) != 0 {
			t.Errorf("sshtab %q contacted the server", c.args)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	r := runCLI(t, nil, "", "--help")
	assertEqual(t, r.code, 0)
	for _, name := range []string{"run", "log", "kill", "sessions", "close"} {
		assertContains(t, r.stdout, "  "+name+" ")
	}
	r = runCLI(t, nil, "", "run", "--help")
	assertEqual(t, r.code, 0)
	assertContains(t, r.stdout, "usage: sshtab run host[/window] command [--wait DURATION]")
	assertEqual(t, runCLI(t, nil, "", "--version").stdout, "sshtab dev\n")
}

func TestServerProblems(t *testing.T) {
	r := runCLI(t, &fakeServer{reply: sshReply{stderr: []byte("ssh: Could not resolve hostname nope\n")}}, "", "run", "nope", "ls")
	assertEqual(t, r.code, exitError)
	assertEqual(t, r.stderr, "error: cannot run on nope: ssh: Could not resolve hostname nope\n")

	r = runCLI(t, &fakeServer{}, "", "run", "pdvm", "ls")
	assertContains(t, r.stderr, "cannot run on pdvm: no response from remote driver")

	r = runCLI(t, &fakeServer{err: errors.New(`exec: "ssh": executable file not found in $PATH`)}, "", "run", "pdvm", "ls")
	assertContains(t, r.stderr, "error: cannot run ssh: exec: \"ssh\"")
}
