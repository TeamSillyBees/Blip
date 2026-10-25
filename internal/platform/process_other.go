//go:build !windows

package platform

import "os/exec"

func HideProcess(command *exec.Cmd) {}
