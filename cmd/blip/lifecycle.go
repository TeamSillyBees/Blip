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

func manageLifecycle(ctx context.Context, path, action string, output io.Writer) error {
	m := lifecycle{query: service.Query, startService: service.Start, stopService: service.Stop, startWorker: start, stopWorker: stop}
	return m.run(ctx, path, action, output)
}

func (m lifecycle) run(ctx context.Context, path, action string, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("未知的进程管理操作 %q", action)
	}
	status, err := m.query(path)
	if err != nil && !errors.Is(err, service.ErrUnsupported) {
		return err
	}
	// SCM can stop a registered service even when its config needs repair.
	// Validate before starting or interrupting a working service on restart.
	var c config.Config
	if action != "stop" {
		c, err = config.ReadWithDefaults(path, status.Defaults)
	} else if !status.Installed {
		c, err = config.ReadLocal(path)
	}
	if err != nil {
		return err
	}
	if status.Installed {
		if action == "stop" || action == "restart" {
			if err := m.stopService(ctx, path); err != nil {
				return err
			}
		}
		if action == "start" || action == "restart" {
			if err := m.startService(ctx, path); err != nil {
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
			if action == "stop" && errors.Is(err, errStopPending) {
				fmt.Fprintln(output, "已请求停止，通知进程正在完成当前操作。")
				return nil
			}
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
