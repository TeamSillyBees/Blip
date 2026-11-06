package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
	"go.yaml.in/yaml/v3"
)

type dshInstaller struct {
	config config.Config
	source config.Source
	path   string
}
type dshManifest struct {
	Format  int              `json:"format"`
	Path    string           `json:"path"`
	Entries []map[string]any `json:"entries"`
	// Content-addressed files remain recoverable across an interrupted upgrade.
	Files []string `json:"files"`
}

func (i dshInstaller) managedDir() string {
	return filepath.Join(i.source.Home, ".blip-hooks", i.source.ID)
}
func (i dshInstaller) manifestPath() string {
	return filepath.Join(StateDir(i.config, i.source), "installation.json")
}
func (i dshInstaller) rowID() string { return "blip-" + i.source.ID }
func (i dshInstaller) manifest() (dshManifest, error) {
	m := dshManifest{Format: 1, Path: i.path}
	if err := store.ReadJSON(i.manifestPath(), &m); errors.Is(err, os.ErrNotExist) {
		return m, nil
	} else if err != nil {
		return m, fmt.Errorf("invalid DSH installation manifest")
	}
	if m.Format != 1 || m.Path != i.path {
		return m, fmt.Errorf("invalid DSH installation manifest")
	}
	for _, row := range m.Entries {
		if row["id"] != i.rowID() {
			return m, fmt.Errorf("invalid DSH ownership entry")
		}
	}
	for _, path := range m.Files {
		if filepath.Dir(path) != i.managedDir() || !strings.HasSuffix(path, ".mjs") || len(filepath.Base(path)) != 68 {
			return m, fmt.Errorf("invalid managed DSH plugin path")
		}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return m, err
		}
		if err == nil && filepath.Base(path) != event.ID(string(data))+".mjs" {
			return m, fmt.Errorf("Blip DSH plugin was edited; restore it before updating or uninstalling")
		}
	}
	return m, nil
}

func readDSHPatch(path string) (*yaml.Node, []byte, os.FileMode, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.SequenceNode, Tag: "!!seq"}}}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, 0600, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return nil, nil, 0, fmt.Errorf("DSH patch must be a regular file no larger than 4 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, data, info.Mode().Perm(), nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err = decoder.Decode(doc); err != nil {
		return nil, nil, 0, fmt.Errorf("invalid DSH patch YAML")
	}
	var trailing yaml.Node
	if decoder.Decode(&trailing) != io.EOF || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.SequenceNode {
		return nil, nil, 0, fmt.Errorf("DSH patch must be one YAML sequence")
	}
	return doc, data, info.Mode().Perm(), nil
}

// Remove only complete inserted rows recorded in the manifest. Unrelated
// patches, comments and !!js nodes remain nodes and are never evaluated.
func (i dshInstaller) removeOwned(doc *yaml.Node, m dshManifest) (int, error) {
	count := 0
	kept := []*yaml.Node{}
	for _, patch := range doc.Content[0].Content {
		if patch.Kind != yaml.MappingNode {
			return 0, fmt.Errorf("DSH patch entries must be explicit mappings")
		}
		_, insert := yamlKey(patch, "insert")
		if insert == nil {
			kept = append(kept, patch)
			continue
		}
		if insert.Kind != yaml.SequenceNode {
			return 0, fmt.Errorf("DSH insert must be an explicit sequence")
		}
		rows := []*yaml.Node{}
		for _, row := range insert.Content {
			if row.Kind != yaml.MappingNode {
				return 0, fmt.Errorf("DSH inserted row must be an explicit mapping")
			}
			_, id := yamlKey(row, "id")
			if id == nil || id.Value != i.rowID() {
				rows = append(rows, row)
				continue
			}
			var value map[string]any
			if row.Decode(&value) != nil {
				return 0, fmt.Errorf("invalid managed DSH row")
			}
			owned := false
			for _, entry := range m.Entries {
				if jsonEqual(value, entry) {
					owned = true
					break
				}
			}
			if !owned {
				return 0, fmt.Errorf("Blip DSH row was edited or its ID already exists; restore the recorded row before changing it")
			}
			count++
		}
		insert.Content = rows
		if len(rows) > 0 || len(patch.Content) > 2 {
			kept = append(kept, patch)
		}
	}
	doc.Content[0].Content = kept
	return count, nil
}

