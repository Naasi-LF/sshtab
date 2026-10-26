package main

import (
	"bytes"
	"errors"
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

func sshArgs(host, controlDir string) []string {
	return []string{
		// Reuse one connection per host: ~20ms per command instead of a full handshake.
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlDir + "/%C",
		"-o", "ControlPersist=10m",
		// Never prompt for a password: a prompt would hang the agent.
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"--", host,
		// Explicit bash: the user's login shell on the server may be zsh, fish, ...
		"bash -s",
	}
}

// runScript feeds script to bash on host with the system ssh client, so
// ~/.ssh/config just works, and captures its raw output.
func runScript(host, script string) (sshReply, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sshReply{}, err
	}
	controlDir := filepath.Join(home, ".sshtab", "cm")
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		return sshReply{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("ssh", sshArgs(host, controlDir)...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		return sshReply{}, err
	}
	return sshReply{stdout.Bytes(), stderr.Bytes()}, nil
}
