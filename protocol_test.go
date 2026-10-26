package main

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// replyOpts describes a fake driver reply; zero values mean an exit 0 in /home/u.
type replyOpts struct {
	out, err       string
	code           int
	cwd            string
	status         string
	elapsed        int
	cmd, signal    string
	outCut, errCut string
}

func fakeReply(o replyOpts) []byte {
	if o.cwd == "" {
		o.cwd = "/home/u"
	}
	if o.status == "" {
		o.status = "exit"
	}
	return []byte(fmt.Sprintf(
		"SSHTAB1 status=%s code=%d cwd=%s elapsed=%d cmd=%s signal=%s out_cut=%s err_cut=%s out=%d err=%d\n%s%s",
		o.status, o.code, b64(o.cwd), o.elapsed, b64(o.cmd), o.signal, o.outCut, o.errCut,
		len(o.out), len(o.err), o.out, o.err))
}

func mustParse(t *testing.T, raw []byte) result {
	t.Helper()
	r, err := parseReply(raw)
	if err != nil {
		t.Fatalf("parseReply: %v", err)
	}
	return r
}

func assertEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func assertContains(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%q does not contain %q", s, sub)
	}
}

func TestParseRoundTrip(t *testing.T) {
	r := mustParse(t, fakeReply(replyOpts{out: "hi\n", err: "oops\n", code: 2, cwd: "/data/项目"}))
	assertEqual(t, []any{r.status, r.code, r.cwd, r.stdout, r.stderr}, []any{"exit", 2, "/data/项目", "hi\n", "oops\n"})
}

func TestParseRunningFields(t *testing.T) {
	r := mustParse(t, fakeReply(replyOpts{out: "epoch 1\n", status: "running", elapsed: 31, cmd: "python train.py"}))
	assertEqual(t, []any{r.status, r.elapsed, r.cmd}, []any{"running", 31, "python train.py"})
}

func TestOutputThatLooksLikeAHeaderIsJustOutput(t *testing.T) {
	fake := "SSHTAB1 status=exit code=0 cwd= out=0 err=0\n"
	r := mustParse(t, fakeReply(replyOpts{out: fake}))
	assertEqual(t, r.stdout, fake)
	assertEqual(t, r.stderr, "")
}

func TestParseSkipsNoiseBeforeHeader(t *testing.T) {
	r := mustParse(t, append([]byte("welcome from .zshenv\n"), fakeReply(replyOpts{out: "ok\n"})...))
	assertEqual(t, r.stdout, "ok\n")
}

func TestInvalidUTF8IsReplaced(t *testing.T) {
	assertEqual(t, mustParse(t, fakeReply(replyOpts{out: "\xff\n"})).stdout, "�\n")
}

func TestParseErrors(t *testing.T) {
	full := fakeReply(replyOpts{out: "abc"})
	for _, raw := range []string{
		"", "no header here\n", "SSHTAB1 status=exit\n", string(full[:len(full)-1]),
		"SSHTAB1 status=exit code=x cwd= out=0 err=0\n", "SSHTAB1 status=exit code=0 cwd= out=-1 err=0\n",
		"SSHTAB1 status=exit code=0 cwd= out=0 err=0 out_cut=1:2\n",
	} {
		if _, err := parseReply([]byte(raw)); err == nil {
			t.Errorf("parseReply(%q) did not fail", raw)
		}
	}
}

func TestBuildScript(t *testing.T) {
	command := `echo "it's" $HOME`
	script := buildScript("run", "train", param{"cmd_b64", encodeCommand(command)}, param{"wait", "30"})
	assertEqual(t, strings.Split(script, "\n")[:4], []string{
		"SSHTAB_ACTION=run", "SSHTAB_WINDOW=train", "SSHTAB_CMD_B64=" + b64(command), "SSHTAB_WAIT=30",
	})
	assertContains(t, script, "SSHTAB1 status=")
}

func TestCutsMoveToLineBreaks(t *testing.T) {
	// The driver kept 9 head bytes and 9 tail bytes of "line1\nline2\n<middle>\nline8\nline9\n".
	head, tail := "line1\nlin", "e8\nline9\n"
	r := mustParse(t, fakeReply(replyOpts{out: head + tail, outCut: fmt.Sprintf("%d:2000:300", len(head))}))
	assertEqual(t, r.stdout, "line1\n... [2.0 KB omitted, 301 lines] ...\nline9\n")
	assertEqual(t, r.truncated, []string{"stdout"})
}

func TestProgressBarCutAtCarriageReturn(t *testing.T) {
	head, tail := " 1%|#\r 2%|#", "#\r99%|####\r100%|#####\ndone\n"
	r := mustParse(t, fakeReply(replyOpts{err: head + tail, errCut: fmt.Sprintf("%d:6291456:0", len(head))}))
	assertContains(t, render(r, "pdvm/train", ""), "... [6.0 MB omitted] ...\n100%|#####\ndone\n")
	assertEqual(t, r.truncated, []string{"stderr"})
}

func TestSingleHugeLineIsCutWhereItIs(t *testing.T) {
	head, tail := strings.Repeat("x", 10), strings.Repeat("y", 20)
	r := mustParse(t, fakeReply(replyOpts{out: head + tail, outCut: "10:5:0"}))
	assertEqual(t, r.stdout, head+"\n... [5 bytes omitted] ...\n"+tail)
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int]string{5: "5 bytes", 2048: "2.0 KB", 78_888_888: "75.2 MB", 3 << 30: "3.0 GB"} {
		assertEqual(t, formatSize(n), want)
	}
}

func TestWithCommas(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 9995172: "9,995,172"} {
		assertEqual(t, withCommas(n), want)
	}
}

func TestParseWindows(t *testing.T) {
	listing := "main\texit\t0\t400\t" + b64("/home/u") + "\t" + b64("ls") + "\n"
	windows, err := parseWindows(listing)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, windows, []window{{"main", result{status: "exit", cwd: "/home/u", elapsed: 400, cmd: "ls"}}})

	if windows, err := parseWindows(""); err != nil || len(windows) != 0 {
		t.Fatalf("empty listing: %v, %v", windows, err)
	}
	if _, err := parseWindows("main\texit\n"); err == nil {
		t.Fatal("bad line accepted")
	}
}
