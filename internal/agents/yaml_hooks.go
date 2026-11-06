package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
	"go.yaml.in/yaml/v3"
)

type yamlHookInstaller struct {
	config config.Config
	source config.Source
	path   string
}

func (i yamlHookInstaller) ownership() jsonHookInstaller {
	return jsonHookInstaller{config: i.config, source: i.source, path: i.path}
}

func readHookYAML(path string) (*yaml.Node, []byte, os.FileMode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}, nil, 0600, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, 0, err
	}
	if !info.Mode().IsRegular() || len(data) > 4*1024*1024 {
		return nil, nil, 0, fmt.Errorf("Hermes settings must be a regular file no larger than 4 MiB")
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, nil, 0, fmt.Errorf("invalid Hermes config.yaml; no settings changed")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, 0, fmt.Errorf("Hermes config.yaml must contain one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, 0, fmt.Errorf("Hermes config.yaml must be a mapping")
	}
	// Decode also rejects duplicate mapping keys. Nodes retain comments and
	// scalar forms rather than resolving and rewriting arbitrary user settings.
	var check map[string]any
	if err := doc.Decode(&check); err != nil {
		return nil, nil, 0, fmt.Errorf("invalid or duplicate Hermes settings keys")
	}
	return &doc, data, info.Mode().Perm(), nil
}
func yamlKey(node *yaml.Node, key string) (int, *yaml.Node) {
	for n := 0; n+1 < len(node.Content); n += 2 {
		if node.Content[n].Value == key {
			return n, node.Content[n+1]
		}
	}
	return -1, nil
}
func yamlHooks(doc *yaml.Node, create bool) (*yaml.Node, error) {
	root := doc.Content[0]
	if _, merge := yamlKey(root, "<<"); merge != nil {
		return nil, fmt.Errorf("top-level YAML merges require explicit hook settings before installation")
	}
	_, hooks := yamlKey(root, "hooks")
	if hooks == nil && create {
		hooks = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hooks"}, hooks)
	}
	if hooks != nil && hooks.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Hermes hooks must be an explicit mapping")
	}
	return hooks, nil
}
func yamlHandler(node *yaml.Node) (map[string]any, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Hermes hook entry must be an explicit mapping")
	}
	if _, merge := yamlKey(node, "<<"); merge != nil {
		return nil, fmt.Errorf("hook entries cannot use YAML merges")
	}
	var result map[string]any
	if err := node.Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid Hermes hook entry")
	}
	if _, ok := result["<<"]; ok {
		return nil, fmt.Errorf("hook entries cannot use YAML merges")
	}
	return result, nil
}

func (i yamlHookInstaller) edit(ctx context.Context, entries []jsonHookEntry, install bool) error {
	if err := os.MkdirAll(filepath.Dir(i.path), 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(filepath.Dir(i.path), ".blip-hooks.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		if err == nil {
			err = fmt.Errorf("Hermes settings are busy")
		}
		return err
	}
	defer lock.Close()
	owner := i.ownership()
	previous, err := owner.manifest()
	if err != nil {
		return err
	}
	doc, original, mode, err := readHookYAML(i.path)
	if err != nil {
		return err
	}
	hooks, err := yamlHooks(doc, install)
	if err != nil {
		return err
	}
	owned := append(append([]jsonHookEntry{}, previous.Entries...), entries...)
	if hooks != nil {
		keptEvents := []*yaml.Node{}
		for n := 0; n+1 < len(hooks.Content); n += 2 {
			key, list := hooks.Content[n], hooks.Content[n+1]
			if key.Value == "outbound" || key.Value == "output_spill" {
				keptEvents = append(keptEvents, key, list)
				continue
			}
			if list.Kind != yaml.SequenceNode {
				return fmt.Errorf("Hermes hook event %s must be a sequence", key.Value)
			}
			kept := []*yaml.Node{}
			removed := false
			for _, node := range list.Content {
				handler, err := yamlHandler(node)
				if err != nil {
					return err
				}
				match := false
				for _, entry := range owned {
					if entry.Event == key.Value && jsonEqual(entry.Hook, handler) {
						match = true
						break
					}
				}
				if !match {
					for _, entry := range previous.Entries {
						if sameHookCommand(handler, entry.Hook) {
							return fmt.Errorf("Blip Hermes hook was edited or moved; restore its recorded entry first")
						}
					}
					kept = append(kept, node)
				} else {
					removed = true
				}
			}
			list.Content = kept
			if !removed || len(kept) > 0 {
				keptEvents = append(keptEvents, key, list)
			}
		}
		hooks.Content = keptEvents
		if install {
			for _, entry := range entries {
				_, list := yamlKey(hooks, entry.Event)
				if list == nil {
					list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
					hooks.Content = append(hooks.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry.Event}, list)
				}
				var node yaml.Node
				if err := node.Encode(entry.Hook); err != nil {
					return err
				}
				list.Content = append(list.Content, &node)
			}
		}
		if len(hooks.Content) == 0 {
			root := doc.Content[0]
			idx, _ := yamlKey(root, "hooks")
			root.Content = append(root.Content[:idx], root.Content[idx+2:]...)
		}
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	updated := buffer.Bytes()
	if install {
		if err := store.WriteJSON(owner.manifestPath(), jsonHookManifest{Format: 1, Path: i.path, Entries: uniqueHookEntries(owned)}); err != nil {
			return err
		}
	}
	if !bytes.Equal(original, updated) && !(len(original) == 0 && !install) {
		if len(original) > 0 {
			if err := store.WriteFile(i.path+".blip-backup-"+time.Now().UTC().Format("20060102T150405.000000000"), original); err != nil {
				return fmt.Errorf("back up Hermes settings: %w", err)
			}
		}
		_, current, _, err := readHookYAML(i.path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fmt.Errorf("Hermes settings changed concurrently; retry using the newer settings")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := store.WriteFileMode(i.path, updated, mode); err != nil {
			return err
		}
	}
	if install {
		return store.WriteJSON(owner.manifestPath(), jsonHookManifest{Format: 1, Path: i.path, Entries: entries})
	}
	if err := os.Remove(owner.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (i yamlHookInstaller) status() string {
	m, err := i.ownership().manifest()
	if err != nil {
		return "invalid_manifest"
	}
	if len(m.Entries) == 0 {
		return "not_installed"
	}
	doc, _, _, err := readHookYAML(i.path)
	if err != nil {
		return "invalid_configuration"
	}
	hooks, err := yamlHooks(doc, false)
	if err != nil || hooks == nil {
		return "missing_configuration"
	}
	for _, entry := range m.Entries {
		_, list := yamlKey(hooks, entry.Event)
		if list == nil || list.Kind != yaml.SequenceNode {
			return "missing_configuration"
		}
		found := false
		for _, node := range list.Content {
			handler, err := yamlHandler(node)
			if err == nil && jsonEqual(handler, entry.Hook) {
				found = true
			}
		}
		if !found {
			return "missing_configuration"
		}
	}
	return "configured_unverified"
}
