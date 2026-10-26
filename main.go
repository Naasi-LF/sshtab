// Command sshtab is a remote shell for AI agents: terminal-like windows on a
// Linux server over SSH, with nothing to install on the server.
package main

import "os"

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	c := &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, call: runScript}
	os.Exit(c.main(os.Args[1:]))
}
