package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

// ErrConflict means the file changed after the editor opened it. Open a fresh
// document and review the newer settings before trying to save again.
var ErrConflict = errors.New("configuration changed while editing; reopen it before saving")

// Document holds editable, unresolved configuration. Opening a document never
// creates a file, applies defaults, resolves paths, or reads credential values.
// Change Config and call Save to publish it; discard it to cancel. A Document
// should be used by only one goroutine at a time.
type Document struct {
	Config Config

	path     string
	existed  bool
	original []byte
	initial  Config
}

// OpenDocument reads a configuration for editing. A missing file starts with
// schema 1 and an empty destination list; runtime defaults remain omitted.
func OpenDocument(path string) (*Document, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("locate configuration: %w", err)
	}
	data, existed, err := readDocumentFile(absolute)
	if err != nil {
		return nil, err
	}
	c := Config{SchemaVersion: 1, Destinations: []Destination{}, Path: absolute}
	if existed {
		c = Config{}
		if err := decodeDocument(data, &c); err != nil {
			return nil, err
		}
		c.Path = absolute
		if err := validateStructure(c); err != nil {
			return nil, err
		}
	}
	return &Document{Config: c, path: absolute, existed: existed, original: data, initial: c.Clone()}, nil
}

// Save validates raw settings, then publishes the complete document atomically.
// Cooperating editors share a file lock and stale snapshots are rejected. An
// existing file is backed up to <path>.bak only when its settings change. Both
// the new file and backup use mode 0600 where the OS supports Unix permissions.
// No environment expansion occurs here.
func (d *Document) Save(ctx context.Context) error {
	if d == nil || d.path == "" {
		return fmt.Errorf("configuration document must be opened before saving")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.validate(); err != nil {
		return err
	}
	unchanged := sameConfig(d.Config, d.initial)
	var data []byte
	var err error
	if !d.existed || !unchanged {
		data, err = d.encode()
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0700); err != nil {
		return fmt.Errorf("prepare configuration directory: %w", err)
	}
	lock := flock.New(d.path + ".lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock configuration: %w", err)
	}
	if !locked {
		_ = lock.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("configuration is busy")
	}
	defer lock.Close()
	current, exists, err := readDocumentFile(d.path)
	if err != nil {
		return err
	}
	if exists != d.existed || !bytes.Equal(current, d.original) {
		return ErrConflict
	}
	if d.existed && unchanged {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.existed {
		if err := store.WriteFile(d.path+".bak", d.original); err != nil {
			return fmt.Errorf("back up configuration: %w", err)
		}
	}
	// Recheck after preparing the backup to catch noncooperating file writers
	// that changed the configuration during that operation.
	current, exists, err = readDocumentFile(d.path)
	if err != nil {
		return err
	}
	if exists != d.existed || !bytes.Equal(current, d.original) {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.WriteFile(d.path, data); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	d.original = data
	d.existed = true
	d.Config.Path = d.path
	d.initial = d.Config.Clone()
	return nil
}

func (d *Document) validate() error {
	if err := validateStructure(d.Config); err != nil {
		return err
	}
	original := make(map[string]Destination, len(d.initial.Destinations))
	for _, destination := range d.initial.Destinations {
		original[destination.ID] = destination
	}
	for _, destination := range d.Config.Destinations {
		previous, exists := original[destination.ID]
		previous.Enabled = false
		if !destination.Enabled && exists && reflect.DeepEqual(destination, previous) {
			// Preserve incomplete disabled examples and allow a broken enabled
			// destination to be disabled without first repairing its settings.
			continue
		}
		if err := ValidateDestination(destination, false); err != nil {
			return fmt.Errorf("validate destination: %w", err)
		}
	}
	return nil
}

func sameConfig(a, b Config) bool {
	a.Path, b.Path = "", ""
	return reflect.DeepEqual(a, b)
}

func readDocumentFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("configuration editing requires a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read configuration: %w", err)
	}
	return data, true, nil
}

