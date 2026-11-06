package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/setup"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
	"github.com/spf13/cobra"
)

var version = "0.2.0-dev.5"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Blip:", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	defaultPath, defaultPathErr := config.DefaultPath()
	path := defaultPath
	root := &cobra.Command{
		Use:           "blip",
		Short:         "Send agent handoff notifications to your messaging channels",
		Long:          "Blip captures agent completion, approval and input events locally.\nUse blip configure to set up notification channels interactively.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE:          func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	root.SetIn(input)
	root.SetOut(output)
	root.SetErr(output)
	root.SetArgs(args)
	root.PersistentFlags().StringVar(&path, "config", defaultPath, "configuration path")
	root.PersistentPreRunE = func(command *cobra.Command, _ []string) error {
		if path != "" || command == root || command.Name() == "version" || command.HasSubCommands() {
			return nil
		}
		for parent := command; parent != nil; parent = parent.Parent() {
			if parent.Name() == "completion" {
				return nil
			}
		}
		if defaultPathErr != nil {
			return fmt.Errorf("default configuration path is unavailable; specify --config: %w", defaultPathErr)
		}
		return fmt.Errorf("configuration path must not be empty")
	}
	root.AddCommand(newServiceCommand(&path))
	root.AddCommand(newSourcesCommand(&path))
	root.SetVersionTemplate("{{.Version}}\n")
	for _, entry := range []struct{ name, description string }{
		{"init", "Create a configuration with disabled example destinations"},
		{"start", "Start a hidden notification worker"},
		{"stop", "Request a graceful worker stop"},
		{"run", "Run the notification worker in this terminal"},
		{"status", "Show readiness and per-destination delivery results"},
		{"test", "Queue a test notification for enabled destinations"},
		{"doctor", "Inspect source installation, capabilities and recent callbacks"},
		{"hook", "Capture a harness hook event from standard input"},
	} {
		var sourceID string
		command := &cobra.Command{
			Use: entry.name, Short: entry.description, Args: cobra.NoArgs, Hidden: entry.name == "hook",
			RunE: func(command *cobra.Command, _ []string) error {
				args := []string{command.Name()}
				if sourceID != "" {
					args = append(args, sourceID)
				}
				return execute(command.Context(), path, args, command.InOrStdin(), command.OutOrStdout())
			},
		}
		if entry.name == "hook" || entry.name == "doctor" {
			command.Flags().StringVar(&sourceID, "source", "", "source ID (hook defaults to codex-default)")
		}
		root.AddCommand(command)
	}
	for _, action := range []string{"install", "uninstall"} {
		parent := &cobra.Command{Use: action, Short: action + " agent integration", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
		}
		for harness := range agents.Builtins() {
			var sourceID string
			child := &cobra.Command{
				Use: harness, Short: action + " Blip's " + harness + " hooks", Args: cobra.NoArgs,
				RunE: func(command *cobra.Command, _ []string) error {
					if sourceID == "" {
						if harness != "codex" {
							return fmt.Errorf("specify --source for this harness")
						}
						sourceID = config.DefaultSourceID
					}
					return execute(command.Context(), path, []string{command.Parent().Name(), harness, sourceID}, command.InOrStdin(), command.OutOrStdout())
				},
			}
			child.Flags().StringVar(&sourceID, "source", "", "configured source ID")
			parent.AddCommand(child)
		}
		root.AddCommand(parent)
	}
	var accessible bool
	configure := &cobra.Command{
		Use: "configure", Aliases: []string{"config", "setup"},
		Short: "Interactively add, edit and enable notification channels",
		Long:  "Configure Feishu, WeCom, Gotify and custom webhook destinations.\nChanges are saved together after confirmation; Ctrl+C cancels without saving.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return setup.Run(command.Context(), path, command.InOrStdin(), command.OutOrStdout(), accessible)
		},
	}
	configure.Flags().BoolVar(&accessible, "accessible", false, "use screen-reader-friendly terminal prompts")
	root.AddCommand(configure)
	root.AddCommand(&cobra.Command{Use: "version", Short: "Show the build version", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(command.OutOrStdout(), version)
			return err
		},
	})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return root.ExecuteContext(ctx)
}

