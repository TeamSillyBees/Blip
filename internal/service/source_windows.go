//go:build windows

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"golang.org/x/sys/windows"
)

// UpdateSources extends an owned service in place and leaves it stopped for the
// caller to finish the handover from any legacy worker before starting SCM.
func UpdateSources(ctx context.Context, path string, sources []config.Source, executable string) error {
	svc, installed, err := ownedService(path, windows.SERVICE_STOP|windows.SERVICE_START)
	if err != nil {
		return administrativeError("update Windows service", err)
	}
	defer svc.Close()
	plan, binary, err := makeInstallMetadata(InstallOptions{
		ConfigPath: path, Executable: executable, StateDir: installed.StateDir, Sources: append([]config.Source{}, sources...),
	})
	if err != nil {
		return err
	}
	staged, err := stageServiceExecutable(binary, installed.Executable)
	if err != nil {
		return err
	}
	if staged != "" {
		defer os.Remove(staged)
	}
	if err := recordServiceSources(&installed, plan); err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\`+installed.Name)
	if err != nil {
		return err
	}
	if err := writeMetadata(installed); err != nil {
		return err
	}
	for _, target := range sourcePermissionTargets(plan) {
		if err := ctx.Err(); err != nil {
			return err
		}
		recorded := slices.ContainsFunc(installed.Grants, func(g aclChange) bool {
			return samePath(g.Path, target.path) && g.SID == sid.String()
		})
		// Journal each new ACE before applying it. A repeat can finish an
		// interrupted grant or restore access to a replaced file.
		_, err := applyPermission(target, sid, recorded, func(change aclChange) error {
			installed.Grants = append(installed.Grants, change)
			return writeMetadata(installed)
		})
		if err != nil {
			return fmt.Errorf("grant source access: %w", err)
		}
	}
	if err := stopService(ctx, svc); err != nil {
		return err
	}
	if staged != "" {
		if err := replaceServiceExecutable(ctx, staged, installed.Executable); err != nil {
			// The original executable is still in place if replacement failed.
			return errors.Join(err, Start(context.WithoutCancel(ctx), path))
		}
	}
	return nil
}

// Keep the complete uninstall ledger, including legacy Codex grants from a
// v1 service. Its captured CodexHome remains part of the SCM command line.
func recordServiceSources(installed *installMetadata, plan installMetadata) error {
	if installed.Format == 1 {
		legacy := config.Config{CodexHome: installed.CodexHome}
		installed.Sources = legacy.EffectiveSources()
		installed.ReadPaths = sourceReadPaths(legacy, installed.Sources[0])
		installed.Format = 2
	}
	for _, source := range plan.Sources {
		index := slices.IndexFunc(installed.Sources, func(s config.Source) bool { return strings.EqualFold(s.ID, source.ID) })
		if index < 0 {
			installed.Sources = append(installed.Sources, source)
		} else {
			previous := installed.Sources[index]
			if previous.ID != source.ID || previous.Harness != source.Harness || !samePath(previous.Home, source.Home) {
				return fmt.Errorf("service source %q belongs to a different installation", source.ID)
			}
			installed.Sources[index] = source
		}
	}
	for _, entry := range plan.ReadPaths {
		if !slices.Contains(installed.ReadPaths, entry) {
			installed.ReadPaths = append(installed.ReadPaths, entry)
		}
	}
	if err := validateReadPaths(*installed); err != nil {
		return err
	}
	return validateRecordedGrants(*installed)
}

func stageServiceExecutable(source, target string) (string, error) {
	if err := noReparsePoints(target); err != nil {
		return "", err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(target)
	if err != nil || bytes.Equal(data, old) {
		return "", err
	}
	staged := target + ".update"
	if err := noReparsePoints(staged); err != nil {
		return "", err
	}
	if err := store.WriteFile(staged, data); err != nil {
		return "", err
	}
	if err := protectInstallFile(staged); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	return staged, nil
}

func replaceServiceExecutable(ctx context.Context, staged, target string) error {
	// SCM can report STOPPED slightly before Windows releases the image.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		err := os.Rename(staged, target)
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
