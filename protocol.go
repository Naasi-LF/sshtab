package main

// Wire format between sshtab and the remote driver (driver.sh).
//
// sshtab sends the driver on ssh's stdin, with its parameters prepended as
// shell assignments; the user's command is base64 so it never needs quoting.
// The driver answers with one header line followed by raw stdout and stderr:
//
//	SSHTAB1 status=running code=0 cwd=<base64> elapsed=31 cmd=<base64> signal= out_cut= err_cut= out=<n bytes> err=<n bytes>\n<stdout><stderr>
//
// status is a window state (exit, running, stopped, idle) or an action
// outcome (busy, closed, missing, list). The streams are delimited by their
// lengths, not by markers, so any output is safe. For list, stdout holds one
// line per window; see parseWindows.
//
// A stream that was too long arrives as its first and last bytes only, and
// its *_cut field says "<head bytes>:<omitted bytes>:<omitted newlines>".

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const magic = "SSHTAB1 "

//go:embed driver.sh
var driver string

type result struct {
	status    string
	code      int
	cwd       string
	stdout    string
	stderr    string
	elapsed   int      // seconds since the window's last command started
	cmd       string   // first line of that command
	signal    string   // last signal `kill` sent (INT, TERM or KILL), if any
	truncated []string // names of the streams whose middle was cut out
}

type param struct{ key, value string }

// buildScript pastes values into bash unquoted: only pass validated names,
// numbers and base64.
func buildScript(action, window string, params ...param) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SSHTAB_ACTION=%s\nSSHTAB_WINDOW=%s\n", action, window)
	for _, p := range params {
		fmt.Fprintf(&b, "SSHTAB_%s=%s\n", strings.ToUpper(p.key), p.value)
	}
	b.WriteString(driver)
	return b.String()
}

func encodeCommand(command string) string {
	return base64.StdEncoding.EncodeToString([]byte(command))
}

func parseReply(raw []byte) (result, error) {
	// Server startup files (e.g. a chatty .zshenv) may print before the driver runs.
	start := 0
	if !bytes.HasPrefix(raw, []byte(magic)) {
		i := bytes.Index(raw, []byte("\n"+magic))
		if i < 0 {
			return result{}, errors.New("no header from remote driver")
		}
		start = i + 1
	}
	header, body, found := bytes.Cut(raw[start:], []byte("\n"))
	if !found {
		return result{}, fmt.Errorf("bad header %q: missing newline after header", header)
	}
	r, err := parseHeader(header, body)
	if err != nil {
		return result{}, fmt.Errorf("bad header %q: %w", header, err)
	}
	return r, nil
}

func parseHeader(header, body []byte) (result, error) {
	fields := map[string]string{}
	for _, item := range strings.Fields(string(header))[1:] {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			return result{}, fmt.Errorf("field without '=': %q", item)
		}
		fields[key] = value
	}
	var r result
	var outLen, errLen int
	var outCut, errCut string
	for _, f := range []struct {
		key      string
		required bool
		into     any
	}{
		{"status", true, &r.status},
		{"code", true, &r.code},
		{"cwd", true, &r.cwd},
		{"out", true, &outLen},
		{"err", true, &errLen},
		{"elapsed", false, &r.elapsed},
		{"cmd", false, &r.cmd},
		{"signal", false, &r.signal},
		{"out_cut", false, &outCut},
		{"err_cut", false, &errCut},
	} {
		value, ok := fields[f.key]
		if !ok {
			if f.required {
				return result{}, fmt.Errorf("missing %s", f.key)
			}
			continue
		}
		switch into := f.into.(type) {
		case *int:
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return result{}, fmt.Errorf("%s is not a count: %q", f.key, value)
			}
			*into = n
		case *string:
			*into = value
		}
	}
	var err error
	if r.cwd, err = unb64(r.cwd); err != nil {
		return result{}, fmt.Errorf("cwd: %w", err)
	}
	if r.cmd, err = unb64(r.cmd); err != nil {
		return result{}, fmt.Errorf("cmd: %w", err)
	}
	if len(body) < outLen+errLen {
		return result{}, errors.New("response ended early")
	}
	if r.stdout, err = decodeStream(body[:outLen], outCut); err != nil {
		return result{}, fmt.Errorf("out_cut: %w", err)
	}
	if r.stderr, err = decodeStream(body[outLen:outLen+errLen], errCut); err != nil {
		return result{}, fmt.Errorf("err_cut: %w", err)
	}
	if outCut != "" {
		r.truncated = append(r.truncated, "stdout")
	}
	if errCut != "" {
		r.truncated = append(r.truncated, "stderr")
	}
	return r, nil
}

// decodeStream decodes a stream; if the driver cut its middle out, it marks where.
func decodeStream(data []byte, cut string) (string, error) {
	if cut == "" {
		return text(data), nil
	}
	parts := strings.Split(cut, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("want 3 numbers, got %q", cut)
	}
	var n [3]int
	for i, p := range parts {
		var err error
		if n[i], err = strconv.Atoi(p); err != nil || n[i] < 0 {
			return "", fmt.Errorf("not a count: %q", p)
		}
	}
	headLen, omitted, lines := n[0], n[1], n[2]
	if headLen > len(data) {
		return "", fmt.Errorf("head of %d bytes in %d", headLen, len(data))
	}
	head, tail := data[:headLen], data[headLen:]
	// Move both cuts to line breaks, so no line or UTF-8 character is split.
	// \r counts as a break too: progress bars redraw with \r and rarely print \n.
	if keep := max(bytes.LastIndexByte(head, '\n'), bytes.LastIndexByte(head, '\r')) + 1; keep > 0 {
		omitted += len(head) - keep
		head = head[:keep]
	}
	if i := bytes.IndexAny(tail, "\n\r"); i >= 0 {
		dropped := tail[:i+1]
		omitted += len(dropped)
		lines += bytes.Count(dropped, []byte("\n"))
		tail = tail[i+1:]
	}
	separator := ""
	if len(head) > 0 && !bytes.HasSuffix(head, []byte("\n")) && !bytes.HasSuffix(head, []byte("\r")) {
		separator = "\n"
	}
	count := ""
	if lines > 0 {
		count = ", " + withCommas(lines) + " lines"
	}
	marker := "... [" + formatSize(omitted) + " omitted" + count + "] ...\n"
	return text(head) + separator + marker + text(tail), nil
}

type window struct {
	name string
	result
}

// parseWindows reads the list reply: one tab-separated line per window, as
// written by driver.sh.
func parseWindows(stdout string) ([]window, error) {
	var windows []window
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("bad window line %q: want 6 fields", line)
		}
		code, err1 := strconv.Atoi(f[2])
		elapsed, err2 := strconv.Atoi(f[3])
		cwd, err3 := unb64(f[4])
		cmd, err4 := unb64(f[5])
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("bad window line %q: %w", line, err)
		}
		windows = append(windows, window{f[0], result{
			status: f[1], code: code, cwd: cwd, elapsed: elapsed, cmd: cmd,
		}})
	}
	return windows, nil
}

func formatSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	size := float64(n) / 1024
	for _, unit := range []string{"KB", "MB"} {
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024
	}
	return fmt.Sprintf("%.1f GB", size)
}

func withCommas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func unb64(s string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(s)
	return text(data), err
}

// text decodes output that should be UTF-8 but may not be.
func text(data []byte) string {
	return strings.ToValidUTF8(string(data), "�")
}