func (i dshInstaller) edit(ctx context.Context, executable string, install bool) error {
	if !config.ValidSourceID(i.source.ID) {
		return fmt.Errorf("invalid source ID")
	}
	if install {
		if info, err := os.Stat(filepath.Join(i.source.Home, "cordis.yml")); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("DSH home must be an existing profile containing cordis.yml")
		}
	}
	if err := os.MkdirAll(i.source.Home, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(i.source.Home, ".blip-hooks.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		return fmt.Errorf("DSH patch is busy")
	}
	defer lock.Close()
	m, err := i.manifest()
	if err != nil {
		return err
	}
	doc, original, mode, err := readDSHPatch(i.path)
	if err != nil {
		return err
	}
	if _, err = i.removeOwned(doc, m); err != nil {
		return err
	}
	if install {
		executable, err = filepath.Abs(executable)
		if err != nil {
			return err
		}
		configPath, err := filepath.Abs(i.config.Path)
		if err != nil {
			return err
		}
		pluginPath := filepath.Join(i.managedDir(), event.ID(string(dshPlugin))+".mjs")
		relative, err := filepath.Rel(i.source.Home, pluginPath)
		if err != nil {
			return err
		}
		entry := map[string]any{"id": i.rowID(), "name": "./" + filepath.ToSlash(relative), "config": map[string]any{"executable": executable, "configPath": configPath, "sourceId": i.source.ID}}
		var patch yaml.Node
		if err = patch.Encode(map[string]any{"insert": []map[string]any{entry}}); err != nil {
			return err
		}
		doc.Content[0].Content = append(doc.Content[0].Content, &patch)
		found := false
		for _, p := range m.Files {
			if p == pluginPath {
				found = true
			}
		}
		if !found {
			m.Files = append(m.Files, pluginPath)
		}
		found = false
		for _, r := range m.Entries {
			if jsonEqual(r, entry) {
				found = true
			}
		}
		if !found {
			m.Entries = append(m.Entries, entry)
		}
		if data, readErr := os.ReadFile(pluginPath); readErr == nil && !bytes.Equal(data, dshPlugin) {
			return fmt.Errorf("managed DSH plugin content conflicts")
		} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		// Journal before publishing either file so a later run can recover.
		if err = store.WriteJSON(i.manifestPath(), m); err != nil {
			return err
		}
		if err = store.WriteFile(pluginPath, dshPlugin); err != nil {
			return err
		}
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err = encoder.Encode(doc); err != nil {
		return err
	}
	if err = encoder.Close(); err != nil {
		return err
	}
	updated := buffer.Bytes()
	if !bytes.Equal(original, updated) && (install || len(original) > 0) {
		if len(original) > 0 {
			if err = store.WriteFile(i.path+".blip-backup-"+time.Now().UTC().Format("20060102T150405.000000000"), original); err != nil {
				return err
			}
		}
		_, current, _, err := readDSHPatch(i.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fmt.Errorf("DSH patch changed concurrently; retry")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = store.WriteFileMode(i.path, updated, mode); err != nil {
			return err
		}
	}
	if !install {
		for _, path := range m.Files {
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err = os.Remove(i.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (i dshInstaller) status() string {
	if _, err := os.Stat(i.manifestPath()); errors.Is(err, os.ErrNotExist) {
		return "not_installed"
	}
	m, err := i.manifest()
	if err != nil {
		return "conflict"
	}
	doc, _, _, err := readDSHPatch(i.path)
	if err != nil {
		return "conflict"
	}
	n, err := i.removeOwned(doc, m)
	if err != nil {
		return "conflict"
	}
	if n == 0 {
		return "missing"
	}
	for _, path := range m.Files {
		if _, err := os.Stat(path); err != nil {
			return "missing"
		}
	}
	return "configured_unverified"
}
