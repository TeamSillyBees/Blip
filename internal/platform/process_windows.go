//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

func HideProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | 0x00000200}
}
