package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pushServer unpacks what sshtab pushes into a temp dir and replies like the driver.
func pushServer(t *testing.T, reply replyOpts) (*fakeServer, string) {
	t.Helper()
	received := t.TempDir()
	return &fakeServer{onStream: func(stdin io.Reader, stdout io.Writer) streamResult {
		if _, err := extractTar(stdin, received); err != nil {
			t.Errorf("server could not read the push: %v", err)
		}
		stdout.Write(fakeReply(reply))
		return streamResult{}
	}}, received
}

// pullServer replies like the driver: a header, then archive.
func pullServer(reply replyOpts, archive []byte, res streamResult) *fakeServer {
	return &fakeServer{onStream: func(stdin io.Reader, stdout io.Writer) streamResult {
		stdout.Write(fakeReply(reply))
		stdout.Write(archive)
		return res
	}}
}

func tarOfDir(t *testing.T, sources ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := writeTar(&buf, sources); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPushSendsFilesAndReports(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "hello", 0o644)
	writeFile(t, filepath.Join(src, "d", "b.txt"), "world!", 0o644)
	server, received := pushServer(t, replyOpts{status: "pushed", cwd: "/srv/w", dest: "/srv/w"})
	r := runCLI(t, server, "", "push", "pdvm/w", filepath.Join(src, "a.txt"), filepath.Join(src, "d"))
	assertEqual(t, r.code, 0)
	assertEqual(t, r.stdout, "[pushed 2 files, 11 bytes · pdvm/w · /srv/w]\n  a.txt\n  d/b.txt\n")
	assertEqual(t, readFile(t, filepath.Join(received, "d", "b.txt")), "world!")
	params := scriptParams(server.scripts[0])
	assertEqual(t, []string{params["SSHTAB_ACTION"], params["SSHTAB_WINDOW"], params["SSHTAB_DEST_B64"]},
		[]string{"push", "w", ""})
}

func TestPushToShowsTheDestination(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "x", 0o644)
	server, _ := pushServer(t, replyOpts{status: "pushed", cwd: "/srv/w", dest: "/srv/w/out"})
	r := runCLI(t, server, "", "push", "pdvm/w", filepath.Join(src, "a.txt"), "--to", "out")
	assertEqual(t, r.stdout, "[pushed 1 file, 1 bytes · pdvm/w · /srv/w]\ninto /srv/w/out\n  a.txt\n")
	assertEqual(t, scriptParams(server.scripts[0])["SSHTAB_DEST_B64"], b64("out"))
}

func TestPushChecksLocalFilesFirst(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "x", 0o644)
	server := &fakeServer{}
	r := runCLI(t, server, "", "push", "pdvm", filepath.Join(src, "a.txt"), "nope1", "nope2")
	assertEqual(t, r.code, exitError)
	assertEqual(t, r.stderr, "error: no such local file: nope1, nope2\n")
	assertEqual(t, len(server.scripts), 0)

	r = runCLI(t, server, "", "push", "pdvm", "/")
	assertContains(t, r.stderr, "refusing to push /")
}

func TestPushFailureExplainsAnExpandedTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "x", 0o644)
	server, _ := pushServer(t, replyOpts{status: "failed", err: "mkdir: cannot create directory: Permission denied\n"})
	r := runCLI(t, server, "", "push", "pdvm/w", filepath.Join(src, "a.txt"), "--to", home+"/data")
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "error: push to pdvm/w failed: mkdir: cannot create directory: Permission denied\n")
	assertContains(t, r.stderr, "hint: "+home+"/data is a local path")
}

func TestPullExtractsIntoTheDestination(t *testing.T) {
	src, dest := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "results", "m.json"), `{"acc": 1}`, 0o644)
	writeFile(t, filepath.Join(src, "train.py"), "print(1)\n", 0o644)
	archive := tarOfDir(t, filepath.Join(src, "results"), filepath.Join(src, "train.py"))
	server := pullServer(replyOpts{status: "pulled", cwd: "/srv/w"}, archive, streamResult{})
	r := runCLI(t, server, "", "pull", "pdvm/w", "results", "train.py", "--to", dest)
	assertEqual(t, r.code, 0)
	assertEqual(t, r.stdout, fmt.Sprintf(
		"[pulled 2 files, 19 bytes · pdvm/w · /srv/w]\ninto %s\n  results/m.json\n  train.py\n", dest))
	assertEqual(t, readFile(t, filepath.Join(dest, "results", "m.json")), `{"acc": 1}`)
	assertEqual(t, scriptParams(server.scripts[0])["SSHTAB_PATHS_B64"], b64("results\ntrain.py"))
}

