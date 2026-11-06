//go:build windows

package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type pendingServiceControl struct {
	states   []svc.State
	queries  int
	controls int
	current  svc.State
}

func (service *pendingServiceControl) Query() (svc.Status, error) {
	index := service.queries
	if index >= len(service.states) {
		index = len(service.states) - 1
	}
	service.current = service.states[index]
	service.queries++
	return svc.Status{State: service.current}, nil
}

func (service *pendingServiceControl) Control(command svc.Cmd) (svc.Status, error) {
	service.controls++
	if command != svc.Stop || service.current != svc.Running {
		return svc.Status{}, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	return svc.Status{State: svc.StopPending}, nil
}

func TestStopServiceWaitsForInitialization(t *testing.T) {
	for _, test := range []struct {
		name     string
		states   []svc.State
		controls int
	}{
		{"becomes ready", []svc.State{svc.StartPending, svc.StartPending, svc.Running, svc.StopPending, svc.Stopped}, 1},
		{"stops during initialization", []svc.State{svc.StartPending, svc.Stopped}, 0},
		{"already stopping", []svc.State{svc.StartPending, svc.StopPending, svc.Stopped}, 0},
		{"already running", []svc.State{svc.Running, svc.StopPending, svc.Stopped}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &pendingServiceControl{states: test.states}
			if err := stopService(context.Background(), service); err != nil {
				t.Fatal(err)
			}
			if service.controls != test.controls || service.current != svc.Stopped {
				t.Fatalf("controls=%d, final state=%v", service.controls, service.current)
			}
		})
	}
}

func TestStopServicePendingWaitRespectsContext(t *testing.T) {
	for _, state := range []svc.State{svc.StartPending, svc.StopPending} {
		t.Run(stateName(state), func(t *testing.T) {
			service := &pendingServiceControl{states: []svc.State{state}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := stopService(ctx, service); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline during pending state, got %v", err)
			}
			if service.controls != 0 {
				t.Fatalf("sent %d stop controls while pending", service.controls)
			}
		})
	}
}

