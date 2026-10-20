package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/local/google-to-domate/internal/models"
	_ "modernc.org/sqlite"
)

func TestMigrationAndTombstone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE mapping (
  todomate_id TEXT PRIMARY KEY,
  google_event_id TEXT NOT NULL UNIQUE,
  todomate_fingerprint TEXT NOT NULL DEFAULT '',
  google_fingerprint TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  last_origin TEXT
)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Upsert(models.SyncMapping{TodomateID: "t1", GoogleEventID: "g1", GoogleOwned: true, GoogleETag: "\"e\""}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone("t1", "google_cancelled"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetByTodomate("t1")
	if err != nil || got == nil || !got.Tombstoned() || !got.GoogleOwned || got.GoogleETag != "\"e\"" || got.TombstoneReason != "google_cancelled" {
		t.Fatalf("got = %+v err=%v", got, err)
	}
}
