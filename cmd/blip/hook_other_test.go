//go:build !windows

package main

import "os/exec"

func hookProcess(binary, path, line string) *exec.Cmd {
	return exec.Command(binary, "--config", path, "hook")
}
