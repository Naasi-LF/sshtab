package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// tarOf builds an archive by hand, for entries writeTar would never produce.
func tarOf(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		h := h
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = "pwned"
			h.Size = int64(len(body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	return buf.Bytes()
}

func TestTarRoundTrip(t *testing.T) {
	src, dest := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(src, "data", "a b 测试.csv"), "1,2\n", 0o644)
	writeFile(t, filepath.Join(src, "data", "sub", "deep.txt"), "deep", 0o600)
	os.MkdirAll(filepath.Join(src, "data", "empty"), 0o755)
	os.Symlink("sub/deep.txt", filepath.Join(src, "data", "link"))
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(src, "run.sh"), old, old)

	var archive bytes.Buffer
	sent, err := writeTar(&archive, []string{filepath.Join(src, "run.sh"), filepath.Join(src, "data")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := extractTar(&archive, dest)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"run.sh", "data/a b 测试.csv", "data/link", "data/sub/deep.txt"}
	assertEqual(t, sent.files, wantFiles)
	assertEqual(t, got.files, wantFiles)
	assertEqual(t, sent.bytes, int64(len("#!/bin/sh\necho hi\n")+len("1,2\n")+len("deep")))
	assertEqual(t, got.bytes, sent.bytes)

	assertEqual(t, readFile(t, filepath.Join(dest, "data", "a b 测试.csv")), "1,2\n")
	for name, mode := range map[string]os.FileMode{"run.sh": 0o755, "data/sub/deep.txt": 0o600} {
		info, _ := os.Stat(filepath.Join(dest, name))
		assertEqual(t, info.Mode().Perm(), mode)
	}
	info, _ := os.Stat(filepath.Join(dest, "run.sh"))
	assertEqual(t, info.ModTime().UTC(), old)
	link, err := os.Readlink(filepath.Join(dest, "data", "link"))
	assertEqual(t, []any{link, err}, []any{"sub/deep.txt", nil})
	if !isDir(filepath.Join(dest, "data", "empty")) {
		t.Fatal("empty directory not created")
	}
}

func TestSourcesKeepTheirOwnName(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "results", "m.json"), "{}", 0o644)
	var archive bytes.Buffer
	// Trailing slash or not, the directory arrives under its own name, like cp -r.
	sent, err := writeTar(&archive, []string{filepath.Join(src, "results") + "/"})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, sent.files, []string{"results/m.json"})
}

func TestExtractOverwritesFiles(t *testing.T) {
	dest := t.TempDir()
	writeFile(t, filepath.Join(dest, "f"), "old content that is longer", 0o644)
	if _, err := extractTar(bytes.NewReader(tarOf(t, tar.Header{Name: "f", Typeflag: tar.TypeReg})), dest); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, readFile(t, filepath.Join(dest, "f")), "pwned")
}

func TestExtractRefusesUnsafePaths(t *testing.T) {
	for _, name := range []string{"../escape", "a/../../escape", "/etc/escape", "./../escape"} {
		dest := t.TempDir()
		_, err := extractTar(bytes.NewReader(tarOf(t, tar.Header{Name: name, Typeflag: tar.TypeReg})), dest)
		if err == nil || !strings.Contains(err.Error(), "unsafe path") {
			t.Errorf("%q: err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape")); err == nil {
			t.Fatalf("%q escaped the destination", name)
		}
	}
}

func TestExtractRefusesWritingThroughSymlinks(t *testing.T) {
	dest, outside := t.TempDir(), t.TempDir()
	archive := tarOf(t,
		tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: outside},
		tar.Header{Name: "evil/pwned.txt", Typeflag: tar.TypeReg},
	)
	_, err := extractTar(bytes.NewReader(archive), dest)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatal("wrote through the symlink")
	}
}

func TestExtractReplacesSymlinkInsteadOfFollowingIt(t *testing.T) {
	dest, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "safe", 0o644)
	os.Symlink(victim, filepath.Join(dest, "f"))
	if _, err := extractTar(bytes.NewReader(tarOf(t, tar.Header{Name: "f", Typeflag: tar.TypeReg})), dest); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, readFile(t, victim), "safe")
	assertEqual(t, readFile(t, filepath.Join(dest, "f")), "pwned")
}

func TestSafeName(t *testing.T) {
	for name, want := range map[string]string{"./a/b": "a/b", "a//b/": "a/b", "./": "", "a/./b": "a/b"} {
		got, err := safeName(name)
		if err != nil {
			t.Fatal(err)
		}
		assertEqual(t, got, want)
	}
}
