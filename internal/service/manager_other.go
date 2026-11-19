//go:build !windows

package service

import (
	"context"

	"github.com/TeamSillyBees/Blip/internal/config"
)

func Install(context.Context, InstallOptions) error                        { return ErrUnsupported }
func Start(context.Context, string) error                                  { return ErrUnsupported }
func Stop(context.Context, string) error                                   { return ErrUnsupported }
func Uninstall(context.Context, string) error                              { return ErrUnsupported }
func Query(string) (Status, error)                                         { return Status{}, ErrUnsupported }
func UpdateSources(context.Context, string, []config.Source, string) error { return ErrUnsupported }
