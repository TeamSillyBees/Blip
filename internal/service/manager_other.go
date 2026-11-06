//go:build !windows

package service

import "context"

func Install(context.Context, InstallOptions) error { return ErrUnsupported }
func Start(context.Context, string) error           { return ErrUnsupported }
func Stop(context.Context, string) error            { return ErrUnsupported }
func Uninstall(context.Context, string) error       { return ErrUnsupported }
func Query(string) (Status, error)                  { return Status{}, ErrUnsupported }
