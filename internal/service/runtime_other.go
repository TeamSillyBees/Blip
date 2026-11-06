//go:build !windows

package service

import (
	"context"
	"fmt"
)

func Run(string, func(context.Context, func()) error) error {
	return fmt.Errorf("Windows services are only available on Windows")
}
