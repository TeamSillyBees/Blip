package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/ncruces/go-sqlite3"
)

// The native hook supplies a conversation ID. Both the app and the CLI keep
// the display name in the summary database under their respective homes.
func (a *antigravityAdapter) SessionTitle(ctx context.Context, id string) string {
	if id == "" || !filepath.IsAbs(a.source.Home) || ctx.Err() != nil {
		return ""
	}
	return antigravityLookup(ctx, TitleLookupTimeout("antigravity"), func(ctx context.Context) string {
		return readAntigravityTitle(ctx, a.source.Home, id)
	})
}

func readAntigravityTitle(ctx context.Context, home, id string) string {
	// The configured home may be a symlink. Only open the database inside it.
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return ""
	}
	path := filepath.Join(home, "conversation_summaries.db")
	// SQLite may also open a journal, WAL or shared-memory file beside the DB.
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return ""
		}
	}
	if ctx.Err() != nil {
		return ""
	}
	db, err := sqlite3.OpenFlags(path, sqlite3.OPEN_READONLY|sqlite3.OPEN_NOFOLLOW)
	if err != nil {
		return ""
	}
	defer db.Close()
	db.SetInterrupt(ctx)
	if err := db.BusyTimeout(50 * time.Millisecond); err != nil {
		return ""
	}
	stmt, _, err := db.Prepare("SELECT title FROM conversation_summaries WHERE conversation_id = ?")
	if err != nil {
		return ""
	}
	defer stmt.Close()
	if err := stmt.BindText(1, id); err != nil || !stmt.Step() || stmt.ColumnType(0) != sqlite3.TEXT {
		return ""
	}
	return event.NormalizeSessionTitle(stmt.ColumnText(0))
}
