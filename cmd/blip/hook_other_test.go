//go:build !windows

package main

import "os/exec"

func hookProcess(line string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", line)
}
