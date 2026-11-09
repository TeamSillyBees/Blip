package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/worker"
	"github.com/spf13/cobra"
)

func newServiceCommand(path *string) *cobra.Command {
	command := &cobra.Command{
		Use: "service", Short: "管理 Windows 服务和开机自启",
		Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	for _, entry := range []struct{ name, description string }{
		{"install", "注册开机自动延迟启动的 Windows 服务（需要管理员权限）"},
		{"uninstall", "停止并移除 Windows 服务，保留配置和通知记录"},
		{"start", "启动已注册的 Windows 服务"},
		{"stop", "平稳停止 Windows 服务"},
		{"restart", "重启 Windows 服务并应用最新配置"},
		{"status", "查看 Windows 服务的注册信息和运行状态"},
	} {
		command.AddCommand(&cobra.Command{
			Use: entry.name, Short: entry.description, Args: noArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				return manageService(command.Context(), command.Name(), *path, command.OutOrStdout())
			},
		})
	}
	var name string
	var defaults config.RuntimeDefaults
	run := &cobra.Command{
		Use: "run", Short: "作为 Windows 服务运行通知进程", Hidden: true, Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return service.Run(name, func(ctx context.Context, ready func()) error {
				return runServiceWorker(ctx, *path, defaults, ready)
			})
		},
	}
	run.Flags().StringVar(&name, "name", "", "注册时使用的服务名称")
	run.Flags().StringVar(&defaults.CodexHome, "default-codex-home", "", "注册时保存的 Codex 配置目录")
	run.Flags().StringVar(&defaults.StateDir, "default-state-dir", "", "注册时保存的状态目录")
	for _, flag := range []string{"name", "default-codex-home", "default-state-dir"} {
		_ = run.MarkFlagRequired(flag)
	}
	command.AddCommand(run)
	return command
}

func manageService(ctx context.Context, action, path string, output io.Writer) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("Windows 服务仅支持在 Windows 系统上使用")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	switch action {
	case "install":
		c, err := config.Read(absolute)
		if err != nil {
			return err
		}
		if len(c.Enabled()) == 0 {
			return fmt.Errorf("安装服务前，请先配置并启用至少一个通知目的地：%s", cliCommand(path, "configure"))
		}
		if running, err := isRunning(c); err != nil {
			return err
		} else if running {
			return fmt.Errorf("通知进程已在运行；安装服务前请先运行 %s", cliCommand(path, "stop"))
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err = service.Install(ctx, service.InstallOptions{ConfigPath: c.Path, Executable: executable, CodexHome: c.CodexHome, StateDir: c.StateDir, Sources: c.Sources}); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows 服务已安装：", service.Name(absolute))
		fmt.Fprintln(output, "已启用开机自动延迟启动。立即启动：", cliCommand(path, "service", "start"))
	case "start":
		if err = service.Start(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows 服务已启动。")
	case "stop":
		if err = service.Stop(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows 服务已停止。")
	case "restart":
		c, readErr := config.Read(absolute)
		if readErr != nil {
			return readErr
		}
		if len(c.Enabled()) == 0 {
			return fmt.Errorf("请先配置并启用至少一个通知目的地：%s", cliCommand(path, "configure"))
		}
		if err = service.Stop(ctx, absolute); err != nil {
			return err
		}
		if err = service.Start(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows 服务已重启。")
	case "uninstall":
		if err = service.Uninstall(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows 服务已移除，配置和通知记录已保留。")
	case "status":
		status, err := service.Query(absolute)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(status)
	default:
		return fmt.Errorf("未知的服务管理操作")
	}
	return nil
}

// Log startup failures in the registered state directory before opening the
// configuration: a malformed file must still leave a useful service diagnostic.
func runServiceWorker(ctx context.Context, path string, defaults config.RuntimeDefaults, ready func()) error {
	if !filepath.IsAbs(path) || !filepath.IsAbs(defaults.StateDir) || (defaults.CodexHome != "" && !filepath.IsAbs(defaults.CodexHome)) {
		return fmt.Errorf("服务配置文件和默认目录必须使用绝对路径")
	}
	if err := os.MkdirAll(defaults.StateDir, 0700); err != nil {
		return fmt.Errorf("打开服务状态目录失败：%w", err)
	}
	file, err := os.OpenFile(filepath.Join(defaults.StateDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("打开服务日志失败：%w", err)
	}
	defer file.Close()
	logger := log.New(file, "Blip 服务 ", log.LstdFlags)
	c, err := config.ReadWithDefaults(path, defaults)
	if err != nil {
		// JSON decoder errors can repeat unknown field names supplied by the
		// file. Keep potentially sensitive configuration text out of the log.
		logger.Print("无法加载配置；请使用此配置文件运行 blip run 查看错误")
		return err
	}
	if c.SchemaVersion == 1 && defaults.CodexHome == "" {
		return fmt.Errorf("旧版服务配置需要原来的 Codex 配置目录；请重新安装服务")
	}
	if len(c.Enabled()) == 0 {
		err = fmt.Errorf("请先启用至少一个通知目的地")
		logger.Print(err)
		return err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		logger.Printf("创建通知进程状态目录失败：%v", err)
		return err
	}
	workerFile, err := os.OpenFile(filepath.Join(c.StateDir, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logger.Printf("打开通知进程日志失败：%v", err)
		return err
	}
	defer workerFile.Close()
	err = worker.RunWithReady(ctx, c, log.New(workerFile, "Blip ", log.LstdFlags), ready)
	if err != nil {
		logger.Printf("通知进程退出：%v", err)
	}
	return err
}
