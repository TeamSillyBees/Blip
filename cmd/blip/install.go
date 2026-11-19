package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
)

type installTarget struct {
	command, harness, defaultID, homeEnv, homeDir, label string
}

var installTargets = []installTarget{
	{"codex", "codex", config.DefaultSourceID, "CODEX_HOME", ".codex", "Codex CLI 和桌面版"},
	{"claude-code", "claude-code", "claude-code", "CLAUDE_CONFIG_DIR", ".claude", "Claude Code"},
	{"hermes", "hermes", "hermes", "HERMES_HOME", ".hermes", "Hermes"},
	{"dsh", "dsh", "dsh-desktop", "", ".dsh/profiles/desktop", "DeepSeek Harness"},
	{"antigravity", "antigravity", "antigravity-app", "", ".gemini/antigravity", "Antigravity 桌面版"},
	{"agy", "antigravity", "agy", "", ".gemini/antigravity-cli", "agy CLI"},
}

func newInstallCommand(path *string) *cobra.Command {
	parent := &cobra.Command{Use: "install", Short: "安装编程助手接入并启动监听（Windows 自动注册或更新服务）", Args: noArgs,
		Long: "选择要监听的编程助手，例如 blip install codex。Windows 请在当前账户的管理员终端运行，服务将在开机时自动启动。",
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var sourceID, home string
	var registerOnly bool
	parent.PersistentFlags().StringVar(&sourceID, "source", "", "选择来源 ID，搭配 --home 可创建命名来源")
	parent.PersistentFlags().StringVar(&home, "home", "", "使用自定义客户端数据目录")
	parent.PersistentFlags().BoolVar(&registerOnly, "register-only", false, "仅登记来源并安装接入，跳过后台服务管理")
	for _, target := range installTargets {
		parent.AddCommand(&cobra.Command{Use: target.command, Short: "启用 " + target.label + " 监听", Args: noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return target.install(cmd.Context(), *path, sourceID, home, registerOnly, cmd.OutOrStdout())
			},
		})
	}
	return parent
}

// Hold the same lock through activation so concurrent installs cannot publish
// an older service binary or start a worker with an older source list.
func withInstallLock(ctx context.Context, path string, apply func(service.Status) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock := flock.New(path + ".install.lock")
	defer lock.Close()
	ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("未能取得来源安装锁")
	}
	status, err := service.Query(path)
	if err != nil && !errors.Is(err, service.ErrUnsupported) {
		return err
	}
	return apply(status)
}

