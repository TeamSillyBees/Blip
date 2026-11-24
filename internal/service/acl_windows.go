//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type aclChange struct {
	Path string `json:"path"`
	SID  string `json:"sid"`
}

type permissionTarget struct {
	path        string
	permissions windows.ACCESS_MASK
	inheritance uint32
	directory   bool
}

const readExecute = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE
const modify = readExecute | windows.FILE_GENERIC_WRITE | windows.DELETE
const inheritChildren = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
const identifyDirectory = windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE
const deleteChildAccess = 0x0040 // FILE_DELETE_CHILD, absent from x/sys v0.47.0.

var deleteACE = windows.NewLazySystemDLL("advapi32.dll").NewProc("DeleteAce")

func ensureInstallParent(path string) error {
	if err := noReparsePoints(path); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0755); err == nil {
		return protectInstallDirectory(path)
	} else if !os.IsExist(err) {
		return err
	}
	// Never place a service executable below an existing directory whose owner
	// or write ACL permits a non-administrator to replace its children.
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validateInstallParentSecurity(sd)
}

func validateInstallParentSecurity(sd *windows.SECURITY_DESCRIPTOR) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || (owner.String() != "S-1-5-32-544" && owner.String() != "S-1-5-18") {
		return fmt.Errorf("existing Blip service directory must be owned by Administrators or SYSTEM")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("existing Blip service directory must have a protected write ACL")
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("unsupported ACL on existing Blip service directory")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		const actualWriteRights = windows.GENERIC_WRITE | windows.GENERIC_ALL |
			windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
			windows.FILE_WRITE_ATTRIBUTES | deleteChildAccess |
			windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER
		if ace.Mask&actualWriteRights != 0 && sid != "S-1-5-32-544" && sid != "S-1-5-18" {
			return fmt.Errorf("existing Blip service directory grants write access outside Administrators and SYSTEM")
		}
	}
	return nil
}

func permissionTargets(m installMetadata) []permissionTarget {
	// Only immediate replacement config files inherit this grant. In particular,
	// this does not recursively expose a user's profile or sibling directories.
	targets := []permissionTarget{
		{filepath.Dir(m.ConfigPath), readExecute, windows.OBJECT_INHERIT_ACE | windows.NO_PROPAGATE_INHERIT_ACE, true},
		{m.ConfigPath, windows.FILE_GENERIC_READ, 0, false},
		{m.StateDir, modify, inheritChildren, true},
	}
	if m.Format != 2 {
		targets = append(targets,
			permissionTarget{m.CodexHome, identifyDirectory, 0, true},
			permissionTarget{filepath.Join(m.CodexHome, "sessions"), readExecute, inheritChildren, true})
	}
	// The title index is optional. Read only an existing regular file; granting
	// access to CodexHome would also expose credentials and other configuration.
	indexPath := filepath.Join(m.CodexHome, "session_index.jsonl")
	if m.Format != 2 && noReparsePoints(indexPath) == nil {
		if info, err := os.Lstat(indexPath); err == nil && info.Mode().IsRegular() {
			targets = append(targets, permissionTarget{indexPath, windows.FILE_GENERIC_READ, 0, false})
		}
	}
	if m.Format == 2 {
		targets = append(targets, sourcePermissionTargets(m)...)
	}
	merged := make(map[string]permissionTarget)
	for _, target := range targets {
		key := strings.ToLower(filepath.Clean(target.path))
		if previous, ok := merged[key]; ok {
			target.permissions |= previous.permissions
			target.inheritance |= previous.inheritance
			if target.inheritance&windows.CONTAINER_INHERIT_ACE != 0 {
				target.inheritance &^= windows.NO_PROPAGATE_INHERIT_ACE
			}
		}
		merged[key] = target
	}
	result := make([]permissionTarget, 0, len(merged))
	for _, target := range merged {
		result = append(result, target)
	}
	// Apply ancestors first; rollback reverses this order to preserve inherited
	// permissions when configuration and queue directories overlap.
	sort.Slice(result, func(i, j int) bool {
		if len(result[i].path) == len(result[j].path) {
			return result[i].path < result[j].path
		}
		return len(result[i].path) < len(result[j].path)
	})
	return result
}

func sourcePermissionTargets(m installMetadata) []permissionTarget {
	var targets []permissionTarget
	for _, source := range m.Sources {
		if source.Enabled {
			if source.Harness == "antigravity" {
				if _, err := os.Lstat(source.Home); os.IsNotExist(err) {
					continue // the shared hooks can be installed before the client starts
				}
			}
			// Grant identity lookup without listing or inheriting access to the home.
			targets = append(targets, permissionTarget{source.Home, identifyDirectory, 0, true})
		}
	}
	for _, entry := range m.ReadPaths {
		if entry.Optional {
			if noReparsePoints(entry.Path) != nil {
				continue
			}
			info, err := os.Lstat(entry.Path)
			if err != nil || (!entry.Directory && !info.Mode().IsRegular()) || (entry.Directory && !info.IsDir()) {
				continue
			}
		}
		permissions, inheritance := windows.ACCESS_MASK(windows.FILE_GENERIC_READ), uint32(0)
		if entry.Directory {
			permissions = readExecute
			inheritance = inheritChildren
		}
		targets = append(targets, permissionTarget{entry.Path, permissions, inheritance, entry.Directory})
	}
	return targets
}

