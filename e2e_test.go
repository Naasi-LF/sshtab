package main

// End-to-end tests: build the real binary and run it against a real server,
// `pdvm` unless SSHTAB_TEST_HOST says otherwise. Skipped with -short or when
// the host is unreachable.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	testHost = envOr("SSHTAB_TEST_HOST", "pdvm")
	binPath  string // the built sshtab, when end-to-end tests can run
	skipE2E  string // why they cannot, otherwise
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(func() int {
		if testing.Short() {
			skipE2E = "-short"
			return m.Run()
		}
		probe := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", testHost, "true")
		if err := probe.Run(); err != nil {
			skipE2E = fmt.Sprintf("%s is not reachable over ssh: %v", testHost, err)
			return m.Run()
		}
		dir, err := os.MkdirTemp("", "sshtab-e2e")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		binPath = filepath.Join(dir, "sshtab")
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}())
}

type proc struct {
	code           int
	stdout, stderr string
}

// lines splits stdout into lines, like Python's splitlines.
func (p proc) lines() []string { return splitLines(p.stdout) }

func sshtab(t *testing.T, args ...string) proc { return sshtabIn(t, "", args...) }

func sshtabIn(t *testing.T, stdin string, args ...string) proc {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binPath, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("sshtab %q: %v", args, err)
	}
	return proc{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}

func onServer(t *testing.T, command string) string {
	t.Helper()
	out, err := exec.Command("ssh", testHost, command).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s %q: %v\n%s", testHost, command, err, out)
	}
	return string(out)
}

// remoteProcesses lists pids whose command line matches pattern.
func remoteProcesses(t *testing.T, pattern string) []string {
	t.Helper()
	// "[s]leep" matches "sleep" but not this pgrep's own shell command line.
	pattern = "[" + pattern[:1] + "]" + pattern[1:]
	return strings.Fields(onServer(t, fmt.Sprintf("pgrep -f '%s' || true", pattern)))
}

func randomName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// e2e gives each test fresh, uniquely named windows and temp paths on the
// server, deleted when the test ends.
type e2e struct {
	t       *testing.T
	cleanup []string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	if skipE2E != "" {
		t.Skip(skipE2E)
	}
	e := &e2e{t: t}
	t.Cleanup(func() {
		if len(e.cleanup) == 0 {
			return
		}
		// Explicit paths, no globs: the login shell may be zsh, which aborts on no match.
		quoted := make([]string, len(e.cleanup))
		for i, p := range e.cleanup {
			if strings.HasPrefix(p, "/") {
				p = "'" + p + "'"
			}
			quoted[i] = p
		}
		onServer(t, "rm -rf "+strings.Join(quoted, " "))
	})
	return e
}

func (e *e2e) window() string {
	name := "t-" + randomName()
	e.cleanup = append(e.cleanup, "~/.sshtab/sessions/"+name)
	return testHost + "/" + name
}

func (e *e2e) tmpPath(suffix string) string {
	p := "/tmp/sshtab-test-" + randomName() + suffix
	e.cleanup = append(e.cleanup, p)
	return p
}

// runIn runs command in target, fails the test on a nonzero exit, and returns
// the header and the output lines.
func (e *e2e) runIn(target, command string) (string, []string) {
	e.t.Helper()
	p := sshtab(e.t, "run", target, command)
	if p.code != 0 {
		e.t.Fatalf("sshtab run %s %q: exit %d\n%s%s", target, command, p.code, p.stdout, p.stderr)
	}
	lines := p.lines()
	return lines[0], lines[1:]
}

// waitUntilDone polls `sshtab log` until the command finishes and returns every reply.
func (e *e2e) waitUntilDone(target string) []proc {
	e.t.Helper()
	var replies []proc
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		p := sshtab(e.t, "log", target)
		replies = append(replies, p)
		if p.code != exitRunning {
			return replies
		}
	}
	e.t.Fatalf("%s still running after 15s", target)
	return nil
}

