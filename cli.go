package main

// Command-line entry point: parses `sshtab <command> <host[/window]> ...`.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const defaultWindow = "main"

const (
	// exitError is sshtab's own failure (bad arguments, connection errors,
	// busy window), like ssh's 255.
	exitError = 255
	// exitRunning means the command is still running: "try again later"
	// (sysexits EX_TEMPFAIL).
	exitRunning = 75
	// exitStopped means the command died without reporting an exit code,
	// as after SIGKILL.
	exitStopped = 137
)

var (
	windowRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	durationRE = regexp.MustCompile(`^(\d+)([smh]?)$`)
)

type target struct{ host, window string }

func (t target) String() string { return t.host + "/" + t.window }

func parseTarget(text string) (target, error) {
	host, win, slash := strings.Cut(text, "/")
	if host == "" || strings.HasPrefix(host, "-") || strings.IndexFunc(host, unicode.IsSpace) >= 0 {
		return target{}, fmt.Errorf("invalid host: %q", host)
	}
	if !slash {
		win = defaultWindow
	} else if !windowRE.MatchString(win) {
		return target{}, fmt.Errorf(
			"invalid window name: %q (letters, digits, '_' and '-', at most 32 characters)", win)
	}
	return target{host, win}, nil
}

// parseDuration turns "30", "30s", "5m" or "2h" into seconds.
func parseDuration(text string) (int, error) {
	m := durationRE.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("invalid duration: %q (e.g. 30s, 5m, 2h)", text)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %q (e.g. 30s, 5m, 2h)", text)
	}
	return n * map[string]int{"": 1, "s": 1, "m": 60, "h": 3600}[m[2]], nil
}

type flagSpec struct {
	name, metavar, help string // an empty metavar makes a boolean flag
}

type commandSpec struct {
	name        string
	positionals []string // all required; a last name ending in "..." takes one or more
	flags       []flagSpec
	help        string
}

func (s commandSpec) variadic() bool {
	return strings.HasSuffix(s.positionals[len(s.positionals)-1], "...")
}

var commands = []commandSpec{
	{"run", []string{"host[/window]", "command"}, []flagSpec{
		{"wait", "DURATION", "return after this long even if the command is still running\n" +
			"(it keeps running in the window); 90, 90s, 10m, 2h or 0; default 30s"},
	}, "run a command in a window (command - reads it from stdin)"},
	{"log", []string{"host[/window]"}, []flagSpec{
		{"tail", "N", "show the last N lines instead, whether seen before or not"},
	}, "show a window's output since you last looked"},
	{"kill", []string{"host[/window]"}, nil,
		"stop a window's command: Ctrl-C, then SIGTERM, then SIGKILL"},
	{"sessions", []string{"host"}, nil, "list a host's windows"},
	{"close", []string{"host[/window]"}, []flagSpec{
		{"force", "", "stop its command first if running"},
	}, "delete a window and its state"},
	{"push", []string{"host[/window]", "path..."}, []flagSpec{
		{"to", "DIR", "server directory to copy into, relative to the window's directory\n" +
			"(quote ~ so your local shell leaves it alone); default: the window's directory"},
	}, "copy local files or directories to the server, into the window's directory"},
	{"pull", []string{"host[/window]", "path..."}, []flagSpec{
		{"to", "DIR", "local directory to copy into; default: the current directory"},
	}, "copy server files or directories, relative to the window's directory, to here"},
}

func (s commandSpec) usage() string {
	parts := append([]string{"sshtab", s.name}, s.positionals...)
	for _, f := range s.flags {
		if f.metavar == "" {
			parts = append(parts, "[--"+f.name+"]")
		} else {
			parts = append(parts, "[--"+f.name+" "+f.metavar+"]")
		}
	}
	return "usage: " + strings.Join(parts, " ")
}

func (s commandSpec) longHelp() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n", s.usage(), s.help)
	if len(s.flags) > 0 {
		b.WriteString("\noptions:\n")
		for _, f := range s.flags {
			name := "--" + f.name
			if f.metavar != "" {
				name += " " + f.metavar
			}
			lines := strings.Split(f.help, "\n")
			fmt.Fprintf(&b, "  %-16s %s\n", name, lines[0])
			for _, line := range lines[1:] {
				fmt.Fprintf(&b, "  %-16s %s\n", "", line)
			}
		}
	}
	return b.String()
}

