package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
	"golang.org/x/mod/semver"
)

// ClientExecutable is the path registered by integrations on this platform.
func ClientExecutable() (string, error) {
	if runtime.GOOS != "windows" {
		return os.Executable()
	}
	local := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(local) {
		return "", fmt.Errorf("LOCALAPPDATA 必须指向当前用户的绝对目录")
	}
	return filepath.Join(local, "Programs", "Blip", "blip.exe"), nil
}

// EnsureClientExecutable is shared by npm's verified native helper, standalone
// installs and the configuration wizard. Hook invocations never synchronize.
func EnsureClientExecutable(ctx context.Context, version, expectedHash string) (string, error) {
	source, err := os.Executable()
	if err != nil || runtime.GOOS != "windows" {
		return source, err
	}
	target, err := ClientExecutable()
	if err != nil {
		return "", err
	}
	if err := syncExecutable(ctx, source, target, version, expectedHash); err != nil {
		return "", fmt.Errorf("无法同步固定程序 %s，已有程序保留：%w", target, err)
	}
	return target, nil
}

func executableHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syncExecutable(ctx context.Context, source, target, version, expectedHash string) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "install.lock"))
	defer lock.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := lock.TryLockContext(ctx, 30*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("另一个 Blip 安装仍在进行")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	hash := executableHash(data)
	if expectedHash != "" && expectedHash != hash {
		return fmt.Errorf("随包程序的 SHA-256 校验失败")
	}
	current, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	currentHash := executableHash(current)
	recordPath := filepath.Join(dir, "versions.json")
	// Journal the old and proposed versions by digest before the single file
	// replacement. A crash or Windows sharing violation leaves the version of
	// the executable actually on disk identifiable on the next invocation.
	var versions map[string]string
	if err := store.ReadJSON(recordPath, &versions); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取程序版本记录：%w", err)
	}
	currentVersion := versions[currentHash]
	if exists && currentHash == hash && currentVersion == version {
		return nil
	}
	if exists && currentHash != hash {
		if currentVersion == "" {
			return fmt.Errorf("无法核实已有 blip.exe 的版本和校验值")
		}
		if semver.Compare("v"+currentVersion, "v"+version) > 0 {
			return nil
		}
	}
	next := map[string]string{hash: version}
	if exists && currentHash != hash {
		next[currentHash] = currentVersion
	}
	if err := store.WriteJSON(recordPath, next); err != nil {
		return err
	}
	if exists && currentHash == hash {
		return nil
	}
	// WriteFileMode stages a synced file in the destination directory and
	// renames it over the target. It never truncates or removes the old file.
	if err := store.WriteFileMode(target, data, 0700); err != nil {
		return fmt.Errorf("替换失败，请检查目录写权限并关闭正在占用该程序的进程后重试：%w", err)
	}
	return nil
}