func windowName(target string) string {
	_, name, _ := strings.Cut(target, "/")
	return name
}

func assertMatch(t *testing.T, s, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(s) {
		t.Fatalf("%q does not match %s", s, pattern)
	}
}

func assertFaster(t *testing.T, start time.Time, limit time.Duration) {
	t.Helper()
	if took := time.Since(start); took > limit {
		t.Fatalf("took %v, want under %v", took, limit)
	}
}

// --- run ---

func TestE2ESeparatesStdoutAndStderrAndKeepsExitCode(t *testing.T) {
	e := newE2E(t)
	p := sshtab(t, "run", e.window(), "echo hi; ls /nope")
	assertEqual(t, p.code, 2)
	header, rest, _ := strings.Cut(p.stdout, "\n")
	assertMatch(t, header, `^\[exit 2 · `+testHost+`/t-\w+ · /.+\]$`)
	out, err, _ := strings.Cut(rest, "--- stderr ---\n")
	assertEqual(t, out, "hi\n")
	assertContains(t, err, "/nope")
}

func TestE2EDefaultWindowIsMain(t *testing.T) {
	if skipE2E != "" {
		t.Skip(skipE2E)
	}
	assertMatch(t, sshtab(t, "run", testHost, "true").stdout, `^\[exit 0 · `+testHost+`/main · `)
}

func TestE2EExitCodePassthrough(t *testing.T) {
	assertEqual(t, sshtab(t, "run", newE2E(t).window(), "exit 7").code, 7)
}

func TestE2EQuotesAndDollarsNeedNoEscaping(t *testing.T) {
	e := newE2E(t)
	_, lines := e.runIn(e.window(), "echo \"it's\" '$HOME' \"$HOME\" `echo tick` | tr a-z A-Z")
	if !strings.HasPrefix(lines[0], "IT'S $HOME /HOME/") || !strings.HasSuffix(lines[0], " TICK") {
		t.Fatalf("got %q", lines[0])
	}
}

func TestE2EMultilineCommandFromStdin(t *testing.T) {
	e := newE2E(t)
	p := sshtabIn(t, "for i in 1 2 3; do\n  echo line $i\ndone\n", "run", e.window(), "-")
	assertEqual(t, p.lines()[1:], []string{"line 1", "line 2", "line 3"})
}

func TestE2EStdinIsClosedSoNothingHangs(t *testing.T) {
	e := newE2E(t)
	start := time.Now()
	p := sshtab(t, "run", e.window(), "cat; read x; echo done")
	assertFaster(t, start, 10*time.Second)
	assertEqual(t, p.lines()[1:], []string{"done"})
}

func TestE2ENonInteractiveEnvironment(t *testing.T) {
	e := newE2E(t)
	_, lines := e.runIn(e.window(), "echo $PAGER $GIT_PAGER $TERM $PYTHONUNBUFFERED")
	assertEqual(t, lines, []string{"cat cat dumb 1"})
}

func TestE2EBinaryOutputDoesNotCrash(t *testing.T) {
	e := newE2E(t)
	_, lines := e.runIn(e.window(), `printf '\377\000ok'`)
	assertContains(t, strings.Join(lines, "\n"), "ok")
}

// --- window state ---

func TestE2EWindowNameShowsInHeader(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	header, _ := e.runIn(w, "true")
	_, pwd := e.runIn(w, "pwd")
	assertEqual(t, header, "[exit 0 · "+w+" · "+pwd[0]+"]")
}

func TestE2ECdIsRemembered(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	header, _ := e.runIn(w, "cd /tmp")
	assertEqual(t, header, "[exit 0 · "+w+" · /tmp]")
	_, lines := e.runIn(w, "pwd")
	assertEqual(t, lines, []string{"/tmp"})
}

