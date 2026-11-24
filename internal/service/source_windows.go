//go:build windows

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
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
	if err := prepareSourceDatabases(ctx, plan.Sources); err != nil {
		return err
	}
	for _, target := range sourcePermissionTargets(plan) {
		if err := ctx.Err(); err != nil {
			return err
		}
		recorded := installed.hasGrant(target.path, sid.String())
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
	if err := reserveAntigravityGrants(&installed, plan.Sources, sid); err != nil {
		return err
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

func (m installMetadata) hasGrant(path, sid string) bool {
	return slices.ContainsFunc(m.Grants, func(g aclChange) bool { return samePath(g.Path, path) && g.SID == sid })
}

// The client can create its home and WAL files after service installation.
// Reserve those exact names in the protected uninstall ledger before a user
// hook may ever restore a grant. Never inherit reads from the client home.
func reserveAntigravityGrants(m *installMetadata, sources []config.Source, sid *windows.SID) error {
	previous := len(m.Grants)
	for _, source := range sources {
		if !source.Enabled || source.Harness != "antigravity" {
			continue
		}
		for _, target := range antigravityGrantTargets(*m, source) {
			if !m.hasGrant(target.path, sid.String()) {
				m.Grants = append(m.Grants, aclChange{Path: target.path, SID: sid.String()})
			}
		}
	}
	if previous == len(m.Grants) {
		return nil
	}
	return writeMetadata(*m)
}

func antigravityGrantTargets(m installMetadata, source config.Source) []permissionTarget {
	targets := []permissionTarget{{path: source.Home, permissions: identifyDirectory, directory: true}}
	// Intersect the adapter's known filenames with the administrator's plan.
	for _, entry := range sourceReadPaths(config.Config{}, source) {
		if slices.Contains(m.ReadPaths, entry) {
			targets = append(targets, permissionTarget{path: entry.Path, permissions: windows.FILE_GENERIC_READ})
		}
	}
	return targets
}

func lockServiceGrants(ctx context.Context, stateDir string) (*flock.Flock, error) {
	path := filepath.Join(stateDir, "service-acl.lock")
	if err := noReparsePoints(path); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// RefreshAntigravityAccess runs as the client, which owns SQLite's replacement
// files. Foreground-only configurations do not need file grants.
func RefreshAntigravityAccess(ctx context.Context, c config.Config, source config.Source) error {
	if !source.Enabled || source.Harness != "antigravity" {
		return nil
	}
	// Skip ordinary foreground installations before touching service state.
	dir, err := managedDirectory(c.Path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, metadataFilename)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := lockServiceGrants(ctx, c.StateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	svc, m, err := ownedService(c.Path, 0)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return nil
	}
	if err != nil {
		return err
	}
	defer svc.Close()
	index := slices.IndexFunc(m.Sources, func(s config.Source) bool {
		return s.Enabled && s.ID == source.ID && s.Harness == source.Harness && samePath(s.Home, source.Home)
	})
	if index < 0 || !samePath(m.StateDir, c.StateDir) {
		return nil
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\`+m.Name)
	if err != nil {
		return err
	}
	return refreshAntigravityFiles(ctx, m, m.Sources[index], sid)
}

func refreshAntigravityFiles(ctx context.Context, m installMetadata, source config.Source, sid *windows.SID) error {
	db := filepath.Join(source.Home, antigravity.SummaryFilename)
	// A cleanly closed WAL database may have neither sidecar. Let SQLite
	// create them as the user before granting the worker read-only access.
	for _, suffix := range []string{"-shm", "-wal"} {
		if _, err := os.Lstat(db + suffix); errors.Is(err, os.ErrNotExist) && m.hasGrant(db+suffix, sid.String()) {
			if err := prepareSourceDatabases(ctx, []config.Source{source}); err != nil {
				return err
			}
			break
		}
	}
	for _, target := range antigravityGrantTargets(m, source) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.hasGrant(target.path, sid.String()) {
			continue
		}
		if err := refreshRecordedPermission(target, sid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func prepareSourceDatabases(ctx context.Context, sources []config.Source) error {
	for _, source := range sources {
		if !source.Enabled || source.Harness != "antigravity" {
			continue
		}
		db := filepath.Join(source.Home, antigravity.SummaryFilename)
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := noReparsePoints(db + suffix); err != nil {
				return err
			}
		}
		if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
			continue // the client has not created a conversation database yet
		} else if err != nil {
			return err
		}
		if err := antigravity.PrepareServiceWAL(ctx, db); err != nil {
			return fmt.Errorf("prepare SQLite files for source %q: %w", source.ID, err)
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
