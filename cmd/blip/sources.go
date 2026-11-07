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
	parent := &cobra.Command{Use: "sources", Short: "查看或管理 agent 来源（终端默认列出）", Args: cobra.NoArgs, RunE: func(c *cobra.Command, args []string) error {
		if !setup.CanPrompt(c.InOrStdin(), c.OutOrStdout()) || *path == "" {
			return c.Help()
		}
		return list(c, args)
	}}
	parent.AddCommand(&cobra.Command{Use: "list", Short: "List configured sources", Args: cobra.NoArgs, RunE: list})
	for _, action := range []string{"migrate", "add", "enable", "disable", "remove", "event-version"} {
		var id, harness, home string
		var eventVersion int
		args := cobra.NoArgs
		use := action
		if action == "event-version" {
			args = cobra.ExactArgs(1)
			use += " <1|2>"
		}
		command := &cobra.Command{Use: use, Short: action + " source configuration", Args: args, RunE: func(command *cobra.Command, args []string) error {
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
					return fmt.Errorf("source %q already exists", id)
				}
				if !config.ValidSourceID(id) || harness == "" || home == "" {
					return fmt.Errorf("add requires --source, --harness and --home")
				}
				if _, exists := agents.Builtins()[harness]; !exists {
					return fmt.Errorf("%w: %s", agents.ErrUnavailable, harness)
				}
				home, err = filepath.Abs(home)
				if err != nil {
					return err
				}
				doc.Config.Sources = append(doc.Config.Sources, config.Source{ID: id, Harness: harness, Home: home, Enabled: true, EventVersion: eventVersion})
			case "enable", "disable", "remove", "event-version":
				if index < 0 {
					return fmt.Errorf("source %q is not configured", id)
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
						return fmt.Errorf("event version must be 1 or 2")
					}
					doc.Config.Sources[index].EventVersion = value
				}
			}
			if action == "add" || action == "event-version" {
				fmt.Fprintln(command.OutOrStdout(), "Event v2 sources send source_id and may send attention_required to every enabled webhook. Receivers must handle schema_version 2.")
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
			command.Flags().StringVar(&id, "source", "", "stable source ID")
			_ = command.MarkFlagRequired("source")
		}
		if action == "add" {
			command.Flags().StringVar(&harness, "harness", "", "harness adapter")
			command.Flags().StringVar(&home, "home", "", "harness configuration directory")
			command.Flags().IntVar(&eventVersion, "event-version", 2, "event protocol version")
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
		return fmt.Errorf("source %q is not configured", id)
	}
	return json.NewEncoder(output).Encode(map[string]any{"sources": statuses})
}