func mainHelp() string {
	var b strings.Builder
	b.WriteString("usage: sshtab <command> ...\n\n" +
		"Remote shell for AI agents: terminal-like windows on a Linux server over SSH.\n\n" +
		"commands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.help)
	}
	b.WriteString("\nhost is a name from ~/.ssh/config; host alone means the window host/main.\n" +
		"Run `sshtab <command> --help` for a command's options.\n")
	return b.String()
}

var errHelp = errors.New("help requested")

// parseArgs splits args into positionals and --flags, which may come in any
// order (`run host cmd --wait 5s`), as --name value or --name=value. "--"
// ends the flags, and "-" alone is a positional (stdin).
func (s commandSpec) parseArgs(args []string) ([]string, map[string]string, error) {
	var positionals []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			return nil, nil, errHelp
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		var spec *flagSpec
		for j := range s.flags {
			if s.flags[j].name == name && strings.HasPrefix(arg, "--") {
				spec = &s.flags[j]
			}
		}
		switch {
		case spec == nil:
			return nil, nil, fmt.Errorf("unrecognized option: %s", arg)
		case spec.metavar == "" && hasValue:
			return nil, nil, fmt.Errorf("option --%s takes no value", name)
		case spec.metavar == "":
			value = "true"
		case !hasValue:
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("option --%s needs a value (%s)", name, spec.metavar)
			}
			i++
			value = args[i]
		}
		flags[name] = value
	}
	if len(positionals) < len(s.positionals) {
		return nil, nil, fmt.Errorf("missing argument: %s", s.positionals[len(positionals)])
	}
	if len(positionals) > len(s.positionals) && !s.variadic() {
		return nil, nil, fmt.Errorf("unexpected argument: %q", positionals[len(s.positionals)])
	}
	return positionals, flags, nil
}

// cli runs one sshtab invocation. call and stream talk to the server; tests
// replace them.
type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	call           func(host, script string) (sshReply, error)
	stream         func(host, script string, stdin io.Reader, stdout io.Writer) (streamResult, error)
}

func (c *cli) main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, mainHelp())
		return exitError
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(c.stdout, mainHelp())
		return 0
	case "--version", "version":
		fmt.Fprintln(c.stdout, "sshtab", version)
		return 0
	}
	var spec *commandSpec
	for i := range commands {
		if commands[i].name == args[0] {
			spec = &commands[i]
		}
	}
	if spec == nil {
		return c.usageError("usage: sshtab <command> ...", fmt.Sprintf(
			"unknown command %q (choose from run, log, kill, sessions, close)", args[0]))
	}
	positionals, flags, err := spec.parseArgs(args[1:])
	if errors.Is(err, errHelp) {
		fmt.Fprint(c.stdout, spec.longHelp())
		return 0
	}
	if err != nil {
		return c.usageError(spec.usage(), err.Error())
	}

	t, err := parseTarget(positionals[0])
	if err != nil {
		return c.fail(err.Error())
	}
	switch spec.name {
	case "run":
		wait := 30
		if value, ok := flags["wait"]; ok {
			if wait, err = parseDuration(value); err != nil {
				return c.usageError(spec.usage(), "--wait: "+err.Error())
			}
		}
		return c.run(t, positionals[1], wait)
	case "log":
		tail, ok := flags["tail"]
		if ok && (tail == "" || strings.Trim(tail, "0123456789") != "") {
			return c.usageError(spec.usage(), fmt.Sprintf("--tail: not a line count: %q", tail))
		}
		return c.log(t, tail)
	case "kill":
		return c.kill(t)
	case "sessions":
		if strings.Contains(positionals[0], "/") {
			return c.fail("sessions takes a host, not a window: try `sshtab sessions " + t.host + "`")
		}
		return c.sessions(t.host)
	case "push":
		return c.push(t, positionals[1:], flags["to"])
	case "pull":
		return c.pull(t, positionals[1:], flags["to"])
	default:
		return c.close(t, flags["force"] != "")
	}
}

