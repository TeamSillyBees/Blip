//go:build !windows

package codex

import "os/exec"

func launcherProcess(line string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", line)
}