func (target installTarget) install(ctx context.Context, path, id, home string, registerOnly bool, out io.Writer) error {
	return withInstallLock(ctx, path, func(status service.Status) error {
		doc, err := config.OpenDocument(path)
		if err != nil {
			return err
		}
		defaultCodex := target.harness == "codex" && home == "" && (id == "" || id == config.DefaultSourceID)
		if !doc.Exists() && !defaultCodex {
			// A new install opts in to just the requested client. Existing v1
			// configurations keep the Codex source they already use.
			doc.Config.SchemaVersion = 2
			doc.Config.Sources = []config.Source{}
		}
		c, err := doc.RuntimeConfig(status.Defaults)
		if err != nil {
			return err
		}
		source, hasDefault := c.Source(config.DefaultSourceID)
		if defaultCodex && !hasDefault {
			// A removed default retains its installation and permanent binding.
			// Require its home explicitly before restoring that registration.
			if previous, err := agents.InstalledSource(c, config.DefaultSourceID); err == nil {
				return fmt.Errorf("来源 %s 已从配置中移除，要重新接入请用 --source %s 并用 --home 指定原目录 %q", previous.ID, previous.ID, previous.Home)
			} else if errors.Is(err, agents.ErrBinding) {
				return fmt.Errorf("检查来源 %s：%w", config.DefaultSourceID, err)
			}
		}
		// A regular Codex install can keep a v1 config and its hook identity.
		if c.SchemaVersion != 1 || !defaultCodex {
			if c.SchemaVersion == 1 {
				if err := agents.Bind(ctx, c, source); err != nil {
					return err
				}
			}
			if err := doc.MigrateSources(status.Defaults); err != nil {
				return err
			}
			source, err = target.enableSource(&doc.Config, id, home)
			if err != nil {
				return err
			}
			c, err = doc.RuntimeConfig(status.Defaults)
			if err != nil {
				return err
			}
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := bindInstallSource(ctx, c, source); err != nil {
			return err
		}
		if err := doc.Save(ctx); err != nil {
			return err
		}
		if err := installClientHook(ctx, c, source, exe, out); err != nil {
			return fmt.Errorf("来源 %s 已保存，客户端接入尚未完成：%w", source.ID, err)
		}
		fmt.Fprintf(out, "已启用 %s（%s）。\n", source.ID, source.Home)
		if !registerOnly {
			if err := activateInstallation(ctx, c, []config.Source{source}, status, exe, out); err != nil {
				return err
			}
		}
		if len(c.Enabled()) == 0 {
			fmt.Fprintln(out, "尚无已启用的通知渠道。接收提醒前请运行", cliCommand(path, "configure"), "并选择“保存并启动”。")
		}
		return nil
	})
}

func (target installTarget) enableSource(c *config.Config, id, home string) (config.Source, error) {
	// Keep the previous unique-source shortcut for named hook installations.
	// Antigravity selects by client directory since both clients use one harness.
	if id == "" && home == "" && target.harness != "antigravity" {
		for _, s := range c.Sources {
			if s.Harness != target.harness || (s.Harness == "codex" && s.ID != config.DefaultSourceID) {
				continue
			}
			var err error
			id, err = inferSource(*c, target.harness)
			if err != nil {
				return config.Source{}, err
			}
			break
		}
	}
	if id != "" && home == "" {
		for i, s := range c.Sources {
			if s.ID == id && s.Harness == target.harness {
				c.Sources[i].Enabled = true
				return c.Sources[i].Resolved(c.Path), nil
			}
		}
		return config.Source{}, fmt.Errorf("没有 ID 为 %q 的 %s 来源，创建命名来源时请同时指定 --home", id, target.harness)
	}
	if target.harness == "codex" && id == "" && home == "" {
		id = config.DefaultSourceID
	}
	if home == "" && target.homeEnv != "" {
		home = os.Getenv(target.homeEnv)
	}
	if home == "" {
		profile, err := os.UserHomeDir()
		if err != nil {
			return config.Source{}, err
		}
		home = filepath.Join(profile, target.homeDir)
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return config.Source{}, err
	}
	canonical, err := config.CanonicalHome(home)
	if err != nil {
		return config.Source{}, err
	}
	for i, s := range c.Sources {
		if s.Harness == target.harness {
			existing, err := config.CanonicalHome(s.Resolved(c.Path).Home)
			if err == nil && existing == canonical {
				if id != "" && id != s.ID {
					return config.Source{}, fmt.Errorf("目录已登记为来源 %q", s.ID)
				}
				c.Sources[i].Enabled = true
				return c.Sources[i].Resolved(c.Path), nil
			}
		}
	}
	if id == "" {
		id = target.defaultID
	}
	for _, s := range c.Sources {
		if strings.EqualFold(s.ID, id) {
			return config.Source{}, fmt.Errorf("来源 %q 已绑定到 %s，请搭配 --source 和 --home 为新目录命名", id, s.Home)
		}
	}
	source := config.Source{ID: id, Harness: target.harness, Home: home, Enabled: true, EventVersion: 2}
	c.Sources = append(c.Sources, source)
	return source, nil
}

func bindInstallSource(ctx context.Context, c config.Config, s config.Source) error {
	if s.Harness == "dsh" {
		if err := agents.CheckDSHProfile(s.Home); err != nil {
			return err
		}
	}
	return agents.Bind(ctx, c, s)
}

func installClientHook(ctx context.Context, c config.Config, s config.Source, exe string, out io.Writer) error {
	adapter, err := agents.Builtins().Open(c, s)
	if err != nil {
		return err
	}
	if installer, ok := adapter.(agents.Installer); ok {
		if err := installer.Install(ctx, exe); err != nil {
			return fmt.Errorf("安装 %s 接入（%s）：%w", s.Harness, s.Home, err)
		}
		fmt.Fprintf(out, "%s 接入已安装（%s）。首次安装或定义变化后，请在客户端中审阅并信任。\n", s.Harness, s.ID)
	}
	return nil
}

func activateInstallation(ctx context.Context, c config.Config, sources []config.Source, status service.Status, exe string, out io.Writer) error {
	if runtime.GOOS != "windows" {
		return manageLifecycle(ctx, c.Path, "restart", out)
	}
	if status.Installed {
		if err := service.UpdateSources(ctx, c.Path, sources, exe); err != nil {
			return fmt.Errorf("更新 Windows 服务失败，请在当前账户的管理员终端重试：%w", err)
		}
	} else {
		opts := service.InstallOptions{ConfigPath: c.Path, Executable: exe, CodexHome: c.CodexHome, StateDir: c.StateDir, Sources: c.Sources}
		// Register before stopping an existing per-user worker. A denied SCM
		// operation must not interrupt a working installation.
		if err := service.Install(ctx, opts); err != nil {
			return fmt.Errorf("注册 Windows 服务失败，请在当前账户的管理员终端重试：%w", err)
		}
	}
	if err := stop(ctx, c, io.Discard); err != nil {
		return fmt.Errorf("切换到 Windows 服务前停止普通进程：%w", err)
	}
	if err := service.Start(ctx, c.Path); err != nil {
		return fmt.Errorf("Windows 服务已注册，启动失败：%w", err)
	}
	fmt.Fprintln(out, "Windows 服务已启动并加载监听配置。")
	for _, s := range sources {
		if s.Harness == "antigravity" {
			if _, err := os.Stat(filepath.Join(s.Home, antigravity.SummaryFilename)); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(out, "%s 尚未创建会话数据库。请先打开一次会话，再执行对应的 install 命令刷新服务读取权限。\n", s.ID)
			}
		}
	}
	return nil
}