func execute(ctx context.Context, path string, command []string, input io.Reader, output io.Writer) error {
	var err error
	if command[0] == "init" {
		if _, err = os.Stat(path); err == nil {
			return fmt.Errorf("configuration already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data, err := config.ExampleBytes()
		if err != nil {
			return err
		}
		if err = store.WriteFile(path, data); err != nil {
			return err
		}
		absolute, _ := filepath.Abs(path)
		fmt.Fprintln(output, "Configuration created:", absolute)
		return nil
	}
	var c config.Config
	if command[0] == "start" || command[0] == "run" {
		c, err = config.Read(path)
	} else {
		c, err = config.ReadLocal(path)
	}
	if err != nil {
		if command[0] == "hook" {
			return nil
		}
		return err
	}
	switch command[0] {
	case "hook":
		id := config.DefaultSourceID
		if len(command) > 1 {
			id = command[1]
		}
		_ = agents.Capture(ctx, c, id, input)
		return nil // Notification failures cannot approve, deny, or block an agent operation.
	case "install", "uninstall":
		if len(command) < 2 {
			return fmt.Errorf("specify the harness")
		}
		id := config.DefaultSourceID
		if len(command) > 2 {
			id = command[2]
		}
		var source config.Source
		if command[0] == "uninstall" {
			source, err = agents.InstalledSource(c, id)
		} else {
			var ok bool
			source, ok = c.Source(id)
			if !ok {
				err = fmt.Errorf("source %q is not configured", id)
			}
		}
		if err != nil {
			return err
		}
		if source.Harness != command[1] {
			return fmt.Errorf("source %q belongs to %s", id, source.Harness)
		}
		adapter, err := agents.Builtins().Open(c, source)
		if err != nil {
			return err
		}
		installer, ok := adapter.(agents.Installer)
		if !ok {
			return fmt.Errorf("source does not support automatic installation")
		}
		if command[0] == "install" {
			if err = agents.Bind(ctx, c, source); err != nil {
				return err
			}
			executable, exeErr := os.Executable()
			if exeErr != nil {
				return exeErr
			}
			err = installer.Install(ctx, executable)
		} else {
			err = installer.Uninstall(ctx)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "%s hooks updated for %s. Review and trust newly installed hooks in the harness; restart existing sessions if needed.\n", source.Harness, source.ID)
		return nil
	case "run":
		if len(c.Enabled()) == 0 {
			return fmt.Errorf("enable at least one destination in the configuration")
		}
		return worker.Run(ctx, c, log.New(os.Stderr, "Blip ", log.LstdFlags))
	case "start":
		return start(c, output)
	case "stop":
		running, statusErr := isRunning(c)
		if statusErr != nil {
			return statusErr
		}
		if !running {
			fmt.Fprintln(output, "Blip is stopped.")
			return nil
		}
		if err = store.WriteFile(filepath.Join(c.StateDir, "stop"), []byte("stop\n")); err != nil {
			return err
		}
		for i := 0; i < 100; i++ {
			running, err = isRunning(c)
			if err != nil {
				return err
			}
			if !running {
				fmt.Fprintln(output, "Blip stopped.")
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprintln(output, "Stop requested; the worker is finishing its current operation.")
		return nil
	case "status":
		return printStatus(c, output)
	case "doctor":
		id := ""
		if len(command) > 1 {
			id = command[1]
		}
		return printDoctor(c, id, output)
	case "test":
		now := time.Now().UTC()
		notification := event.Event{SchemaVersion: 1, ID: event.ID("test", now.Format(time.RFC3339Nano)), Agent: "codex", Kind: event.TurnCompleted, Source: "blip/test", OccurredAt: now, SessionID: "blip-test", SessionTitle: "Blip 测试通知", TurnID: now.Format("20060102T150405.000000000"), Project: "Blip", DeviceName: c.DeviceName, Detail: "这是一条 Blip 测试通知。"}
		added, err := (store.Queue{Dir: c.StateDir}).Enqueue(ctx, notification, worker.DestinationIDs(c))
		if err != nil {
			return err
		}
		if !added {
			return fmt.Errorf("enable at least one destination before queuing a test notification")
		}
		fmt.Fprintln(output, "Test notification queued. Use blip status to inspect delivery.")
		return nil
	default:
		return fmt.Errorf("unknown command %q; use blip help", command[0])
	}
}

func isRunning(c config.Config) (bool, error) {
	lock, available, err := (store.Queue{Dir: c.StateDir}).WorkerLock()
	if err != nil {
		return false, err
	}
	if available {
		_ = lock.Unlock()
	}
	return !available, nil
}

func start(c config.Config, output io.Writer) error {
	if len(c.Enabled()) == 0 {
		return fmt.Errorf("enable at least one destination in the configuration")
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if running {
		var status worker.Status
		if store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.Ready {
			fmt.Fprintln(output, "Blip is already running.")
		} else {
			fmt.Fprintln(output, "Blip is initializing. Inspect blip status for readiness.")
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(c.StateDir, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	child := exec.Command(executable, "--config", c.Path, "run")
	child.Stdout, child.Stderr = file, file
	platform.HideProcess(child)
	if err = child.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	childPID := child.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	for i := 0; i < 50; i++ {
		running, err = isRunning(c)
		if err != nil {
			return err
		}
		var status worker.Status
		if running && store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.PID == childPID && status.Ready {
			fmt.Fprintln(output, "Blip started.")
			return nil
		}
		select {
		case <-exited:
			// A simultaneous start may have won the worker lock.
			active, _ := isRunning(c)
			if active && store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status) == nil && status.Ready {
				fmt.Fprintln(output, "Blip is already running.")
				return nil
			}
			return fmt.Errorf("worker exited during startup; inspect worker.log in the state directory")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("worker has not reported ready yet; inspect blip status and worker.log before starting again")
}
