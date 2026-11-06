// Package agents selects built-in harness adapters. Queue storage and sending
// intentionally do not depend on this registry, so removed sources still drain.
package agents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

var ErrUnavailable = errors.New("harness adapter is not available")
var ErrCheckpoint = errors.New("source checkpoint is invalid; collection paused")

type Capability struct {
	Event      string `json:"event"`
	Support    string `json:"support"`
	Evidence   string `json:"evidence"`
	Validation string `json:"validation"`
}

type Adapter interface {
	ParseHook(context.Context, []byte, time.Time) ([]event.Event, error)
	Capabilities() []Capability
}
type Collector interface {
	Poll(context.Context, func(event.Event) error) error
}
type Installer interface {
	Install(context.Context, string) error
	Uninstall(context.Context) error
	InstallationStatus() string
}
type TitleResolver interface {
	SessionTitle(context.Context, string) string
}
type ReadPath struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Optional  bool   `json:"optional,omitempty"`
}
type FileReader interface{ ReadPaths() []ReadPath }

type Factory func(config.Config, config.Source) Adapter
type Registry map[string]Factory

func Builtins() Registry { return Registry{"codex": newCodex} }

func (r Registry) Open(c config.Config, source config.Source) (Adapter, error) {
	create := r[source.Harness]
	if create == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, source.Harness)
	}
	if source.EventVersion == 0 {
		source.EventVersion = 2
	}
	return create(c, source), nil
}

// StateDir retains the default source's old installation and cursor paths.
func StateDir(c config.Config, source config.Source) string {
	if source.ID == config.DefaultSourceID {
		return c.StateDir
	}
	return filepath.Join(c.StateDir, "sources", source.ID)
}

func SourceForEvent(c config.Config, e event.Event) (config.Source, bool) {
	id := e.SourceID
	if e.SchemaVersion == 1 && e.Agent == "codex" {
		id = config.DefaultSourceID
	}
	s, ok := c.Source(id)
	return s, ok && s.Harness == e.Agent
}

func ResolveTitle(ctx context.Context, c config.Config, e event.Event) string {
	source, ok := SourceForEvent(c, e)
	if !ok {
		return ""
	}
	if err := CheckBinding(c, source); err != nil {
		return ""
	}
	adapter, err := Builtins().Open(c, source)
	if err != nil {
		return ""
	}
	if resolver, ok := adapter.(TitleResolver); ok {
		return resolver.SessionTitle(ctx, e.SessionID)
	}
	return ""
}
