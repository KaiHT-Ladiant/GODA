package store

import (
	"database/sql"
	"time"

	"github.com/local/google-to-domate/internal/models"
	_ "modernc.org/sqlite"
)

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
  last_origin TEXT
);
CREATE TABLE IF NOT EXISTS sync_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`)
	return err
}

func (s *Store) GetByTodomate(id string) (*models.SyncMapping, error) {
	row := s.db.QueryRow(`SELECT todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin FROM mapping WHERE todomate_id=?`, id)
	return scanMapping(row)
}

func (s *Store) GetByGoogle(id string) (*models.SyncMapping, error) {
	row := s.db.QueryRow(`SELECT todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin FROM mapping WHERE google_event_id=?`, id)
	return scanMapping(row)
}

func (s *Store) All() ([]models.SyncMapping, error) {
	rows, err := s.db.Query(`SELECT todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin FROM mapping`)
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
	_, err := s.db.Exec(`
INSERT INTO mapping (todomate_id, google_event_id, todomate_fingerprint, google_fingerprint, last_synced_at, last_origin)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(todomate_id) DO UPDATE SET
  google_event_id=excluded.google_event_id,
  todomate_fingerprint=excluded.todomate_fingerprint,
  google_fingerprint=excluded.google_fingerprint,
  last_synced_at=excluded.last_synced_at,
  last_origin=excluded.last_origin`,
		m.TodomateID, m.GoogleEventID, m.TodomateFingerprint, m.GoogleFingerprint, synced, m.LastOrigin)
	return err
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
	var synced sql.NullString
	var origin sql.NullString
	err := row.Scan(&m.TodomateID, &m.GoogleEventID, &m.TodomateFingerprint, &m.GoogleFingerprint, &synced, &origin)
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
	return &m, nil
}

func scanMappingRows(rows *sql.Rows) (*models.SyncMapping, error) {
	return scanMapping(rows)
}
