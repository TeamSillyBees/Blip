package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteJSON publishes a complete file, so readers never observe a partial JSON document.
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(data, '\n'))
}

func WriteFile(path string, data []byte) error {
	return WriteFileMode(path, data, 0600)
}

// WriteFileMode preserves an existing external settings file's Unix mode while
// publishing atomically. Internal state continues to use private mode 0600.
func WriteFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".blip-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(mode.Perm()); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("publish local state: %w", err)
	}
	return nil
}

func ReadJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
