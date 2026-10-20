package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/local/google-to-domate/internal/models"
	_ "modernc.org/sqlite"
)

const mappingCols = `todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin, google_etag, google_owned, tombstoned_at, tombstone_reason`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS mapping (
  todomate_id TEXT PRIMARY KEY,
  google_event_id TEXT NOT NULL UNIQUE,
  todomate_fingerprint TEXT NOT NULL DEFAULT '',
  google_fingerprint TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  last_origin TEXT,
  google_etag TEXT NOT NULL DEFAULT '',
  google_owned INTEGER NOT NULL DEFAULT 0,
  tombstoned_at TEXT NOT NULL DEFAULT '',
  tombstone_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS sync_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	// Databases created before the safety columns existed.
	for _, col := range []struct{ name, decl string }{
		{"google_etag", "TEXT NOT NULL DEFAULT ''"},
		{"google_owned", "INTEGER NOT NULL DEFAULT 0"},
		{"tombstoned_at", "TEXT NOT NULL DEFAULT ''"},
		{"tombstone_reason", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn("mapping", col.name, col.decl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

func (s *Store) GetByTodomate(id string) (*models.SyncMapping, error) {
	row := s.db.QueryRow(`SELECT `+mappingCols+` FROM mapping WHERE todomate_id=?`, id)
	return scanMapping(row)
}

func (s *Store) GetByGoogle(id string) (*models.SyncMapping, error) {
	row := s.db.QueryRow(`SELECT `+mappingCols+` FROM mapping WHERE google_event_id=?`, id)
	return scanMapping(row)
}

func (s *Store) All() ([]models.SyncMapping, error) {
	rows, err := s.db.Query(`SELECT ` + mappingCols + ` FROM mapping`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.SyncMapping
	for rows.Next() {
		m, err := scanMappingRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) Upsert(m models.SyncMapping) error {
	var synced any
	if !m.LastSyncedAt.IsZero() {
		synced = m.LastSyncedAt.UTC().Format(time.RFC3339)
	}
	owned := 0
	if m.GoogleOwned {
		owned = 1
	}
	tombAt := ""
	if !m.TombstonedAt.IsZero() {
		tombAt = m.TombstonedAt.UTC().Format(time.RFC3339)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// google_event_id is UNIQUE; clear any prior row for either side before insert.
	if _, err := tx.Exec(`DELETE FROM mapping WHERE google_event_id=? OR todomate_id=?`, m.GoogleEventID, m.TodomateID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO mapping (todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin, google_etag, google_owned, tombstoned_at, tombstone_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.TodomateID, m.GoogleEventID, m.TodomateFingerprint, m.GoogleFingerprint, synced, m.LastOrigin, m.GoogleETag, owned, tombAt, m.TombstoneReason); err != nil {
		return err
	}
	return tx.Commit()
}

// Tombstone soft-deletes a link. The row stays so neither side is recreated
// and a failed remote delete cannot look like "already removed".
func (s *Store) Tombstone(todomateID, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`UPDATE mapping SET tombstoned_at=?, tombstone_reason=? WHERE todomate_id=? AND tombstoned_at=''`, now, reason, todomateID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("tombstone: mapping %s not updated", todomateID)
	}
	return nil
}

func (s *Store) DeleteByTodomate(id string) error {
	_, err := s.db.Exec(`DELETE FROM mapping WHERE todomate_id=?`, id)
	return err
}

func (s *Store) DeleteByGoogle(id string) error {
	_, err := s.db.Exec(`DELETE FROM mapping WHERE google_event_id=?`, id)
	return err
}

func (s *Store) GetState(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM sync_state WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetState(key, value string) error {
	_, err := s.db.Exec(`
INSERT INTO sync_state(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMapping(row scanner) (*models.SyncMapping, error) {
	var m models.SyncMapping
	var synced, origin, etag, tombAt, tombReason sql.NullString
	var owned int
	err := row.Scan(
		&m.TodomateID, &m.GoogleEventID, &m.TodomateFingerprint, &m.GoogleFingerprint,
		&synced, &origin, &etag, &owned, &tombAt, &tombReason,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if synced.Valid && synced.String != "" {
		t, _ := time.Parse(time.RFC3339, synced.String)
		m.LastSyncedAt = t
	}
	if origin.Valid {
		m.LastOrigin = origin.String
	}
	if etag.Valid {
		m.GoogleETag = etag.String
	}
	m.GoogleOwned = owned != 0
	if tombAt.Valid && tombAt.String != "" {
		t, _ := time.Parse(time.RFC3339, tombAt.String)
		m.TombstonedAt = t
	}
	if tombReason.Valid {
		m.TombstoneReason = tombReason.String
	}
	return &m, nil
}

func scanMappingRows(rows *sql.Rows) (*models.SyncMapping, error) {
	return scanMapping(rows)
}