func protectInstallDirectory(path string) error {
	if err := noReparsePoints(path); err != nil {
		return err
	}
	// This directory was created exclusively by this installation. Keep its
	// ownership and write access with administrators; ordinary users may inspect
	// the executable and installation record, but cannot replace either.
	sd, err := windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)")
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}

func protectInstallFile(path string) error {
	owner, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, owner, nil, nil, nil)
}

func currentDACL(path string) (*windows.ACL, string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, "", err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return nil, "", err
	}
	return acl, sd.String(), nil
}

func grantPermission(target permissionTarget, sid *windows.SID) (aclChange, error) {
	return applyPermission(target, sid, false, nil)
}

func applyPermission(target permissionTarget, sid *windows.SID, recorded bool, beforeGrant func(aclChange) error) (aclChange, error) {
	change := aclChange{Path: target.path}
	if err := noReparsePoints(target.path); err != nil {
		return change, err
	}
	if target.directory {
		if err := os.MkdirAll(target.path, 0700); err != nil {
			return change, err
		}
	}
	acl, _, err := currentDACL(target.path)
	if err != nil {
		return change, err
	}
	updated, err := aclWithServiceGrant(acl, target, sid, recorded)
	if err != nil || updated == nil {
		return change, err
	}
	if !recorded && beforeGrant != nil {
		if err := beforeGrant(aclChange{Path: target.path, SID: sid.String()}); err != nil {
			return change, err
		}
	}
	if err = windows.SetNamedSecurityInfo(target.path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, updated, nil); err != nil {
		return change, err
	}
	change.SID = sid.String()
	return change, nil
}

// Return nil when no change is necessary.
func aclWithServiceGrant(acl *windows.ACL, target permissionTarget, sid *windows.SID, recorded bool) (*windows.ACL, error) {
	if acl == nil {
		// A null DACL already permits access; replacing it with only this service
		// would silently remove other access, so leave it intact.
		return nil, nil
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceFlags&windows.INHERITED_ACE == 0 && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == sid.String() {
			if recorded && ace.Mask&target.permissions == target.permissions && uint32(ace.Header.AceFlags) == target.inheritance {
				return nil, nil
			}
			return nil, fmt.Errorf("service identity already has explicit access at %s; clean the prior service installation first", target.path)
		}
	}
	var pinned runtime.Pinner
	pinned.Pin(sid)
	defer pinned.Unpin()
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: target.permissions,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       target.inheritance,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, acl)
}

// A client hook may see replacement files between callbacks. Inspect the
// object and change its DACL through the same handle, without following a
// reparse point or granting access to another name for a hard-linked file.
func refreshRecordedPermission(target permissionTarget, sid *windows.SID) error {
	// Service plans contain only absolute DOS drive paths. Ask NT to reject
	// reparses in the whole name while opening it, including its ancestors.
	name, err := windows.NewNTUnicodeString(`\??\` + target.path)
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var status windows.IO_STATUS_BLOCK
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	const read = windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES
	open := func(access uint32) error {
		return windows.NtCreateFile(&h, access, &oa, &status, nil, 0, share, windows.FILE_OPEN, 0, 0, 0)
	}
	err = open(read | windows.WRITE_DAC)
	if errors.Is(err, windows.STATUS_ACCESS_DENIED) {
		// An existing admin-owned file can already have the necessary grant.
		err = open(read)
	}
	if err != nil {
		if nt, ok := err.(windows.NTStatus); ok {
			err = nt.Errno()
		}
		return &os.PathError{Op: "open recorded service grant", Path: target.path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != target.directory || (!target.directory && info.NumberOfLinks != 1) {
		return fmt.Errorf("invalid file for recorded service grant at %s", target.path)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	updated, err := aclWithServiceGrant(acl, target, sid, true)
	if err != nil || updated == nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, updated, nil)
}

func restorePermission(change aclChange) error {
	if change.SID == "" {
		return nil
	}
	if err := noReparsePoints(change.Path); err != nil {
		return err
	}
	if _, err := os.Stat(change.Path); os.IsNotExist(err) {
		return nil
	}
	acl, _, err := currentDACL(change.Path)
	if err != nil {
		return err
	}
	if acl == nil {
		return nil
	}
	// Remove only our explicit allow ACEs. Keep inherited access and all other
	// principals, including changes made after installation. A config replaced
	// atomically may now have only inherited access; that is a normal no-op here.
	changed := false
	for index := int(acl.AceCount) - 1; index >= 0; index-- {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, uint32(index), &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 || (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() != change.SID {
			continue
		}
		ok, _, callErr := deleteACE.Call(uintptr(unsafe.Pointer(acl)), uintptr(index))
		if ok == 0 {
			return callErr
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return windows.SetNamedSecurityInfo(change.Path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
