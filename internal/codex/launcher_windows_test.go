//go:build windows

package codex

import (
	"os"
	"os/exec"
	"syscall"
)

func launcherProcess(line string) *exec.Cmd {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	command := exec.Command(shell)
	command.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /D /C "` + line + `"`, HideWindow: true}
	return command
}