func (c *cli) run(t target, command string, wait int) int {
	if command == "-" {
		data, err := io.ReadAll(c.stdin)
		if err != nil {
			return c.fail("cannot read the command from stdin: " + err.Error())
		}
		command = string(data)
	}
	r, ok := c.request(t.host, buildScript("run", t.window,
		param{"cmd_b64", encodeCommand(command)}, param{"wait", strconv.Itoa(wait)}))
	if !ok {
		return exitError
	}
	if r.status == "busy" {
		other := "main"
		if t.window == defaultWindow {
			other = "main2"
		}
		return c.fail(fmt.Sprintf("%s\nhint: use another window, e.g. `sshtab run %s/%s '...'`, "+
			"or `sshtab kill %s` to stop it", busy(t, r), t.host, other, t))
	}
	fmt.Fprint(c.stdout, render(r, t.String(), ""))
	return exitCode(r)
}

func (c *cli) log(t target, tail string) int {
	r, ok := c.request(t.host, buildScript("log", t.window, param{"tail", tail}))
	if !ok {
		return exitError
	}
	fmt.Fprint(c.stdout, render(r, t.String(), "(no new output)"))
	return exitCode(r)
}

func (c *cli) kill(t target) int {
	r, ok := c.request(t.host, buildScript("kill", t.window))
	if !ok {
		return exitError
	}
	if r.status == "running" {
		return c.fail(fmt.Sprintf("could not stop %s: still running after SIGKILL", t))
	}
	fmt.Fprint(c.stdout, render(r, t.String(), ""))
	if r.signal != "" {
		fmt.Fprintf(c.stdout, "stopped with SIG%s\n", r.signal)
	} else {
		fmt.Fprintln(c.stdout, "(nothing was running)")
	}
	return 0
}

func (c *cli) sessions(host string) int {
	r, ok := c.request(host, buildScript("list", ""))
	if !ok {
		return exitError
	}
	windows, err := parseWindows(r.stdout)
	if err != nil {
		return c.fail(fmt.Sprintf("bad reply from %s: %v", host, err))
	}
	fmt.Fprint(c.stdout, renderWindows(windows))
	return 0
}

func (c *cli) close(t target, force bool) int {
	forceValue := ""
	if force {
		forceValue = "1"
	}
	r, ok := c.request(t.host, buildScript("close", t.window, param{"force", forceValue}))
	if !ok {
		return exitError
	}
	switch r.status {
	case "busy":
		hint := ""
		if !force {
			hint = fmt.Sprintf("\nhint: `sshtab kill %s` first, or `sshtab close %s --force`", t, t)
		}
		return c.fail(busy(t, r) + hint)
	case "missing":
		fmt.Fprintf(c.stdout, "%s does not exist (nothing to close)\n", t)
	default:
		fmt.Fprintf(c.stdout, "closed %s\n", t)
	}
	return 0
}

// errTransferOver stops packing files once ssh has finished, e.g. after the
// server refused the push.
var errTransferOver = errors.New("transfer over")

func (c *cli) push(t target, sources []string, to string) int {
	var missing []string
	for _, s := range sources {
		if _, err := os.Lstat(s); err != nil {
			missing = append(missing, s)
		} else if abs, _ := filepath.Abs(s); abs == "/" {
			return c.fail("refusing to push / (the whole filesystem)")
		}
	}
	if len(missing) > 0 {
		return c.fail("no such local file: " + strings.Join(missing, ", "))
	}
	if strings.Contains(to, "\n") {
		return c.fail("--to: a path cannot contain a newline")
	}

	archive, archiveWriter := io.Pipe()
	type packed struct {
		sent transferred
		err  error
	}
	packing := make(chan packed, 1)
	go func() {
		sent, err := writeTar(archiveWriter, sources)
		archiveWriter.CloseWithError(err)
		packing <- packed{sent, err}
	}()
	var reply bytes.Buffer
	res, err := c.stream(t.host, buildScript("push", t.window, param{"dest_b64", encodeCommand(to)}), archive, &reply)
	archive.CloseWithError(errTransferOver) // unblocks the packer if ssh quit early
	p := <-packing
	if err != nil {
		return c.fail("cannot run ssh: " + err.Error())
	}
	if p.err != nil && !errors.Is(p.err, errTransferOver) {
		return c.fail("reading local files: " + p.err.Error())
	}
	r, err := parseReply(reply.Bytes())
	if err != nil {
		return c.fail(fmt.Sprintf("cannot run on %s: %s", t.host, sshDetail(res.stderr)))
	}
	if r.status != "pushed" {
		return c.fail(fmt.Sprintf("push to %s failed: %s%s", t, strings.TrimSpace(r.stderr), localHomeHint([]string{to})))
	}
	into := ""
	if r.dest != r.cwd {
		into = r.dest
	}
	fmt.Fprint(c.stdout, renderTransfer("pushed", p.sent, t.String(), r.cwd, into))
	return 0
}

