package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

var ErrBinding = errors.New("source identity binding is invalid or changed; preserve the existing source state")

type binding struct {
	Format        int           `json:"format"`
	Source        config.Source `json:"source"`
	CanonicalHome string        `json:"canonical_home"`
}

func bindingPath(c config.Config, id string) string {
	return filepath.Join(c.StateDir, "sources", id, "binding.json")
}

// CheckBinding is read-only and permits an unbound legacy source. It prevents
// optional title lookup from crossing into a rebound home before worker startup.
func CheckBinding(c config.Config, source config.Source) error {
	if !config.ValidSourceID(source.ID) {
		return ErrBinding
	}
	var previous binding
	err := store.ReadJSON(bindingPath(c, source.ID), &previous)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrBinding
	}
	home, err := config.CanonicalHome(source.Home)
	if err != nil {
		return err
	}
	if previous.Format != 1 || previous.Source.ID != source.ID || previous.Source.Harness != source.Harness || previous.CanonicalHome != home {
		return ErrBinding
	}
	return nil
}

// Bind reserves an ID permanently, including after removal from configuration.
// A callback must never inherit another home, checkpoint or installation record.
func Bind(ctx context.Context, c config.Config, source config.Source) error {
	if !config.ValidSourceID(source.ID) || source.Home == "" || (source.ID == config.DefaultSourceID && source.Harness != "codex") {
		return ErrBinding
	}
	home, err := config.CanonicalHome(source.Home)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(c.StateDir, "sources"), 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(c.StateDir, "sources", "bindings.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !ok {
		_ = lock.Close()
		return fmt.Errorf("source bindings are busy")
	}
	defer lock.Close()
	path := bindingPath(c, source.ID)
	var previous binding
	err = store.ReadJSON(path, &previous)
	if err == nil {
		if previous.Format != 1 || previous.Source.ID != source.ID || previous.Source.Harness != source.Harness || previous.CanonicalHome != home {
			return ErrBinding
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ErrBinding
	}
	// Validate a legacy manifest before reserving its first explicit binding.
	if source.ID == config.DefaultSourceID {
		var legacy struct {
			CodexHome string `json:"codex_home"`
		}
		err = store.ReadJSON(filepath.Join(c.StateDir, "installation.json"), &legacy)
		if err == nil {
			oldHome, homeErr := config.CanonicalHome(legacy.CodexHome)
			if legacy.CodexHome == "" || homeErr != nil || oldHome != home {
				return ErrBinding
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrBinding
		}
	}
	return store.WriteJSON(path, binding{Format: 1, Source: source, CanonicalHome: home})
}

// InstalledSource also works after the source was removed from configuration.
func InstalledSource(c config.Config, id string) (config.Source, error) {
	if !config.ValidSourceID(id) {
		return config.Source{}, ErrBinding
	}
	var saved binding
	err := store.ReadJSON(bindingPath(c, id), &saved)
	if err == nil {
		home, homeErr := config.CanonicalHome(saved.Source.Home)
		if saved.Format != 1 || saved.Source.ID != id || homeErr != nil || home != saved.CanonicalHome {
			return config.Source{}, ErrBinding
		}
		return saved.Source, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return config.Source{}, ErrBinding
	}
	if id == config.DefaultSourceID {
		var legacy struct {
			CodexHome string `json:"codex_home"`
		}
		if err = store.ReadJSON(filepath.Join(c.StateDir, "installation.json"), &legacy); err == nil && legacy.CodexHome != "" {
			return config.Source{ID: id, Harness: "codex", Home: legacy.CodexHome, EventVersion: 1}, nil
		}
	}
	if source, ok := c.Source(id); ok {
		return source, nil
	}
	return config.Source{}, fmt.Errorf("source %q has no configuration or saved installation", id)
}
