package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// Keep lifecycle selection in one place: a registered service must never be
// replaced by a second, unmanaged worker when applying configuration.
type lifecycle struct {
	query        func(string) (service.Status, error)
	startService func(context.Context, string) error
	stopService  func(context.Context, string) error
	startWorker  func(context.Context, config.Config, io.Writer) error
	stopWorker   func(context.Context, config.Config, io.Writer) error
}

var errStopPending = errors.New("通知进程仍在停止中")

func manageLifecycle(ctx context.Context, c config.Config, action string, output io.Writer) error {
	m := lifecycle{query: service.Query, startService: service.Start, stopService: service.Stop, startWorker: start, stopWorker: stop}
	return m.run(ctx, c, action, output)
}

func (m lifecycle) run(ctx context.Context, c config.Config, action string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("未知的进程管理操作 %q", action)
	}
	status, err := m.query(c.Path)
	if err != nil && !errors.Is(err, service.ErrUnsupported) {
		return err
	}
	if status.Installed {
		if action == "stop" || action == "restart" {
			if err := m.stopService(ctx, c.Path); err != nil {
				return err
			}
		}
		if action == "start" || action == "restart" {
			if err := m.startService(ctx, c.Path); err != nil {
				return err
			}
		}
		fmt.Fprintf(output, "Windows 服务%s。\n", map[string]string{"start": "已启动", "stop": "已停止", "restart": "已重启"}[action])
		return nil
	}
	if action == "stop" || action == "restart" {
		stopOutput := output
		if action == "restart" {
			stopOutput = io.Discard
		}
		if err := m.stopWorker(ctx, c, stopOutput); err != nil {
			return err
		}
	}
	if action == "start" || action == "restart" {
		return m.startWorker(ctx, c, output)
	}
	return nil
}

func stop(ctx context.Context, c config.Config, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if !running {
		fmt.Fprintln(output, "Blip 已停止。")
		return nil
	}
	if err := store.WriteFile(filepath.Join(c.StateDir, "stop"), []byte("stop\n")); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, 10*time.Second, errStopPending)
	defer cancel()
	for {
		running, err = isRunning(c)
		if err != nil {
			return err
		}
		if !running {
			fmt.Fprintln(output, "Blip 已停止。")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("已请求停止，但通知进程尚未退出；运行 %s 查看状态：%w", cliCommand(c.Path, "status"), context.Cause(ctx))
		case <-time.After(100 * time.Millisecond):
		}
	}
}