func TestE2EWindowsAreIndependent(t *testing.T) {
	e := newE2E(t)
	a, b := e.window(), e.window()
	e.runIn(a, "cd /tmp && export ONLY_A=1")
	_, lines := e.runIn(b, "pwd; echo ${ONLY_A-unset}")
	if lines[0] == "/tmp" || lines[1] != "unset" {
		t.Fatalf("window b sees window a's state: %q", lines)
	}
}

func TestE2ENewWindowStartsAtHome(t *testing.T) {
	e := newE2E(t)
	_, home := e.runIn(e.window(), `echo "$HOME"`)
	_, pwd := e.runIn(e.window(), "pwd")
	assertEqual(t, pwd, home)
}

// A plain `ssh host cmd` skips the login profiles, where hosts like AutoDL put
// conda and CUDA on PATH. A new window must see the PATH a login shell sees.
func TestE2ENewWindowReadsLoginProfiles(t *testing.T) {
	e := newE2E(t)
	login := onServer(t, `bash -l -c 'printf "%s\n" "$PATH"' </dev/null 2>/dev/null`)
	_, lines := e.runIn(e.window(), `printf "%s\n" "$PATH"`)
	assertEqual(t, lines, []string{strings.TrimSpace(login)})
}

func TestE2EVariablesAndFunctionsAreRemembered(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "plain=1; export EXPORTED=2; declare -a arr=(a 'b c'); greet() { echo hi $1; }")
	_, lines := e.runIn(w, `echo "$plain $EXPORTED ${arr[1]}"; greet bob; sh -c "echo \$EXPORTED"`)
	assertEqual(t, lines, []string{"1 2 b c", "hi bob", "2"})
}

func TestE2EUnsetIsRemembered(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "unset LANG; export LANG")
	_, lines := e.runIn(w, `echo "${LANG-unset}"`)
	assertEqual(t, lines, []string{"unset"})
}

func TestE2EStateIsSavedEvenWhenCommandExits(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	p := sshtab(t, "run", w, "cd /tmp && exit 3")
	assertEqual(t, p.code, 3)
	assertContains(t, p.lines()[0], "· /tmp]")
	_, lines := e.runIn(w, "pwd")
	assertEqual(t, lines, []string{"/tmp"})
}

func TestE2ECdDashReturnsToPreviousDirectory(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "cd /tmp")
	e.runIn(w, "cd /etc")
	_, lines := e.runIn(w, "cd - >/dev/null; pwd")
	assertEqual(t, lines, []string{"/tmp"})
}

func TestE2EDirectoryWithSpacesAndUnicode(t *testing.T) {
	e := newE2E(t)
	w, d := e.window(), e.tmpPath(" 测试")
	header, _ := e.runIn(w, fmt.Sprintf("mkdir -p '%s' && cd '%s'", d, d))
	if !strings.HasSuffix(header, "· "+d+"]") {
		t.Fatalf("header %q", header)
	}
	_, lines := e.runIn(w, "pwd")
	assertEqual(t, lines, []string{d})
}

func TestE2EDeletedDirectoryFallsBackToHome(t *testing.T) {
	e := newE2E(t)
	w, d := e.window(), e.tmpPath("")
	e.runIn(w, "mkdir -p "+d+" && cd "+d)
	e.runIn(e.window(), "rm -rf "+d)
	p := sshtab(t, "run", w, "pwd")
	assertContains(t, p.stdout, "no longer exists")
	if strings.Contains(p.lines()[0], d) {
		t.Fatalf("header still in the deleted directory: %q", p.lines()[0])
	}
}

func TestE2EForcedNonInteractiveSettingsWin(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "export PAGER=less")
	_, lines := e.runIn(w, "echo $PAGER")
	assertEqual(t, lines, []string{"cat"})
}

