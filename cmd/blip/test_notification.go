package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/TeamSillyBees/Blip/internal/worker"
)

func sendTest(ctx context.Context, c config.Config, output io.Writer) error {
	if len(c.Enabled()) == 0 {
		return fmt.Errorf("请先启用通知目的地：%s", cliCommand(c.Path, "configure"))
	}
	running, err := isRunning(c)
	if err != nil {
		return err
	}
	if !running {
		if err := manageLifecycle(ctx, c.Path, "start", output); err != nil {
			return err
		}
	}
	id, err := enqueueTest(ctx, c)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "测试通知已入队，正在检查投递（最多 15 秒）…")
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return waitForTest(waitCtx, c, id, output)
}

// Keep queuing independent of channel validation and process startup so the
// existing noninteractive test command remains useful while the worker is off.
func enqueueTest(ctx context.Context, c config.Config) (string, error) {
	now := time.Now().UTC()
	notification := event.Event{SchemaVersion: 1, ID: event.ID("test", now.Format(time.RFC3339Nano)), Agent: "codex", Kind: event.TurnCompleted, Source: "blip/test", OccurredAt: now, SessionID: "blip-test", SessionTitle: "Blip 测试通知", TurnID: now.Format("20060102T150405.000000000"), Project: "Blip", DeviceName: c.DeviceName, Detail: "这是一条 Blip 测试通知。"}
	added, err := (store.Queue{Dir: c.StateDir}).Enqueue(ctx, notification, worker.DestinationIDs(c))
	if err != nil {
		return "", err
	}
	if !added {
		return "", fmt.Errorf("测试通知入队前，请先启用至少一个通知目的地")
	}
	return notification.ID, nil
}

func waitForTest(ctx context.Context, c config.Config, id string, output io.Writer) error {
	queue := store.Queue{Dir: c.StateDir}
	for {
		job, err := queue.ReadJob(id)
		if err != nil {
			return fmt.Errorf("读取测试投递结果失败；查看 %s：%w", cliCommand(c.Path, "status"), err)
		}
		settled := len(job.Deliveries) > 0
		for _, receipt := range job.Deliveries {
			if receipt.DeliveredAt == nil && !receipt.Failed && receipt.LastError == "" {
				settled = false
			}
		}
		if settled || ctx.Err() != nil {
			allDelivered := len(job.Deliveries) > 0
			for _, receipt := range job.Deliveries {
				switch {
				case receipt.DeliveredAt != nil:
					fmt.Fprintf(output, "  %s：已送达渠道\n", receipt.DestinationID)
				case receipt.Failed:
					fmt.Fprintf(output, "  %s：投递失败（%s）\n", receipt.DestinationID, receipt.LastError)
				case receipt.LastError != "":
					fmt.Fprintf(output, "  %s：等待重试（%s）\n", receipt.DestinationID, receipt.LastError)
				default:
					fmt.Fprintf(output, "  %s：仍在等待投递\n", receipt.DestinationID)
				}
				allDelivered = allDelivered && receipt.DeliveredAt != nil
			}
			if allDelivered {
				return nil
			}
			return fmt.Errorf("测试通知尚未全部送达；可重试的记录保留在队列中。查看 %s", cliCommand(c.Path, "status"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
