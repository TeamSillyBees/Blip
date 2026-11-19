package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/setup"
	"github.com/spf13/cobra"
)

func newSourcesCommand(path *string) *cobra.Command {
	list := func(c *cobra.Command, _ []string) error {
		cfg, err := config.ReadLocal(*path)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(cfg.EffectiveSources())
	}
	parent := &cobra.Command{Use: "sources", Short: "查看或管理编程助手来源（终端默认列出）", Args: noArgs, RunE: func(c *cobra.Command, args []string) error {
		if !setup.CanPrompt(c.InOrStdin(), c.OutOrStdout()) || *path == "" {
			return c.Help()
		}
		return list(c, args)
	}}
	parent.AddCommand(&cobra.Command{Use: "list", Short: "列出已配置的来源", Args: noArgs, RunE: list})
	for _, entry := range []struct{ action, description string }{
		{"migrate", "将旧版配置迁移为多来源配置"},
		{"add", "添加编程助手来源"},
		{"enable", "启用指定来源"},
		{"disable", "停用指定来源"},
		{"remove", "移除指定来源的配置"},
		{"event-version", "设置指定来源的事件协议版本"},
	} {
		action := entry.action
		var id, harness, home string
		var eventVersion int
		args := noArgs
		use := action
		if action == "event-version" {
			args = oneEventVersionArg
			use += " <1|2>"
		}
		command := &cobra.Command{Use: use, Short: entry.description, Args: args, RunE: func(command *cobra.Command, args []string) error {
			doc, err := config.OpenDocument(*path)
			if err != nil {
				return err
			}
			if err = doc.MigrateSources(); err != nil {
				return err
			}
			index := -1
			for i, s := range doc.Config.Sources {
				if s.ID == id {
					index = i
					break
				}
			}
			switch action {
			case "migrate":
			case "add":
				if index >= 0 {
					return fmt.Errorf("来源 %q 已存在", id)
				}
				if !config.ValidSourceID(id) || harness == "" || home == "" {
					return fmt.Errorf("添加来源时必须提供有效的 --source、--harness 和 --home")
				}
				if _, exists := agents.Builtins()[harness]; !exists {
					return &localizedError{message: "不支持的编程助手类型：" + harness, cause: agents.ErrUnavailable}
				}
				home, err = filepath.Abs(home)
				if err != nil {
					return err
				}
				doc.Config.Sources = append(doc.Config.Sources, config.Source{ID: id, Harness: harness, Home: home, Enabled: true, EventVersion: eventVersion})
			case "enable", "disable", "remove", "event-version":
				if index < 0 {
					return fmt.Errorf("尚未配置来源 %q", id)
				}
				switch action {
				case "enable":
					doc.Config.Sources[index].Enabled = true
				case "disable":
					doc.Config.Sources[index].Enabled = false
				case "remove":
					doc.Config.Sources = append(doc.Config.Sources[:index], doc.Config.Sources[index+1:]...)
				case "event-version":
					value, parseErr := strconv.Atoi(args[0])
					if parseErr != nil || (value != 1 && value != 2) {
						return fmt.Errorf("事件协议版本必须为 1 或 2")
					}
					doc.Config.Sources[index].EventVersion = value
				}
			}
			if action == "add" || action == "event-version" {
				fmt.Fprintln(command.OutOrStdout(), "事件协议第 2 版会向所有已启用的 Webhook 发送 source_id，也可能发送 attention_required 事件。接收端须支持 schema_version 为 2 的消息。")
			}
			if err = doc.Validate(); err != nil {
				return err
			}
			current, err := config.ReadLocal(*path)
			if err != nil {
				return err
			}
			// Reserve old IDs before removal and new IDs before publication.
			// A failed/stale save may leave a reservation, never a reused ID.
			for _, source := range append(current.EffectiveSources(), doc.Config.Sources...) {
				if err = agents.Bind(command.Context(), current, source.Resolved(current.Path)); err != nil {
					return err
				}
			}
			if err = doc.Save(command.Context()); err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), "来源配置已保存。加载变更：", cliCommand(*path, "restart"))
			fmt.Fprintln(command.OutOrStdout(), "Windows 服务的来源读取目录发生变化时，仍需重新安装服务。")
			return nil
		}}
		if action != "migrate" {
			command.Flags().StringVar(&id, "source", "", "固定的来源 ID（必填）")
			_ = command.MarkFlagRequired("source")
		}
		if action == "add" {
			command.Flags().StringVar(&harness, "harness", "", "编程助手类型：antigravity（含 agy CLI）、codex、claude-code、dsh 或 hermes（必填）")
			command.Flags().StringVar(&home, "home", "", "编程助手配置目录（必填）")
			command.Flags().IntVar(&eventVersion, "event-version", 2, "事件协议版本：1 或 2")
			_ = command.MarkFlagRequired("harness")
			_ = command.MarkFlagRequired("home")
		}
		parent.AddCommand(command)
	}
	return parent
}

func printDoctor(c config.Config, id string, output io.Writer) error {
	statuses := []agents.SourceStatus{}
	for _, source := range c.EffectiveSources() {
		if id == "" || source.ID == id {
			statuses = append(statuses, agents.Inspect(c, source))
		}
	}
	if id != "" && len(statuses) == 0 {
		return fmt.Errorf("尚未配置来源 %q", id)
	}
	return json.NewEncoder(output).Encode(map[string]any{"sources": statuses})
}