func TestE2EVenvActivateAndDeactivate(t *testing.T) {
	e := newE2E(t)
	w, venv := e.window(), e.tmpPath("-venv")
	e.runIn(w, "python3 -m venv --without-pip "+venv+" && source "+venv+"/bin/activate")
	_, lines := e.runIn(w, "echo $VIRTUAL_ENV; command -v python")
	assertEqual(t, lines, []string{venv, venv + "/bin/python"})
	e.runIn(w, "deactivate")
	_, lines = e.runIn(w, `echo "${VIRTUAL_ENV-none}"; echo "$PATH"`)
	assertEqual(t, lines[0], "none")
	if strings.Contains(lines[1], venv) {
		t.Fatalf("PATH still has the venv: %q", lines[1])
	}
}

// --- long commands ---

func TestE2EReturnsAfterWaitAndKeepsRunning(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	start := time.Now()
	p := sshtab(t, "run", w, "echo started; sleep 3; echo finished", "--wait", "1")
	assertFaster(t, start, 2500*time.Millisecond)
	assertEqual(t, p.code, exitRunning)
	lines := p.lines()
	assertMatch(t, lines[0], `^\[running \ds · `+w+` · .+\]$`)
	assertEqual(t, lines[1:], []string{
		"started", "hint: still running. `sshtab log " + w + "` to check, `sshtab kill " + w + "` to stop",
	})
	replies := e.waitUntilDone(w)
	final := replies[len(replies)-1]
	assertEqual(t, final.code, 0)
	assertEqual(t, final.lines()[1:], []string{"finished"})
}

func TestE2ELogShowsOnlyNewOutput(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	// Output already in run's own reply counts as seen, so count across all replies.
	replies := append([]proc{sshtab(t, "run", w, "echo a; sleep 1.5; echo b >&2", "--wait", "0")}, e.waitUntilDone(w)...)
	var seen strings.Builder
	for _, r := range replies {
		_, rest, _ := strings.Cut(r.stdout, "\n")
		seen.WriteString(rest)
	}
	assertEqual(t, strings.Count(seen.String(), "a\n"), 1)
	assertEqual(t, strings.Count(seen.String(), "--- stderr ---\nb\n"), 1)
	assertEqual(t, sshtab(t, "log", w).lines()[1:], []string{"(no new output)"})
}

func TestE2ELogTailShowsLastLinesAgain(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "seq 1 100")
	assertEqual(t, sshtab(t, "log", w, "--tail", "3").lines()[1:], []string{"98", "99", "100"})
	assertEqual(t, sshtab(t, "log", w, "--tail", "1").lines()[1:], []string{"100"})
}

func TestE2EBusyWindowRefusesNewCommands(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "sleep 2", "--wait", "0")
	p := sshtab(t, "run", w, "echo should-not-run")
	assertEqual(t, p.code, exitError)
	assertContains(t, p.stderr, w+" is busy (sleep 2,")
	assertContains(t, p.stderr, "use another window")
	e.waitUntilDone(w)
	_, lines := e.runIn(w, "echo free")
	assertEqual(t, lines, []string{"free"})
}

func TestE2EWaitZeroReturnsAtOnceAndSurvivesDisconnect(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	start := time.Now()
	assertEqual(t, sshtab(t, "run", w, "sleep 1; echo survived", "--wait", "0").code, exitRunning)
	assertFaster(t, start, time.Second)
	replies := e.waitUntilDone(w)
	assertContains(t, replies[len(replies)-1].stdout, "survived")
}

func TestE2EStateIsSavedWhenADetachedCommandFinishes(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "sleep 1; cd /tmp; export DONE=yes", "--wait", "0")
	e.waitUntilDone(w)
	_, lines := e.runIn(w, "pwd; echo $DONE")
	assertEqual(t, lines, []string{"/tmp", "yes"})
}

func TestE2EQuickCommandsReturnQuickly(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "true")
	start := time.Now()
	e.runIn(w, "true")
	assertFaster(t, start, time.Second)
}

