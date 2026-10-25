package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
)

var version = "0.1.0-dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Blip:", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("blip", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("config", defaultPath, "configuration path")
	if err = flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			help(output)
			return nil
		}
		return err
	}
	command := flags.Args()
	if len(command) == 0 {
		help(output)
		return nil
	}
	if command[0] == "version" {
		fmt.Fprintln(output, version)
		return nil
	}
	if command[0] == "help" {
		help(output)
		return nil
	}
	if command[0] == "init" {
		if _, err = os.Stat(*path); err == nil {
			return fmt.Errorf("configuration already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = store.WriteFile(*path, config.Example); err != nil {
			return err
		}
		absolute, _ := filepath.Abs(*path)
		fmt.Fprintln(output, "Configuration created:", absolute)
		return nil
	}
	var c config.Config
	if command[0] == "start" || command[0] == "run" {
		c, err = config.Read(*path)
	} else {
		c, err = config.ReadLocal(*path)
	}
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	switch command[0] {
	case "hook":
		notification, err := codex.ParseHook(input, time.Now())
		if err != nil || notification == nil {
			return err
		}
		hookCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		_, err = (store.Queue{Dir: c.StateDir}).Enqueue(hookCtx, *notification, worker.DestinationIDs(c))
		return err // Successful hooks write no stdout and cannot approve/deny a Codex operation.
	case "install", "uninstall":
		if len(command) != 2 || command[1] != "codex" {
			return fmt.Errorf("use blip install codex or blip uninstall codex")
		}
		if command[0] == "install" {
			executable, exeErr := os.Executable()
			if exeErr != nil {
				return exeErr
			}
			err = codex.Install(ctx, c, executable)
		} else {
			err = codex.Uninstall(ctx, c)
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(output, "Codex hooks updated. Review and trust newly installed hooks in Codex; restart existing sessions if needed.")
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
		running, err := isRunning(c)
		if err != nil {
			return err
		}
		jobs, err := (store.Queue{Dir: c.StateDir}).Load()
		if err != nil {
			return err
		}
		pending, delivered, failed := 0, 0, 0
		for _, job := range jobs {
			for _, receipt := range job.Deliveries {
				if receipt.DeliveredAt != nil {
					delivered++
				} else if receipt.Failed {
					failed++
				} else {
					pending++
				}
			}
		}
		return json.NewEncoder(output).Encode(map[string]any{"running": running, "pending": pending, "delivered": delivered, "failed": failed, "state_dir": c.StateDir})
	case "test":
		now := time.Now().UTC()
		notification := event.Event{SchemaVersion: 1, ID: event.ID("test", now.Format(time.RFC3339Nano)), Agent: "codex", Kind: event.TurnCompleted, Source: "blip/test", OccurredAt: now, SessionID: "blip-test", TurnID: now.Format("20060102T150405.000000000"), Project: "Blip", Detail: "这是一条 Blip 测试通知。"}
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
		fmt.Fprintln(output, "Blip is already running.")
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
	_ = child.Process.Release()
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
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("worker did not start; inspect worker.log in the state directory")
}

func help(output io.Writer) {
	fmt.Fprintln(output, `Blip — Codex notifications

Usage: blip [--config PATH] COMMAND

  init              Create a configuration with disabled example destinations
  install codex     Install input and approval hooks; preserve existing hooks
  uninstall codex   Remove only this configuration's Blip hooks
  start / stop      Start a hidden worker or request a graceful stop
  run               Run the worker in the current terminal
  status            Show worker and per-destination delivery counts
  test              Queue a test notification for enabled destinations
  version           Show the build version

Set --config before COMMAND. Completion notifications require the worker;
new hooks require review and trust in Codex.`)
}