func TestPullMissingFiles(t *testing.T) {
	server := pullServer(replyOpts{status: "missing", out: "nope.txt\nold/\n"}, nil, streamResult{})
	r := runCLI(t, server, "", "pull", "pdvm/w", "nope.txt", "old/", "ok.txt", "--to", t.TempDir())
	assertEqual(t, r.code, exitError)
	assertEqual(t, r.stderr, "error: no such file on pdvm/w: nope.txt, old/\n")
}

func TestPullTarWarnings(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "train.log"), "epoch 1\n", 0o644)
	archive := tarOfDir(t, filepath.Join(src, "train.log"))
	warning := streamResult{stderr: []byte("tar: ./train.log: file changed as we read it\n"), code: 1}
	r := runCLI(t, pullServer(replyOpts{status: "pulled"}, archive, warning), "", "pull", "pdvm/w", "train.log", "--to", t.TempDir())
	assertEqual(t, r.code, 0)
	assertEqual(t, r.stderr, "warning: tar: ./train.log: file changed as we read it\n")

	fatal := streamResult{stderr: []byte("tar: ./secret: Cannot open: Permission denied\n"), code: 2}
	r = runCLI(t, pullServer(replyOpts{status: "pulled"}, archive, fatal), "", "pull", "pdvm/w", "secret", "--to", t.TempDir())
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "some files could not be read on pdvm: tar: ./secret: Cannot open")
}

func TestPullRefusesHostileArchives(t *testing.T) {
	dest := t.TempDir()
	archive := tarOf(t, tar.Header{Name: "../escape", Typeflag: tar.TypeReg})
	r := runCLI(t, pullServer(replyOpts{status: "pulled"}, archive, streamResult{}), "", "pull", "pdvm/w", "x", "--to", dest)
	assertEqual(t, r.code, exitError)
	assertContains(t, r.stderr, "error: writing local files: refusing unsafe path")
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape")); err == nil {
		t.Fatal("hostile archive escaped the destination")
	}
}

func TestPullDefaultsToTheCurrentDirectory(t *testing.T) {
	src, here := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "m.json"), "{}", 0o644)
	previous, _ := os.Getwd()
	if err := os.Chdir(here); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(previous) })
	server := pullServer(replyOpts{status: "pulled"}, tarOfDir(t, filepath.Join(src, "m.json")), streamResult{})
	r := runCLI(t, server, "", "pull", "pdvm/w", "m.json")
	assertEqual(t, r.stdout, "[pulled 1 file, 2 bytes · pdvm/w · /home/u]\n  m.json\n")
	assertEqual(t, readFile(t, filepath.Join(here, "m.json")), "{}")
}

func TestTransferArguments(t *testing.T) {
	for _, args := range [][]string{{"push", "pdvm"}, {"pull", "pdvm/w"}} {
		r := runCLI(t, nil, "", args...)
		assertEqual(t, r.code, exitError)
		assertContains(t, r.stderr, "missing argument: path...")
	}
	r := runCLI(t, nil, "", "pull", "pdvm", "a\nb")
	assertContains(t, r.stderr, "cannot contain a newline")
}

func TestRenderTransferListsAtMost20Files(t *testing.T) {
	var moved transferred
	for i := 25; i > 0; i-- {
		moved.add(fmt.Sprintf("f%02d", i), 10)
	}
	out := renderTransfer("pulled", moved, "pdvm/w", "/srv", "")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	assertEqual(t, lines[0], "[pulled 25 files, 250 bytes · pdvm/w · /srv]")
	assertEqual(t, lines[1:3], []string{"  f01", "  f02"})
	assertEqual(t, lines[len(lines)-1], "  ... and 5 more")
	assertEqual(t, len(lines), 22)
}