func TestE2ELogOnUnusedWindowDoesNotCreateIt(t *testing.T) {
	newE2E(t)
	name := "t-" + randomName()
	p := sshtab(t, "log", testHost+"/"+name)
	assertEqual(t, p.code, 0)
	assertEqual(t, p.lines()[1:], []string{"(no new output)"})
	assertContains(t, p.stdout, "[idle ·")
	assertEqual(t, strings.TrimSpace(onServer(t, "test -e ~/.sshtab/sessions/"+name+" && echo yes || echo no")), "no")
}

func TestE2EProgressBarsShowLastRedraw(t *testing.T) {
	e := newE2E(t)
	_, lines := e.runIn(e.window(), `printf '10%%\r50%%\r100%%\n'`)
	assertEqual(t, lines, []string{"100%"})
}

// --- truncation ---

var markerRE = regexp.MustCompile(`^\.\.\. \[[\d.]+ (?:bytes|KB|MB) omitted, ([\d,]+) lines\] \.\.\.$`)

func markerIndex(lines []string) int {
	for i, line := range lines {
		if markerRE.MatchString(line) {
			return i
		}
	}
	return -1
}

func TestE2EHugeOutputIsCutOnTheServer(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	start := time.Now()
	p := sshtab(t, "run", w, "seq 1 10000000") // 75 MB
	assertFaster(t, start, 5*time.Second)
	assertEqual(t, p.code, 0)
	if len(p.stdout) > 40_000 {
		t.Fatalf("got %d bytes", len(p.stdout))
	}
	lines := p.lines()[1:]
	assertEqual(t, lines[len(lines)-1], "hint: output truncated. In full: ~/.sshtab/sessions/"+windowName(w)+
		"/stdout on the server, until this window runs another command")
	shown := lines[:len(lines)-1]
	at := markerIndex(shown)
	if at < 0 {
		t.Fatal("no truncation marker")
	}
	head, tail := shown[:at], shown[at+1:]
	omitted, _ := strconv.Atoi(strings.ReplaceAll(markerRE.FindStringSubmatch(shown[at])[1], ",", ""))
	// Whole lines only, and nothing lost: the counts add up to all 10M lines.
	assertEqual(t, head[:2], []string{"1", "2"})
	assertEqual(t, tail[len(tail)-1], "10000000")
	last, _ := strconv.Atoi(head[len(head)-1])
	first, _ := strconv.Atoi(tail[0])
	assertEqual(t, last+1+omitted, first)
	assertEqual(t, len(head)+omitted+len(tail), 10_000_000)
}

func TestE2EFullOutputStaysOnTheServer(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "seq 1 100000")
	path := "~/.sshtab/sessions/" + windowName(w) + "/stdout"
	_, lines := e.runIn(e.window(), "sed -n 50000p "+path+"; wc -l < "+path)
	assertEqual(t, lines, []string{"50000", "100000"})
}

func TestE2EStreamsAreCutIndependently(t *testing.T) {
	e := newE2E(t)
	p := sshtab(t, "run", e.window(), "echo small; seq 1 100000 >&2")
	out, err, _ := strings.Cut(p.stdout, "--- stderr ---\n")
	assertEqual(t, splitLines(out)[1:], []string{"small"})
	if markerIndex(splitLines(err)) < 0 {
		t.Fatal("stderr has no truncation marker")
	}
	assertContains(t, err, "/stderr on the server")
}

func TestE2ELogTailIsCutToo(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "seq 1 100000")
	lines := sshtab(t, "log", w, "--tail", "50000").lines()
	assertEqual(t, lines[1], "50001")
	if markerIndex(lines) < 0 {
		t.Fatal("no truncation marker")
	}
}

func TestE2EOutputUnderTheLimitIsUntouched(t *testing.T) {
	e := newE2E(t)
	_, lines := e.runIn(e.window(), "seq 1 5000") // 24 KB
	want := make([]string, 5000)
	for i := range want {
		want[i] = strconv.Itoa(i + 1)
	}
	assertEqual(t, lines, want)
}