func decodeDocument(data []byte, c *Config) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(c); err != nil {
		// Decoder errors can echo unknown field names; those may be secrets.
		return fmt.Errorf("invalid configuration JSON or unknown configuration field")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("configuration must contain exactly one JSON object")
	}
	// encoding/json accepts case-insensitive field aliases. Reject those here
	// so edits cannot introduce two differently spelled keys for one setting.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("invalid configuration JSON")
	}
	for name := range fields {
		switch name {
		case "schema_version", "state_dir", "codex_home", "poll_interval", "request_timeout", "max_attempts", "destinations":
		default:
			return fmt.Errorf("unknown configuration field")
		}
	}
	var destinations []map[string]json.RawMessage
	if raw, ok := fields["destinations"]; ok {
		if err := json.Unmarshal(raw, &destinations); err != nil {
			return fmt.Errorf("invalid destination JSON")
		}
	}
	for _, destination := range destinations {
		for name := range destination {
			switch name {
			case "id", "kind", "enabled", "url", "token", "secret", "headers", "priority":
			default:
				return fmt.Errorf("unknown destination field")
			}
		}
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	if err := checkUniqueKeys(decoder); err != nil {
		return fmt.Errorf("configuration JSON contains duplicate object fields")
	}
	return nil
}

func checkUniqueKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return fmt.Errorf("duplicate field")
			}
			keys[name] = true
		}
		if err := checkUniqueKeys(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// Merge changed known fields into the original objects so omitted defaults and
// explicit zero values survive unrelated edits. Unknown fields were rejected at
// open time rather than silently discarded.
func (d *Document) encode() ([]byte, error) {
	if !d.existed {
		data, err := json.MarshalIndent(d.Config, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("cannot encode configuration")
		}
		return append(data, '\n'), nil
	}
	fields := make(map[string]json.RawMessage)
	if d.existed {
		if err := json.Unmarshal(d.original, &fields); err != nil {
			return nil, fmt.Errorf("cannot encode configuration")
		}
	}
	before, _ := configFields(d.initial)
	after, err := configFields(d.Config)
	if err != nil {
		return nil, err
	}
	for name, value := range after {
		if name != "destinations" && (!d.existed || !bytes.Equal(value, before[name])) {
			fields[name] = value
		}
	}
	if !d.existed || !reflect.DeepEqual(d.Config.Destinations, d.initial.Destinations) {
		var oldObjects []map[string]json.RawMessage
		if raw, ok := fields["destinations"]; ok {
			_ = json.Unmarshal(raw, &oldObjects)
		}
		oldByID := make(map[string]int)
		for i, destination := range d.initial.Destinations {
			oldByID[destination.ID] = i
		}
		objects := make([]map[string]json.RawMessage, 0, len(d.Config.Destinations))
		for _, destination := range d.Config.Destinations {
			object := make(map[string]json.RawMessage)
			var previous Destination
			index, exists := oldByID[destination.ID]
			if !exists {
				data, _ := json.Marshal(destination)
				_ = json.Unmarshal(data, &object)
				objects = append(objects, object)
				continue
			}
			if exists && index < len(oldObjects) {
				object = oldObjects[index]
				previous = d.initial.Destinations[index]
			}
			oldFields, _ := destinationFields(previous)
			newFields, _ := destinationFields(destination)
			for name, value := range newFields {
				if !exists || !bytes.Equal(value, oldFields[name]) {
					object[name] = value
				}
			}
			objects = append(objects, object)
		}
		fields["destinations"], _ = json.Marshal(objects)
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cannot encode configuration")
	}
	return append(data, '\n'), nil
}

func configFields(c Config) (map[string]json.RawMessage, error) {
	return encodeFields(map[string]any{
		"schema_version": c.SchemaVersion, "state_dir": c.StateDir,
		"codex_home": c.CodexHome, "poll_interval": c.PollInterval,
		"request_timeout": c.RequestTimeout, "max_attempts": c.MaxAttempts,
	})
}

func destinationFields(d Destination) (map[string]json.RawMessage, error) {
	return encodeFields(map[string]any{
		"id": d.ID, "kind": d.Kind, "enabled": d.Enabled, "url": d.URL,
		"token": d.Token, "secret": d.Secret, "headers": d.Headers, "priority": d.Priority,
	})
}

func encodeFields(values map[string]any) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("cannot encode configuration")
		}
		fields[name] = data
	}
	return fields, nil
}
