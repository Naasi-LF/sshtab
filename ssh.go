package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sshReply is what ssh printed; its exit status does not matter, the driver's
// header (or its absence) says what happened.
type sshReply struct {
	stdout, stderr []byte
}

func sshArgs(host, controlDir, remoteCommand string) []string {
	return []string{
		// Reuse one connection per host: no full handshake per command.
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlDir + "/%C",
		"-o", "ControlPersist=10m",
		// Never prompt for a password: a prompt would hang the agent.
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"--", host,
		remoteCommand,
	}
}

// sshCommand prepares ssh to run remoteCommand on host with the system ssh
// client, so ~/.ssh/config just works.
func sshCommand(host, remoteCommand string) (*exec.Cmd, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	controlDir := filepath.Join(home, ".sshtab", "cm")
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		return nil, err
	}
	return exec.Command("ssh", sshArgs(host, controlDir, remoteCommand)...), nil
}

// runScript feeds script to bash on host and captures its raw output.
func runScript(host, script string) (sshReply, error) {
	// Explicit bash: the user's login shell on the server may be zsh, fish, ...
	cmd, err := sshCommand(host, "bash -s")
	if err != nil {
		return sshReply{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		return sshReply{}, err
	}
	return sshReply{stdout.Bytes(), stderr.Bytes()}, nil
}

// streamResult is how a streamed call ended on the server side.
type streamResult struct {
	stderr []byte
	code   int // the remote script's exit status
}

// streamScript runs script with bash on host, passing it on the command line
// (base64, inside single quotes that any login shell leaves alone) so that
// ssh's stdin and stdout carry file data. Scripts sent this way must stay well
// under the 128KB limit on one argument: no user commands.
func streamScript(host, script string, stdin io.Reader, stdout io.Writer) (streamResult, error) {
	remote := `bash -c 'eval "$(printf %s "$1" | base64 -d)"' sshtab ` + encodeCommand(script)
	cmd, err := sshCommand(host, remote)
	if err != nil {
		return streamResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return streamResult{}, err
	}
	return streamResult{stderr.Bytes(), cmd.ProcessState.ExitCode()}, nil
}
