package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/setup"
)

func configure(ctx context.Context, path string, input io.Reader, output io.Writer, accessible bool) error {
	result, err := setup.Run(ctx, path, input, output, accessible)
	if err != nil || !result.Saved {
		return err
	}
	if result.Apply {
		if err := applyConfiguration(ctx, path, output); err != nil {
			return fmt.Errorf("配置已保存，但尚未完成启动和测试：%w\n修正后可运行 %s，再选择保存并启动", err, cliCommand(path, "configure"))
		}
		return nil
	}
	c, err := config.ReadLocal(path)
	if err != nil {
		return err
	}
	if len(c.Enabled()) == 0 {
		fmt.Fprintln(output, "没有启用的通知目的地。需要接收通知时，重新运行", cliCommand(path, "configure"))
		if running, _ := isRunning(c); running && result.Changed {
			fmt.Fprintln(output, "运行中的进程仍使用原配置，停止通知：", cliCommand(path, "stop"))
		}
		return nil
	}
	for _, source := range c.EffectiveSources() {
		if source.Enabled && agents.Inspect(c, source).Installation != "configured_unverified" {
			fmt.Fprintln(output, "安装接入：", cliCommand(path, "install", source.Harness, "--source", source.ID))
		}
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if !running {
		fmt.Fprintln(output, "启动并测试：", cliCommand(path, "test", "--wait"))
	} else if result.Changed {
		fmt.Fprintln(output, "加载新配置：", cliCommand(path, "restart"))
	} else {
		fmt.Fprintln(output, "配置未变化，Blip 已在运行。")
	}
	return nil
}

func applyConfiguration(ctx context.Context, path string, output io.Writer) error {
	c, err := config.Read(path)
	if err != nil {
		return err
	}
	if len(c.Enabled()) == 0 {
		return errors.New("请先启用至少一个通知目的地")
	}
	for _, source := range c.EffectiveSources() {
		if !source.Enabled {
			continue
		}
		// Installation is idempotent and also refreshes a moved/upgraded binary.
		if err := execute(ctx, path, []string{"install", source.Harness, source.ID}, strings.NewReader(""), output); err != nil {
			return fmt.Errorf("接入 %s：%w", source.ID, err)
		}
	}
	if err := manageLifecycle(ctx, c, "restart", output); err != nil {
		return err
	}
	return sendTest(ctx, c, output)
}

// cliCommand produces a copyable command for the current configuration. Single
// quotes protect spaces, $, backticks and Unicode in PowerShell and POSIX shells.
func cliCommand(path string, args ...string) string {
	command := "blip " + strings.Join(args, " ")
	defaultPath, err := config.DefaultPath()
	abs, absErr := filepath.Abs(path)
	if err == nil && absErr == nil {
		same := filepath.Clean(abs) == filepath.Clean(defaultPath)
		if runtime.GOOS == "windows" {
			same = strings.EqualFold(filepath.Clean(abs), filepath.Clean(defaultPath))
		}
		if same {
			return command
		}
	}
	if absErr == nil {
		path = abs
	}
	quote := "'\\''"
	if runtime.GOOS == "windows" {
		quote = "''"
	}
	return command + " --config '" + strings.ReplaceAll(path, "'", quote) + "'"
}

func inferSource(c config.Config, harness string) (string, error) {
	// This shortcut also targets a saved installation after codex-default is
	// removed. Never silently redirect an old command to a different home.
	if harness == "codex" {
		return config.DefaultSourceID, nil
	}
	var matches []string
	for _, source := range c.EffectiveSources() {
		if source.Harness == harness {
			matches = append(matches, source.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("尚未配置 %s 来源；查看 %s", harness, cliCommand(c.Path, "sources", "add", "--help"))
	}
	return "", fmt.Errorf("有多个 %s 来源，请用 --source 选择：%s", harness, strings.Join(matches, ", "))
}