// --- kill ---

func TestE2ECtrlCStopsCommandAndKeepsState(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "cd /tmp; sleep 41.25", "--wait", "0")
	start := time.Now()
	p := sshtab(t, "kill", w)
	assertFaster(t, start, 3*time.Second)
	assertEqual(t, p.code, 0)
	assertEqual(t, p.lines(), []string{"[exit 130 · " + w + " · /tmp]", "stopped with SIGINT"})
	assertEqual(t, len(remoteProcesses(t, "sleep 41.25")), 0)
	_, lines := e.runIn(w, "pwd")
	assertEqual(t, lines, []string{"/tmp"})
}

func TestE2EPythonGetsKeyboardInterrupt(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	script := "import time\ntry:\n    time.sleep(30)\nexcept KeyboardInterrupt:\n    print('saving checkpoint')\n    raise\n"
	sshtab(t, "run", w, `python3 -c "`+script+`"`, "--wait", "1")
	p := sshtab(t, "kill", w)
	assertContains(t, p.stdout, "saving checkpoint")
	assertContains(t, p.stdout, "KeyboardInterrupt")
}

func TestE2EEscalatesToSIGKILL(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "trap '' INT TERM; sleep 42.25", "--wait", "0")
	p := sshtab(t, "kill", w)
	assertEqual(t, p.code, 0)
	assertMatch(t, p.lines()[0], `^\[stopped `)
	assertContains(t, p.stdout, "stopped with SIGKILL")
	assertEqual(t, len(remoteProcesses(t, "sleep 42.25")), 0)
}

func TestE2EStopsChildProcessesToo(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "sleep 43.25 & sleep 44.25; wait", "--wait", "0")
	assertEqual(t, sshtab(t, "kill", w).code, 0)
	assertEqual(t, len(remoteProcesses(t, "sleep 4[34].25")), 0)
}

func TestE2ENothingRunning(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "true")
	p := sshtab(t, "kill", w)
	assertEqual(t, p.code, 0)
	assertEqual(t, p.lines()[1:], []string{"(nothing was running)"})
}

func TestE2EWaitingRunReturnsWhenKilled(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	var stdout bytes.Buffer
	waiting := exec.Command(binPath, "run", w, "sleep 45.25", "--wait", "20")
	waiting.Stdout = &stdout
	if err := waiting.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	sshtab(t, "kill", w)
	done := make(chan error, 1)
	go func() { done <- waiting.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		waiting.Process.Kill()
		t.Fatal("run kept waiting after kill")
	}
	assertEqual(t, waiting.ProcessState.ExitCode(), 130)
	assertMatch(t, stdout.String(), `^\[exit 130 · `+w+` ·`)
}

// --- sessions and close ---

func (e *e2e) sessions() map[string]string {
	e.t.Helper()
	p := sshtab(e.t, "sessions", testHost)
	assertEqual(e.t, p.code, 0)
	rows := map[string]string{}
	for _, line := range p.lines()[1:] {
		rows[strings.Fields(line)[0]] = line
	}
	return rows
}

func TestE2ESessionsListsWindowsWithStatus(t *testing.T) {
	e := newE2E(t)
	idle, busy := e.window(), e.window()
	e.runIn(idle, "cd /tmp")
	sshtab(t, "run", busy, "sleep 2", "--wait", "0")
	rows := e.sessions()
	assertMatch(t, rows[windowName(idle)], `exit 0\s+/tmp\s+cd /tmp$`)
	assertMatch(t, rows[windowName(busy)], `running \ds\s+/\S+\s+sleep 2$`)
	e.waitUntilDone(busy)
}

