//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceAccount = `NT AUTHORITY\LocalService`

func administrativeError(action string, err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return fmt.Errorf("%s requires Administrator privileges; run this command in an elevated terminal: %w", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func connectSCM(access uint32) (*mgr.Mgr, error) {
	handle, err := windows.OpenSCManager(nil, nil, access)
	if err != nil {
		return nil, err
	}
	return &mgr.Mgr{Handle: handle}, nil
}

func openService(name string, access uint32) (*mgr.Service, error) {
	manager, err := connectSCM(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	defer manager.Disconnect()
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, err := windows.OpenService(manager.Handle, pointer, access)
	if err != nil {
		return nil, err
	}
	return &mgr.Service{Name: name, Handle: handle}, nil
}

func verifyServiceOwner(actual mgr.Config, metadata installMetadata) error {
	arguments, err := windows.DecomposeCommandLine(actual.BinaryPathName)
	expected := append([]string{metadata.Executable}, serviceArguments(metadata)...)
	if err != nil || len(arguments) != len(expected) {
		return fmt.Errorf("service name is occupied by an installation that does not belong to this configuration")
	}
	for index, value := range expected {
		matches := arguments[index] == value
		switch index {
		case 0, 2, 8, 10:
			matches = samePath(arguments[index], value)
		}
		if !matches {
			return fmt.Errorf("refusing to modify a service with a different executable, configuration, or startup arguments")
		}
	}
	if actual.ServiceType != windows.SERVICE_WIN32_OWN_PROCESS {
		return fmt.Errorf("refusing to manage a service with a different service type")
	}
	if !strings.EqualFold(actual.ServiceStartName, serviceAccount) || actual.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		return fmt.Errorf("refusing to manage a service whose LocalService account or service identity was changed")
	}
	return nil
}

func ownedService(configPath string, access uint32) (*mgr.Service, installMetadata, error) {
	var metadata installMetadata
	abs, err := localAbsolute(configPath)
	if err != nil {
		return nil, metadata, err
	}
	service, err := openService(Name(abs), access|windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return nil, metadata, err
	}
	metadata, err = readMetadata(abs)
	if err == nil {
		var settings mgr.Config
		settings, err = service.Config()
		if err == nil {
			err = verifyServiceOwner(settings, metadata)
		}
	}
	if err != nil {
		service.Close()
		return nil, metadata, err
	}
	return service, metadata, nil
}

// Install creates a delayed automatic LocalService service but does not start
// it. Its executable is an administrator-owned copy, independent of npm updates.
func Install(ctx context.Context, opts InstallOptions) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, source, err := makeInstallMetadata(opts)
	if err != nil {
		return err
	}
	manager, err := connectSCM(windows.SC_MANAGER_CONNECT | windows.SC_MANAGER_CREATE_SERVICE)
	if err != nil {
		return administrativeError("install Windows service", err)
	}
	defer manager.Disconnect()
	existing, err := openService(metadata.Name, windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS)
	if err == nil {
		existing.Close()
		owned, _, ownershipErr := ownedService(metadata.ConfigPath, 0)
		if ownershipErr != nil {
			return fmt.Errorf("service name already exists; %w", ownershipErr)
		}
		owned.Close()
		return fmt.Errorf("Windows service is already installed; stop and uninstall it, then install again to update the managed copy")
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return administrativeError("inspect existing Windows service", err)
	}
	directory := filepath.Dir(metadata.Executable)
	if _, err = os.Lstat(directory); err == nil {
		return fmt.Errorf("managed service directory already exists; run blip service uninstall with this configuration to clean the previous installation")
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(directory)
	if err = ensureInstallParent(parent); err != nil {
		return administrativeError("prepare protected service directory", err)
	}
	if err = os.Mkdir(directory, 0755); err != nil {
		return administrativeError("create managed service directory", err)
	}
	var registered *mgr.Service
	defer func() {
		canCleanFiles := true
		if registered != nil {
			if result != nil {
				if deleteErr := registered.Delete(); deleteErr != nil {
					canCleanFiles = false
					result = errors.Join(result, fmt.Errorf("rollback service registration: %w", deleteErr))
				}
			}
			registered.Close()
		}
		if result != nil && canCleanFiles {
			if cleanupErr := cleanupInstallation(context.Background(), metadata); cleanupErr != nil {
				result = errors.Join(result, fmt.Errorf("installation rollback requires attention: %w", cleanupErr))
			}
		}
	}()
	if err = protectInstallDirectory(directory); err != nil {
		return administrativeError("protect service executable directory", err)
	}
	if err = copyExecutable(source, metadata.Executable); err != nil {
		return fmt.Errorf("copy service executable: %w", err)
	}
	if err = protectInstallFile(metadata.Executable); err != nil {
		return administrativeError("protect service executable owner", err)
	}
	if err = writeMetadata(metadata); err != nil {
		return err
	}
	// Do not allow a partial installation to auto-start before its permissions
	// and recovery settings are fully installed.
	registered, err = manager.CreateService(metadata.Name, metadata.Executable, mgr.Config{
		DisplayName: "Blip notifications (" + strings.TrimPrefix(metadata.Name, "Blip-") + ")",
		Description: "Sends local agent notifications using " + metadata.ConfigPath,
		StartType:   mgr.StartManual, ErrorControl: mgr.ErrorNormal,
		ServiceStartName: serviceAccount, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, serviceArguments(metadata)...)
	if err != nil {
		return administrativeError("register Windows service", err)
	}
	sid, _, _, err := windows.LookupSID("", `NT SERVICE\`+metadata.Name)
	if err != nil {
		return fmt.Errorf("resolve service identity: %w", err)
	}
	if _, err = grantPermission(permissionTarget{directory, readExecute, inheritChildren, true}, sid); err != nil {
		return fmt.Errorf("grant service executable access: %w", err)
	}
	for _, target := range permissionTargets(metadata) {
		if err = ctx.Err(); err != nil {
			return err
		}
		change, grantErr := grantPermission(target, sid)
		if change.SID != "" {
			metadata.Grants = append(metadata.Grants, change)
		}
		if err = writeMetadata(metadata); err != nil {
			return err
		}
		if grantErr != nil {
			return fmt.Errorf("grant service file access: %w", grantErr)
		}
	}
	if err = registered.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.NoAction},
	}, 86400); err != nil {
		return fmt.Errorf("set service recovery: %w", err)
	}
	if err = registered.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set service failure recovery: %w", err)
	}
	settings, err := registered.Config()
	if err != nil {
		return err
	}
	settings.StartType, settings.DelayedAutoStart = mgr.StartAutomatic, true
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = registered.UpdateConfig(settings); err != nil {
		return fmt.Errorf("enable automatic service startup: %w", err)
	}
	return nil
}

func copyExecutable(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	return errors.Join(copyErr, output.Close())
}

func Start(ctx context.Context, configPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	service, _, err := ownedService(configPath, windows.SERVICE_START)
	if err != nil {
		return administrativeError("start Windows service", err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Running {
		return nil
	}
	if status.State != svc.StartPending {
		if status.State != svc.Stopped {
			return fmt.Errorf("Windows service is %s; wait before starting it", stateName(status.State))
		}
		if err = service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return administrativeError("start Windows service", err)
		}
	}
	return waitState(ctx, service, svc.Running)
}

func Stop(ctx context.Context, configPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	service, _, err := ownedService(configPath, windows.SERVICE_STOP)
	if err != nil {
		return administrativeError("stop Windows service", err)
	}
	defer service.Close()
	return stopService(ctx, service)
}

type serviceControl interface {
	Query() (svc.Status, error)
	Control(svc.Cmd) (svc.Status, error)
}

func stopService(ctx context.Context, service serviceControl) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stopRequested := false
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for Windows service to become stopped: %w", err)
		}
		status, err := service.Query()
		if err != nil {
			return err
		}
		switch status.State {
		case svc.Stopped:
			return nil
		case svc.StartPending, svc.StopPending:
			// SCM does not accept Stop during initialization. Wait for the
			// worker to become ready or finish stopping within the same budget.
		default:
			if !stopRequested {
				if _, err = service.Control(svc.Stop); err != nil {
					return administrativeError("stop Windows service", err)
				}
				stopRequested = true
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Windows service to become stopped: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitState(ctx context.Context, service *mgr.Service, expected svc.State) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == expected {
			return nil
		}
		if expected == svc.Running && status.State == svc.Stopped {
			return fmt.Errorf("Windows service stopped during startup (exit code %d, service code %d); inspect service.log and worker.log in the state directory", status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Windows service to become %s: %w", stateName(expected), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func Query(configPath string) (Status, error) {
	abs, err := localAbsolute(configPath)
	if err != nil {
		return Status{}, err
	}
	result := Status{Name: Name(abs), ConfigPath: abs, State: "not_installed"}
	service, metadata, err := ownedService(abs, 0)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return result, nil
	}
	if err != nil {
		return result, administrativeError("query Windows service", err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return result, err
	}
	settings, err := service.Config()
	if err != nil {
		return result, err
	}
	result.Installed, result.State, result.Executable, result.Account = true, stateName(status.State), metadata.Executable, settings.ServiceStartName
	switch settings.StartType {
	case mgr.StartAutomatic:
		result.StartType = "automatic"
		if settings.DelayedAutoStart {
			result.StartType = "automatic_delayed"
		}
	case mgr.StartManual:
		result.StartType = "manual"
	case mgr.StartDisabled:
		result.StartType = "disabled"
	default:
		result.StartType = fmt.Sprintf("unknown_%d", settings.StartType)
	}
	return result, nil
}

func Uninstall(ctx context.Context, configPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	service, metadata, err := ownedService(configPath, windows.SERVICE_STOP|windows.DELETE)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		// A prior uninstall may have removed the SCM entry before an executable
		// handle closed, or before a filesystem cleanup error was resolved.
		metadata, err = readMetadata(configPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return cleanupInstallation(ctx, metadata)
	}
	if err != nil {
		return administrativeError("uninstall Windows service", err)
	}
	if err = stopService(ctx, service); err != nil {
		service.Close()
		return err
	}
	err = service.Delete()
	service.Close()
	if err != nil {
		return administrativeError("unregister Windows service", err)
	}
	return cleanupInstallation(ctx, metadata)
}

func cleanupInstallation(ctx context.Context, metadata installMetadata) error {
	directory, err := managedDirectory(metadata.ConfigPath)
	if err != nil || !samePath(filepath.Join(directory, "blip.exe"), metadata.Executable) || metadata.Name != Name(metadata.ConfigPath) {
		return fmt.Errorf("refusing cleanup outside the owned service installation")
	}
	if err = noReparsePoints(directory); err != nil {
		return err
	}
	for index := len(metadata.Grants) - 1; index >= 0; index-- {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = restorePermission(metadata.Grants[index]); err != nil {
			return fmt.Errorf("service removed, but permission cleanup is incomplete: %w", err)
		}
		metadata.Grants = metadata.Grants[:index]
		if err = writeMetadata(metadata); err != nil {
			return err
		}
	}
	// SERVICE_STOPPED can precede process exit and release of the executable.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = os.Remove(metadata.Executable)
		if err == nil || os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service removed, but managed executable is still in use; rerun service uninstall: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = os.Remove(filepath.Join(directory, metadataFilename)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = os.Remove(directory); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove owned service directory: %w", err)
	}
	return nil
}

func stateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown_%d", state)
	}
}
