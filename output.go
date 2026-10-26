package main

// What the agent sees: a status header line, then the raw output.

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

func header(status, target, cwd string) string {
	return "[" + status + " · " + target + " · " + cwd + "]"
}

func statusLabel(r result) string {
	switch r.status {
	case "exit":
		return fmt.Sprintf("exit %d", r.code)
	case "running":
		return "running " + formatDuration(r.elapsed)
	}
	return r.status
}

func formatDuration(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
}

func render(r result, target, emptyNote string) string {
	stdout, stderr := asTerminal(r.stdout), asTerminal(r.stderr)
	var b strings.Builder
	b.WriteString(header(statusLabel(r), target, r.cwd) + "\n")
	if stdout != "" {
		b.WriteString(withNewline(stdout))
	}
	if stderr != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(withNewline(stderr))
	}
	if stdout == "" && stderr == "" && emptyNote != "" {
		b.WriteString(emptyNote + "\n")
	}
	if len(r.truncated) > 0 {
		_, win, _ := strings.Cut(target, "/")
		files := make([]string, len(r.truncated))
		for i, name := range r.truncated {
			files[i] = "~/.sshtab/sessions/" + win + "/" + name
		}
		fmt.Fprintf(&b, "hint: output truncated. In full: %s on the server, "+
			"until this window runs another command\n", strings.Join(files, " and "))
	}
	if r.status == "running" {
		fmt.Fprintf(&b, "hint: still running. `sshtab log %s` to check, `sshtab kill %s` to stop\n", target, target)
	}
	return b.String()
}

// renderTransfer reports a push or pull: a header, the destination when it is
// not the default, and the files moved (the first 20 of them).
func renderTransfer(verb string, t transferred, target, cwd, into string) string {
	const shown = 20
	var b strings.Builder
	noun := "files"
	if len(t.files) == 1 {
		noun = "file"
	}
	status := fmt.Sprintf("%s %d %s, %s", verb, len(t.files), noun, formatSize(int(t.bytes)))
	b.WriteString(header(status, target, cwd) + "\n")
	if into != "" {
		b.WriteString("into " + into + "\n")
	}
	files := append([]string(nil), t.files...)
	sort.Strings(files)
	for i, name := range files {
		if i == shown {
			fmt.Fprintf(&b, "  ... and %d more\n", len(files)-shown)
			break
		}
		b.WriteString("  " + name + "\n")
	}
	return b.String()
}

func renderWindows(windows []window) string {
	if len(windows) == 0 {
		return "(no windows)\n"
	}
	rows := [][]string{{"WINDOW", "STATUS", "DIR", "COMMAND"}}
	for _, w := range windows {
		rows = append(rows, []string{w.name, statusLabel(w.result), w.cwd, w.cmd})
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], displayWidth(cell))
		}
	}
	var b strings.Builder
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = cell + strings.Repeat(" ", widths[i]-displayWidth(cell))
		}
		b.WriteString(strings.TrimRightFunc(strings.Join(cells, "  "), unicode.IsSpace) + "\n")
	}
	return b.String()
}

// displayWidth counts terminal columns: CJK and other wide characters take two.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if isWide(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// wideRanges are the main East Asian Wide and Fullwidth blocks.
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2E80, 0x303E},   // CJK radicals, punctuation
	{0x3041, 0x33FF},   // kana, CJK symbols
	{0x3400, 0x4DBF},   // CJK extension A
	{0x4E00, 0x9FFF},   // CJK unified ideographs
	{0xA000, 0xA4CF},   // Yi
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compatibility ideographs
	{0xFE30, 0xFE4F},   // CJK compatibility forms
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x1F300, 0x1F64F}, // emoji
	{0x1F900, 0x1F9FF}, // emoji
	{0x20000, 0x3FFFD}, // CJK extensions B and later
}

func isWide(r rune) bool {
	for _, w := range wideRanges {
		if r >= w[0] && r <= w[1] {
			return true
		}
	}
	return false
}

// asTerminal shows \r redraws (tqdm, curl progress) as a terminal would: only the last one.
func asTerminal(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if j := strings.LastIndexByte(line, '\r'); j >= 0 {
			line = line[j+1:]
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

// splitLines splits text into lines, ignoring one trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func withNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
