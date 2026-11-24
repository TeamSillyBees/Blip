package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/event"
)

type antigravityConversation struct {
	id, parent, title, workspaces string
	depth                         int
	inactive                      bool
}

type summaryReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func conversation(ctx context.Context, db summaryReader, id string) (c antigravityConversation, err error) {
	c.id = id
	err = db.QueryRowContext(ctx, "SELECT parent_conversation_id, nesting_depth, title, workspace_uris, (killed OR status = 'CASCADE_RUN_STATUS_CANCELING') FROM conversation_summaries WHERE conversation_id = ?", id).
		Scan(&c.parent, &c.depth, &c.title, &c.workspaces, &c.inactive)
	c.title = event.NormalizeSessionTitle(c.title)
	return
}

func (c antigravityConversation) root() bool { return c.depth == 0 && c.parent == "" }

// A missing parent row can arrive in a later snapshot. Never promote an
// orphan, a cycle, or a child with only nesting_depth to a user conversation.
func conversationRoot(ctx context.Context, db summaryReader, id string) (antigravityConversation, error) {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		c, err := conversation(ctx, db, id)
		if err == nil && c.inactive {
			return antigravityConversation{}, sql.ErrNoRows
		}
		if err != nil || c.root() {
			return c, err
		}
		id = c.parent
	}
	return antigravityConversation{}, sql.ErrNoRows
}

func (c antigravityConversation) cwd() string {
	var uris []string
	if json.Unmarshal([]byte(c.workspaces), &uris) != nil {
		return ""
	}
	for _, value := range uris {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "file" || u.Path == "" {
			continue
		}
		path := u.Path
		if u.Host != "" && u.Host != "localhost" {
			path = "//" + u.Host + path
		} else if len(path) >= 4 && path[0] == '/' && path[2] == ':' && path[3] == '/' {
			path = path[1:]
		}
		return filepath.FromSlash(path)
	}
	return ""
}

// A returned conversation always identifies a verified root. Give transcript
// lookup a separate budget if SQLite is unavailable during first publication.
func (a *antigravityAdapter) stopRoot(ctx context.Context, id string) antigravityConversation {
	home := a.source.Home
	found := antigravityLookup(ctx, TitleLookupTimeout("antigravity"), func(ctx context.Context) antigravityConversation {
		db, err := antigravity.OpenSummaryDB(ctx, filepath.Join(home, antigravity.SummaryFilename))
		if err != nil {
			return antigravityConversation{}
		}
		defer db.Close()
		c, err := conversation(ctx, db, id)
		if err != nil {
			return antigravityConversation{}
		}
		return c
	})
	if found.id != "" {
		if found.root() && !found.inactive {
			return found
		}
		return antigravityConversation{}
	}
	// Only the origin of step zero counts. A later user reply in a child
	// conversation cannot establish that the user started the conversation.
	explicit := antigravityLookup(ctx, TitleLookupTimeout("antigravity"), func(context.Context) bool {
		f, err := openAntigravityTranscript(home, id)
		if err != nil {
			return false
		}
		defer f.Close()
		var first struct {
			Index  *int   `json:"step_index"`
			Type   string `json:"type"`
			Source string `json:"source"`
		}
		err = json.NewDecoder(io.LimitReader(f, antigravityTranscriptTail)).Decode(&first)
		return err == nil && first.Index != nil && *first.Index == 0 && first.Type == "USER_INPUT" && first.Source == "USER_EXPLICIT"
	})
	if explicit {
		return antigravityConversation{id: id}
	}
	return antigravityConversation{}
}

// The hook and the worker both use this optional resolver for late title generation.
func (a *antigravityAdapter) SessionTitle(ctx context.Context, id string) string {
	if id == "" || !filepath.IsAbs(a.source.Home) || ctx.Err() != nil {
		return ""
	}
	return antigravityLookup(ctx, TitleLookupTimeout("antigravity"), func(ctx context.Context) string {
		db, err := antigravity.OpenSummaryDB(ctx, filepath.Join(a.source.Home, antigravity.SummaryFilename))
		if err != nil {
			return ""
		}
		defer db.Close()
		var title string
		if db.QueryRowContext(ctx, "SELECT title FROM conversation_summaries WHERE conversation_id = ?", id).Scan(&title) != nil {
			return ""
		}
		return event.NormalizeSessionTitle(title)
	})
}