func TestE2ECloseDeletesWindow(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "cd /tmp; export GONE=1")
	p := sshtab(t, "close", w)
	assertEqual(t, []any{p.code, p.stdout}, []any{0, "closed " + w + "\n"})
	if _, ok := e.sessions()[windowName(w)]; ok {
		t.Fatal("closed window is still listed")
	}
	// Using the name again starts a fresh window.
	_, lines := e.runIn(w, "echo ${GONE-unset}; pwd")
	if lines[0] != "unset" || lines[1] == "/tmp" {
		t.Fatalf("reused name kept old state: %q", lines)
	}
}

func TestE2ECloseRefusesRunningWindowUnlessForced(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	sshtab(t, "run", w, "sleep 46.25", "--wait", "0")
	p := sshtab(t, "close", w)
	assertEqual(t, p.code, exitError)
	assertContains(t, p.stderr, "--force")
	assertEqual(t, sshtab(t, "close", w, "--force").stdout, "closed "+w+"\n")
	assertEqual(t, len(remoteProcesses(t, "sleep 46.25")), 0)
}

func TestE2ECloseMissingWindow(t *testing.T) {
	newE2E(t)
	p := sshtab(t, "close", testHost+"/t-never-existed")
	assertEqual(t, p.code, 0)
	assertContains(t, p.stdout, "does not exist")
}

func TestE2EUnknownHost(t *testing.T) {
	newE2E(t)
	p := sshtab(t, "run", "no-such-host.invalid", "true")
	assertEqual(t, p.code, exitError)
	assertContains(t, p.stderr, "cannot run on no-such-host.invalid")
}

// --- push and pull ---

