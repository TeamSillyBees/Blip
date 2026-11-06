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
		Use: "service", Short: "Manage the Windows service and automatic startup",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error { return command.Help() },
	}
	for _, entry := range []struct{ name, description string }{
		{"install", "Register an automatic delayed-start Windows service (administrator required)"},
		{"uninstall", "Stop and remove the Windows service, keeping configuration and data"},
		{"start", "Start the registered Windows service"},
		{"stop", "Stop the Windows service gracefully"},
		{"status", "Show Windows service registration and status"},
	} {
		command.AddCommand(&cobra.Command{
			Use: entry.name, Short: entry.description, Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				return manageService(command.Context(), command.Name(), *path, command.OutOrStdout())
			},
		})
	}
	var name string
	var defaults config.RuntimeDefaults
	run := &cobra.Command{
		Use: "run", Hidden: true, Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return service.Run(name, func(ctx context.Context, ready func()) error {
				return runServiceWorker(ctx, *path, defaults, ready)
			})
		},
	}
	run.Flags().StringVar(&name, "name", "", "registered service name")
	run.Flags().StringVar(&defaults.CodexHome, "default-codex-home", "", "Codex home captured during registration")
	run.Flags().StringVar(&defaults.StateDir, "default-state-dir", "", "state directory captured during registration")
	for _, flag := range []string{"name", "default-codex-home", "default-state-dir"} {
		_ = run.MarkFlagRequired(flag)
	}
	command.AddCommand(run)
	return command
}

func manageService(ctx context.Context, action, path string, output io.Writer) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("Windows services are only available on Windows")
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
			return fmt.Errorf("configure and enable at least one destination before installing the service")
		}
		if running, err := isRunning(c); err != nil {
			return err
		} else if running {
			return fmt.Errorf("a worker is already running; run blip stop before installing the service")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err = service.Install(ctx, service.InstallOptions{ConfigPath: c.Path, Executable: executable, CodexHome: c.CodexHome, StateDir: c.StateDir, Sources: c.Sources}); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows service installed:", service.Name(absolute))
		fmt.Fprintln(output, "Automatic delayed startup is enabled. Run blip service start to start it now.")
	case "start":
		if err = service.Start(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows service started.")
	case "stop":
		if err = service.Stop(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows service stopped.")
	case "uninstall":
		if err = service.Uninstall(ctx, absolute); err != nil {
			return err
		}
		fmt.Fprintln(output, "Windows service removed. Configuration and notification data were kept.")
	case "status":
		status, err := service.Query(absolute)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(status)
	default:
		return fmt.Errorf("unknown service operation")
	}
	return nil
}

// Log startup failures in the registered state directory before opening the
// configuration: a malformed file must still leave a useful service diagnostic.
func runServiceWorker(ctx context.Context, path string, defaults config.RuntimeDefaults, ready func()) error {
	if !filepath.IsAbs(path) || !filepath.IsAbs(defaults.StateDir) || (defaults.CodexHome != "" && !filepath.IsAbs(defaults.CodexHome)) {
		return fmt.Errorf("service configuration and default directories must be absolute")
	}
	if err := os.MkdirAll(defaults.StateDir, 0700); err != nil {
		return fmt.Errorf("open service state directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(defaults.StateDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	defer file.Close()
	logger := log.New(file, "Blip service ", log.LstdFlags)
	c, err := config.ReadWithDefaults(path, defaults)
	if err != nil {
		// JSON decoder errors can repeat unknown field names supplied by the
		// file. Keep potentially sensitive configuration text out of the log.
		logger.Print("configuration could not be loaded; run blip run with this configuration to inspect the error")
		return err
	}
	if c.SchemaVersion == 1 && defaults.CodexHome == "" {
		return fmt.Errorf("legacy service configuration requires its original Codex home; reinstall the service")
	}
	if len(c.Enabled()) == 0 {
		err = fmt.Errorf("enable at least one notification destination")
		logger.Print(err)
		return err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		logger.Printf("worker state directory: %v", err)
		return err
	}
	workerFile, err := os.OpenFile(filepath.Join(c.StateDir, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logger.Printf("worker log: %v", err)
		return err
	}
	defer workerFile.Close()
	err = worker.RunWithReady(ctx, c, log.New(workerFile, "Blip ", log.LstdFlags), ready)
	if err != nil {
		logger.Printf("worker exited: %v", err)
	}
	return err
}
