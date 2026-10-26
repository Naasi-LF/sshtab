package main

// Command-line entry point: parses `sshtab <command> <host[/window]> ...`.

import (
	"errors"
	"fmt"
	"io"
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
	positionals []string // all required
	flags       []flagSpec
	help        string
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
	if len(positionals) > len(s.positionals) {
		return nil, nil, fmt.Errorf("unexpected argument: %q", positionals[len(s.positionals)])
	}
	return positionals, flags, nil
}

// cli runs one sshtab invocation. call talks to the server; tests replace it.
type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	call           func(host, script string) (sshReply, error)
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
		detail := strings.TrimSpace(text(reply.stderr))
		if detail == "" {
			detail = "no response from remote driver"
		}
		c.fail(fmt.Sprintf("cannot run on %s: %s", host, detail))
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
