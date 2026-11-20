package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/spf13/pflag"
)

func TestChineseHelpAcrossCommandTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	commands := []string{"", "help", "configure", "config", "setup", "init", "start", "stop", "restart", "run", "status", "test", "doctor", "hook", "version", "sources", "service", "completion"}
	for _, action := range []string{"install", "uninstall"} {
		commands = append(commands, action)
		for _, harness := range agents.Builtins().InstallableHarnesses() {
			commands = append(commands, action+" "+harness)
		}
	}
	commands = append(commands, "install agy", "uninstall agy")
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart", "status", "run"} {
		commands = append(commands, "service "+action)
	}
	for _, action := range []string{"list", "migrate", "add", "enable", "disable", "remove", "event-version"} {
		commands = append(commands, "sources "+action)
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		commands = append(commands, "completion "+shell)
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			args := append(strings.Fields(command), "--help", "--config", path)
			if err := run(args, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"用法：", "选项：", "查看帮助信息", "配置文件路径"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("help is missing %q: %s", want, output.String())
				}
			}
			for _, english := range []string{"Usage:", "Available Commands:", "Flags:", "Aliases:", "help for", "version for", "[flags]", "[command]", "(default ", "Generate the", "Show the", "Manage the", "source configuration", "agent integration"} {
				if strings.Contains(output.String(), english) {
					t.Errorf("English help fragment %q: %s", english, output.String())
				}
			}
			if command == "" {
				for _, want := range []string{"常用操作：", "接入与维护：", "安装编程助手接入", "管理 Windows 服务和开机自启", "显示版本号"} {
					if !strings.Contains(output.String(), want) {
						t.Errorf("root help is missing %q", want)
					}
				}
				common, _, _ := strings.Cut(output.String(), "接入与维护：")
				for _, name := range []string{"install", "start", "stop", "restart"} {
					if !strings.Contains(common, "\n  "+name+" ") {
						t.Errorf("%s is missing from common operations: %s", name, common)
					}
				}
			}
			if command != "help" {
				var topic bytes.Buffer
				if err := run(append([]string{"--config", path, "help"}, strings.Fields(command)...), strings.NewReader(""), &topic); err != nil {
					t.Fatal(err)
				}
				if topic.String() != output.String() {
					t.Errorf("help and --help differ:\n%s\n%s", topic.String(), output.String())
				}
			}
		})
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("help wrote a configuration")
	}
}

func TestChineseFlagHelpPreservesDefaultsAndValues(t *testing.T) {
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	path := `C:\中文目录\[flags] (default string)\config.json`
	flags.String("config", path, "配置文件路径")
	flags.String("source", "0", "来源 ID")
	flags.Int("event-version", 2, "事件协议版本")
	flags.BoolP("help", "h", false, "查看帮助信息")
	flags.String("hidden", "secret", "内部选项")
	if err := flags.MarkHidden("hidden"); err != nil {
		t.Fatal(err)
	}
	before := flags.FlagUsages()
	output := chineseFlagUsages(flags)
	for _, want := range []string{fmt.Sprintf("默认值：%q", path), `默认值："0"`, "默认值：2", "--config 字符串", "--event-version 整数", "-h, --help"} {
		if !strings.Contains(output, want) {
			t.Errorf("help changed a value or label %q: %s", want, output)
		}
	}
	if strings.Contains(output, "secret") || flags.FlagUsages() != before || flags.Lookup("config").Value.String() != path {
		t.Fatalf("help exposed a hidden flag or changed flag metadata: %s", output)
	}
}

func TestLocalizedCompletionCommandsStillGenerateScripts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for shell, marker := range map[string]string{"bash": "__start_blip", "zsh": "#compdef blip", "fish": "complete -c blip", "powershell": "Register-ArgumentCompleter"} {
		for _, noDescriptions := range []bool{false, true} {
			args := []string{"--config", path, "completion", shell}
			if noDescriptions {
				args = append(args, "--no-descriptions")
			}
			var output bytes.Buffer
			if err := run(args, strings.NewReader(""), &output); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if !strings.Contains(output.String(), marker) || strings.Contains(output.String(), "用法：") {
				t.Fatalf("%v did not generate a shell script: %s", args, output.String())
			}
		}
	}
}

func TestChineseArgumentErrorsBeforeConfigurationAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "不接受参数或子命令"},
		{[]string{"help", "unknown"}, "找不到帮助主题"},
		{[]string{"service", "install", "extra"}, "不接受参数或子命令"},
		{[]string{"--unknown"}, "未知选项：--unknown"},
		{[]string{"-x"}, "未知选项：-x"},
		{[]string{"--config"}, "需要一个值"},
		{[]string{"status", "--json=invalid"}, "请使用 true 或 false"},
		{[]string{"sources", "add", "--event-version=invalid"}, "整数"},
		{[]string{"sources", "add"}, "缺少必填选项：--harness、--home、--source"},
		{[]string{"sources", "event-version"}, "请指定一个事件协议版本"},
	} {
		err := run(append([]string{"--config", path}, test.args...), strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "--help") && test.args[0] != "help" {
			t.Errorf("%v: want %q and help guidance, got %v", test.args, test.want, err)
		}
		if len(test.args) == 1 && test.args[0] == "--unknown" {
			var unknown *pflag.NotExistError
			if !errors.As(err, &unknown) {
				t.Errorf("localized error lost its cause: %v", err)
			}
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid arguments wrote a configuration")
	}
}