func (c *cli) pull(t target, paths []string, to string) int {
	for _, p := range append([]string{to}, paths...) {
		if strings.Contains(p, "\n") {
			return c.fail("a path cannot contain a newline")
		}
	}
	dest := to
	if dest == "" {
		dest = "."
	}

	data, dataWriter := io.Pipe()
	type unpacked struct {
		r   result
		got transferred
		err error // problem with the reply
		bad error // problem writing local files
	}
	unpacking := make(chan unpacked, 1)
	go func() {
		var u unpacked
		br := bufio.NewReader(data)
		if u.r, u.err = readReply(br); u.err == nil && u.r.status == "pulled" {
			u.got, u.bad = extractTar(br, dest)
		}
		io.Copy(io.Discard, br) // keep ssh flowing even if we stopped early
		unpacking <- u
	}()
	script := buildScript("pull", t.window, param{"paths_b64", encodeCommand(strings.Join(paths, "\n"))})
	res, err := c.stream(t.host, script, strings.NewReader(""), dataWriter)
	dataWriter.Close()
	u := <-unpacking
	switch {
	case err != nil:
		return c.fail("cannot run ssh: " + err.Error())
	case u.err != nil:
		return c.fail(fmt.Sprintf("cannot run on %s: %s", t.host, sshDetail(res.stderr)))
	case u.r.status == "missing":
		return c.fail(fmt.Sprintf("no such file on %s: %s%s", t, strings.Join(splitLines(u.r.stdout), ", "),
			localHomeHint(paths)))
	case u.r.status != "pulled":
		return c.fail(fmt.Sprintf("pull from %s failed: %s", t, strings.TrimSpace(u.r.stderr)))
	case u.bad != nil:
		return c.fail("writing local files: " + u.bad.Error())
	}
	// tar exits 1 for warnings such as a log that grew while being read; 2 and
	// up means some files could not be read at all.
	if warning := strings.TrimSpace(text(res.stderr)); warning != "" || res.code != 0 {
		if res.code >= 2 {
			return c.fail(fmt.Sprintf("some files could not be read on %s: %s", t.host, warning))
		}
		fmt.Fprintf(c.stderr, "warning: %s\n", warning)
	}
	fmt.Fprint(c.stdout, renderTransfer("pulled", u.got, t.String(), u.r.cwd, to))
	return 0
}

// localHomeHint explains a classic mistake: an unquoted ~ that the local shell
// expanded into a local path before sshtab saw it.
func localHomeHint(paths []string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, p := range paths {
		if strings.HasPrefix(p, home+"/") {
			return fmt.Sprintf("\nhint: %s is a local path: your shell turned ~ into %s. "+
				"Quote server paths that start with ~, e.g. '~/data'", p, home)
		}
	}
	return ""
}

func sshDetail(stderr []byte) string {
	if detail := strings.TrimSpace(text(stderr)); detail != "" {
		return detail
	}
	return "no response from remote driver"
}

func busy(t target, r result) string {
	return fmt.Sprintf("%s is busy (%s, %s)", t, r.cmd, formatDuration(r.elapsed))
}

// request runs script on host and parses the reply, reporting failures itself.
func (c *cli) request(host, script string) (result, bool) {
	reply, err := c.call(host, script)
	if err != nil {
		c.fail(fmt.Sprintf("cannot run ssh: %v", err))
		return result{}, false
	}
	r, err := parseReply(reply.stdout)
	if err != nil {
		c.fail(fmt.Sprintf("cannot run on %s: %s", host, sshDetail(reply.stderr)))
		return result{}, false
	}
	return r, true
}

func exitCode(r result) int {
	switch r.status {
	case "exit":
		return r.code
	case "running":
		return exitRunning
	case "stopped":
		return exitStopped
	}
	return 0
}

func (c *cli) usageError(usage, message string) int {
	fmt.Fprintf(c.stderr, "%s\nsshtab: error: %s\n", usage, message)
	return exitError
}

func (c *cli) fail(message string) int {
	fmt.Fprintf(c.stderr, "error: %s\n", message)
	return exitError
}