func TestServicePlanningPinsLocalPathsAndQuotesArguments(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "中文 with spaces")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	opts := InstallOptions{ConfigPath: filepath.Join(directory, "config.json"), Executable: filepath.Join(directory, "source.exe"), CodexHome: filepath.Join(directory, "codex home"), StateDir: filepath.Join(directory, "state")}
	for _, path := range []string{opts.ConfigPath, opts.Executable} {
		if err := os.WriteFile(path, []byte("test only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, source, err := makeInstallMetadata(opts)
	if err != nil {
		t.Fatal(err)
	}
	if source != opts.Executable || plan.ConfigPath != opts.ConfigPath || plan.CodexHome != opts.CodexHome || plan.StateDir != opts.StateDir || !strings.Contains(plan.Executable, filepath.Join("Blip", Name(opts.ConfigPath))) {
		t.Fatalf("incorrect service paths: %+v", plan)
	}
	if Name(opts.ConfigPath) != Name(strings.ToUpper(opts.ConfigPath)) || Name(opts.ConfigPath) == Name(filepath.Join(directory, "other.json")) {
		t.Fatal("service name is not stable and configuration-specific")
	}
	args := append([]string{plan.Executable}, serviceArguments(plan)...)
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = syscall.EscapeArg(arg)
	}
	settings := mgr.Config{BinaryPathName: strings.Join(quoted, " "), ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, ServiceStartName: serviceAccount, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}
	if err = verifyServiceOwner(settings, plan); err != nil {
		t.Fatalf("valid quoted installation rejected: %v", err)
	}
	for name, change := range map[string]func(*mgr.Config){
		"account":   func(c *mgr.Config) { c.ServiceStartName = "LocalSystem" },
		"identity":  func(c *mgr.Config) { c.SidType = windows.SERVICE_SID_TYPE_NONE },
		"type":      func(c *mgr.Config) { c.ServiceType = windows.SERVICE_WIN32_SHARE_PROCESS },
		"arguments": func(c *mgr.Config) { c.BinaryPathName += " --unrelated" },
		"config": func(c *mgr.Config) {
			c.BinaryPathName = strings.Replace(c.BinaryPathName, "config.json", "other.json", 1)
		},
		"binary": func(c *mgr.Config) {
			c.BinaryPathName = strings.Replace(c.BinaryPathName, "blip.exe", "unrelated.exe", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			modified := settings
			change(&modified)
			if verifyServiceOwner(modified, plan) == nil {
				t.Fatal("unowned or reconfigured service was accepted")
			}
		})
	}
}

func TestServicePathGuardsAndReadOnlyMissingQuery(t *testing.T) {
	for _, path := range []string{"", `\\server\share\config.json`, `\\?\C:\config.json`, "C:\\bad\x00path"} {
		if _, err := localAbsolute(path); err == nil {
			t.Fatalf("unsupported path accepted: %q", path)
		}
	}
	if rejectBroadDirectory(`C:\`) == nil {
		t.Fatal("drive root accepted for service permissions")
	}
	profile, err := windows.KnownFolderPath(windows.FOLDERID_Profile, 0)
	if err == nil && rejectBroadDirectory(profile) == nil {
		t.Fatal("profile root accepted for service permissions")
	}
	status, err := Query(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil || status.Installed || status.State != "not_installed" {
		t.Fatalf("read-only absent service query failed: %+v %v", status, err)
	}
}

func TestProtectedInstallParentAllowsReadOnlyUsers(t *testing.T) {
	for _, entry := range []struct {
		name, descriptor string
		valid            bool
	}{
		{"created parent", "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)", true},
		{"read only users", "O:BAG:BAD:P(A;OICI;FA;;;BA)(A;OICI;FR;;;BU)", true},
		{"users write", "O:BAG:BAD:P(A;OICI;FA;;;BA)(A;OICI;FW;;;BU)", false},
		{"users delete children", "O:BAG:BAD:P(A;OICI;FA;;;BA)(A;OICI;0x40;;;BU)", false},
		{"untrusted owner", "O:BUG:BUD:P(A;OICI;FA;;;BA)(A;OICI;FR;;;BU)", false},
	} {
		t.Run(entry.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(entry.descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if err = validateInstallParentSecurity(sd); (err == nil) != entry.valid {
				t.Fatalf("valid=%v: %v", entry.valid, err)
			}
		})
	}
}

func allowedSIDEntries(t *testing.T, path, sid string, explicitOnly bool) int {
	t.Helper()
	acl, _, err := currentDACL(path)
	if err != nil {
		t.Fatal(err)
	}
	if acl == nil {
		t.Fatal("unexpected null DACL")
	}
	count := 0
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && (!explicitOnly || ace.Header.AceFlags&windows.INHERITED_ACE == 0) && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == sid {
			count++
		}
	}
	return count
}

func TestServicePermissionsSurviveAtomicConfigReplacementAndCleanUp(t *testing.T) {
	sid, err := windows.StringToSid("S-1-5-80-104021-104022-104023-104024-104025")
	if err != nil {
		t.Fatal(err)
	}
	otherSID, err := windows.StringToSid("S-1-5-80-204021-204022-204023-204024-204025")
	if err != nil {
		t.Fatal(err)
	}
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated ACL change", true: "atomic replacement"}[replace], func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "config.json")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			parentGrant, err := grantPermission(permissionTarget{directory, readExecute, windows.OBJECT_INHERIT_ACE | windows.NO_PROPAGATE_INHERIT_ACE, true}, sid)
			if err != nil {
				t.Fatal(err)
			}
			fileGrant, err := grantPermission(permissionTarget{path, windows.FILE_GENERIC_READ, 0, false}, sid)
			if err != nil {
				t.Fatal(err)
			}
			if replace {
				next := filepath.Join(directory, "replacement.json")
				if err := os.WriteFile(next, []byte("edited"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(next, path); err != nil {
					t.Fatal(err)
				}
				if allowedSIDEntries(t, path, sid.String(), false) == 0 || allowedSIDEntries(t, path, sid.String(), true) != 0 {
					t.Fatal("replacement config did not inherit service read access")
				}
			}
			if _, err := grantPermission(permissionTarget{path, windows.FILE_GENERIC_READ, 0, false}, otherSID); err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(directory, "unrelated-child")
			if err := os.Mkdir(child, 0700); err != nil {
				t.Fatal(err)
			}
			childFile := filepath.Join(child, "private.txt")
			if err := os.WriteFile(childFile, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			if allowedSIDEntries(t, childFile, sid.String(), false) != 0 {
				t.Fatal("configuration permission unexpectedly propagated into sibling directories")
			}
			if err := restorePermission(fileGrant); err != nil {
				t.Fatal(err)
			}
			if err := restorePermission(parentGrant); err != nil {
				t.Fatal(err)
			}
			if allowedSIDEntries(t, path, sid.String(), false) != 0 || allowedSIDEntries(t, path, otherSID.String(), true) == 0 {
				t.Fatal("cleanup left service access or removed unrelated access")
			}
		})
	}
}

// This test creates a separate service and a managed executable under Program
// Files. It is never run by default and never uses real Codex or channel data.
func TestWindowsServiceSCMLifecycle(t *testing.T) {
	if os.Getenv("BLIP_TEST_WINDOWS_SERVICE") != "1" {
		t.Skip("set BLIP_TEST_WINDOWS_SERVICE=1 in an elevated terminal for isolated SCM integration")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("SCM integration requires an elevated terminal")
	}
	directory := t.TempDir()
	opts := InstallOptions{ConfigPath: filepath.Join(directory, "config.json"), Executable: filepath.Join(directory, "source.exe"), CodexHome: filepath.Join(directory, "codex"), StateDir: filepath.Join(directory, "state")}
	build := exec.Command("go", "build", "-o", opts.Executable, "../../cmd/blip")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	data, _ := json.Marshal(map[string]any{"schema_version": 1, "device_name": "service-test", "poll_interval": "100ms", "destinations": []any{map[string]any{"id": "unused", "kind": "webhook", "enabled": true, "url": "http://127.0.0.1:9/"}}})
	if err := os.WriteFile(opts.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := Uninstall(cleanupCtx, opts.ConfigPath); err != nil {
			t.Errorf("isolated service cleanup: %v", err)
		}
	})
	if err := Install(ctx, opts); err != nil {
		t.Fatal(err)
	}
	status, err := Query(opts.ConfigPath)
	if err != nil || !status.Installed || status.State != "stopped" || status.StartType != "automatic_delayed" || !strings.EqualFold(status.Account, serviceAccount) {
		t.Fatalf("installation status: %+v %v", status, err)
	}
	if err := Start(ctx, opts.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := Stop(ctx, opts.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(ctx, opts.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if status, err = Query(opts.ConfigPath); err != nil || status.Installed {
		t.Fatalf("service remained registered: %+v %v", status, err)
	}
	if _, err := os.Stat(opts.ConfigPath); err != nil {
		t.Fatal("uninstall removed user configuration")
	}
}
