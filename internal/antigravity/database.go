package antigravity

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

const SummaryFilename = "conversation_summaries.db"

// SummaryDB pins one SQLite connection: PRAGMA data_version values from
// different connections cannot be compared. The native Windows VFS can read
// a live WAL even when the service SID only has read access to both sidecars.
type SummaryDB struct {
	*sql.Conn
	pool *sql.DB
}

func OpenSummaryDB(ctx context.Context, path string) (*SummaryDB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	home, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(home, filepath.Base(abs))
	// SQLite opens its own sidecars. Only allow regular files in the bound home.
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		info, err := os.Lstat(abs + suffix)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("invalid Antigravity database file")
		}
	}
	name := filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name // Windows drive letter in a file URI.
	}
	uri := url.URL{Scheme: "file", Path: name, RawQuery: "mode=ro"}
	pool, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &SummaryDB{Conn: conn, pool: pool}, nil
}

func (db *SummaryDB) Close() error {
	return errors.Join(db.Conn.Close(), db.pool.Close())
}

// PrepareServiceWAL lets SQLite open its coordination files under the
// installing user's identity before file-specific service ACLs are assigned.
// This also handles a cleanly closed client with only the main database left.
func PrepareServiceWAL(ctx context.Context, path string) error {
	db, err := OpenSummaryDB(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Raw(func(conn any) error {
		_, err := conn.(sqlite.FileControl).FileControlPersistWAL("main", 1)
		return err
	}); err != nil {
		return err
	}
	var version int64
	return db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
}
