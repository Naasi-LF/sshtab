package main

// File transfer for push and pull: tar streams over the same ssh connection.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// transferred lists what a push or pull moved.
type transferred struct {
	files []string // regular files and symlinks, by their path in the destination
	bytes int64
}

func (t *transferred) add(name string, size int64) {
	t.files = append(t.files, name)
	t.bytes += size
}

// writeTar archives each source under its own name, like `cp -r`: directories
// recursively, symlinks as links (never followed). Other file types are skipped.
func writeTar(w io.Writer, sources []string) (transferred, error) {
	var done transferred
	tw := tar.NewWriter(w)
	for _, source := range sources {
		abs, err := filepath.Abs(source)
		if err != nil {
			return done, err
		}
		base := filepath.Base(abs)
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(abs, p)
			if err != nil {
				return err
			}
			name := path.Join(base, filepath.ToSlash(rel))
			info, err := d.Info()
			if err != nil {
				return err
			}
			link := ""
			if info.Mode()&fs.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			} else if !info.Mode().IsRegular() && !info.IsDir() {
				return nil // sockets, devices, fifos
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			if info.IsDir() {
				hdr.Name += "/"
			}
			hdr.Uname, hdr.Gname = "", "" // the server's users are not ours
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				if _, err := io.Copy(tw, f); err != nil {
					return err
				}
			}
			if !info.IsDir() {
				done.add(name, hdr.Size)
			}
			return nil
		})
		if err != nil {
			return done, err
		}
	}
	return done, tw.Close()
}

// extractTar unpacks a tar stream from the server into dest. It never writes
// outside dest: names with ".." or a leading "/" are refused, and so is
// writing through a symlink, which a hostile archive could plant first.
func extractTar(r io.Reader, dest string) (transferred, error) {
	var done transferred
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return done, err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return done, nil
		}
		if err != nil {
			return done, fmt.Errorf("reading the archive: %w", err)
		}
		name, err := safeName(hdr.Name)
		if err != nil {
			return done, err
		}
		if name == "" {
			continue
		}
		target, err := safeParent(dest, name)
		if err != nil {
			return done, err
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, mode|0o700); err != nil && !isDir(target) {
				return done, err
			}
		case tar.TypeReg:
			if err := removeIfLink(target); err != nil {
				return done, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return done, err
			}
			_, err = io.Copy(f, tr)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return done, err
			}
			// OpenFile only applies the mode to new files.
			if err := os.Chmod(target, mode); err != nil {
				return done, err
			}
			if err := os.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
				return done, err
			}
			done.add(name, hdr.Size)
		case tar.TypeSymlink:
			if err := removeIfLink(target); err != nil {
				return done, err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return done, err
			}
			done.add(name, 0)
		}
	}
}

// safeName cleans an archive member name and refuses ones that leave the
// destination. "" means the destination itself.
func safeName(name string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	switch {
	case path.IsAbs(name) || strings.HasPrefix(name, "/"):
		return "", fmt.Errorf("refusing unsafe path from the server: %q", name)
	case clean == "..", strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("refusing unsafe path from the server: %q", name)
	case clean == ".":
		return "", nil
	}
	return clean, nil
}

// safeParent creates the directories between dest and name as needed,
// refusing to pass through a symlink, and returns the full target path.
func safeParent(dest, name string) (string, error) {
	parts := strings.Split(name, "/")
	dir := dest
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case info.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("refusing to write through the symlink %s", dir)
		case !info.IsDir():
			return "", fmt.Errorf("%s is not a directory", dir)
		}
	}
	return filepath.Join(dir, parts[len(parts)-1]), nil
}

func removeIfLink(p string) error {
	if info, err := os.Lstat(p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return os.Remove(p)
	}
	return nil
}

func isDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}