func TestE2EPushIntoWindowDirectoryAndPullBack(t *testing.T) {
	e := newE2E(t)
	w, dir := e.window(), e.tmpPath("")
	e.runIn(w, "mkdir -p "+dir+" && cd "+dir)

	local := t.TempDir()
	payload := make([]byte, 1<<20)
	rand.Read(payload)
	if err := os.WriteFile(filepath.Join(local, "blob.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(local, "data", "a b 测试.csv"), "1,2\n", 0o644)
	writeFile(t, filepath.Join(local, "data", "-dash.txt"), "dash\n", 0o644)
	writeFile(t, filepath.Join(local, "run.sh"), "#!/bin/sh\necho ran\n", 0o755)
	os.Symlink("run.sh", filepath.Join(local, "data", "link"))

	p := sshtab(t, "push", w, filepath.Join(local, "blob.bin"), filepath.Join(local, "data"), filepath.Join(local, "run.sh"))
	assertEqual(t, p.code, 0)
	assertEqual(t, p.lines()[0], "[pushed 5 files, 1.0 MB · "+w+" · "+dir+"]")
	_, lines := e.runIn(w, "./run.sh; cat 'data/a b 测试.csv' data/-dash.txt; readlink data/link")
	assertEqual(t, lines, []string{"ran", "1,2", "dash", "run.sh"})

	back := t.TempDir()
	p = sshtab(t, "pull", w, "blob.bin", "data/", "run.sh", "--to", back)
	assertEqual(t, p.code, 0)
	assertEqual(t, p.lines()[0], "[pulled 5 files, 1.0 MB · "+w+" · "+dir+"]")
	got, _ := os.ReadFile(filepath.Join(back, "blob.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatal("blob.bin changed in the round trip")
	}
	assertEqual(t, readFile(t, filepath.Join(back, "data", "a b 测试.csv")), "1,2\n")
	info, _ := os.Stat(filepath.Join(back, "run.sh"))
	assertEqual(t, info.Mode().Perm(), os.FileMode(0o755))
	link, _ := os.Readlink(filepath.Join(back, "data", "link"))
	assertEqual(t, link, "run.sh")
}

func TestE2EPushTo(t *testing.T) {
	e := newE2E(t)
	w, dir := e.window(), e.tmpPath("")
	e.runIn(w, "mkdir -p "+dir+" && cd "+dir)
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "f.txt"), "x", 0o644)

	p := sshtab(t, "push", w, filepath.Join(local, "f.txt"), "--to", "new/sub")
	assertEqual(t, p.lines()[1], "into "+dir+"/new/sub")
	p = sshtab(t, "push", w, filepath.Join(local, "f.txt"), "--to", dir+"/abs")
	assertEqual(t, p.lines()[1], "into "+dir+"/abs")
	_, lines := e.runIn(w, "cat new/sub/f.txt abs/f.txt")
	assertEqual(t, lines, []string{"xx"})

	// '~/...' means the server's home, which is also where a fresh window starts.
	name := "sshtab-test-" + randomName()
	e.cleanup = append(e.cleanup, "~/"+name)
	p = sshtab(t, "push", w, filepath.Join(local, "f.txt"), "--to", "~/"+name)
	assertEqual(t, p.code, 0)
	_, lines = e.runIn(e.window(), "cat "+name+"/f.txt")
	assertEqual(t, lines, []string{"x"})
}

func TestE2EPullWhileTheWindowIsBusy(t *testing.T) {
	e := newE2E(t)
	w, dir := e.window(), e.tmpPath("")
	e.runIn(w, "mkdir -p "+dir+" && cd "+dir)
	sshtab(t, "run", w, "for i in 1 2 3; do echo epoch $i >> train.log; sleep 1; done", "--wait", "0")
	time.Sleep(1500 * time.Millisecond)
	back := t.TempDir()
	p := sshtab(t, "pull", w, "train.log", "--to", back)
	assertEqual(t, p.code, 0)
	assertContains(t, readFile(t, filepath.Join(back, "train.log")), "epoch 1\n")
	// The command kept running.
	assertEqual(t, sshtab(t, "log", w).code, exitRunning)
	e.waitUntilDone(w)
}

func TestE2ETransfersDoNotCreateWindows(t *testing.T) {
	newE2E(t)
	name := "t-" + randomName()
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "f.txt"), "x", 0o644)
	dest := "/tmp/sshtab-test-" + randomName()
	defer onServer(t, "rm -rf "+dest)
	assertEqual(t, sshtab(t, "push", testHost+"/"+name, filepath.Join(local, "f.txt"), "--to", dest).code, 0)
	assertEqual(t, sshtab(t, "pull", testHost+"/"+name, dest+"/f.txt", "--to", local+"/back").code, 0)
	assertEqual(t, strings.TrimSpace(onServer(t, "test -e ~/.sshtab/sessions/"+name+" && echo yes || echo no")), "no")
}

func TestE2EPullMissingFiles(t *testing.T) {
	e := newE2E(t)
	w := e.window()
	e.runIn(w, "cd /tmp")
	back := t.TempDir()
	p := sshtab(t, "pull", w, "/etc/hostname", "no-such-file-"+randomName(), "--to", back)
	assertEqual(t, p.code, exitError)
	assertContains(t, p.stderr, "no such file on "+w+": no-such-file-")
	if _, err := os.Stat(filepath.Join(back, "hostname")); err == nil {
		t.Fatal("pulled files although one was missing")
	}
}

func TestE2EBigFile(t *testing.T) {
	e := newE2E(t)
	w, dir := e.window(), e.tmpPath("")
	e.runIn(w, "mkdir -p "+dir+" && cd "+dir+" && head -c 50000000 /dev/urandom > big.bin && sha256sum big.bin")
	back := t.TempDir()
	start := time.Now()
	assertEqual(t, sshtab(t, "pull", w, "big.bin", "--to", back).code, 0)
	assertFaster(t, start, 20*time.Second)
	_, lines := e.runIn(w, "sha256sum big.bin")
	out, err := exec.Command("shasum", "-a", "256", filepath.Join(back, "big.bin")).Output()
	if err != nil {
		t.Skip("no shasum locally")
	}
	assertEqual(t, strings.Fields(string(out))[0], strings.Fields(lines[0])[0])
}
