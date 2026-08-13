from __future__ import annotations

import sqlite3
from datetime import datetime, timezone
from pathlib import Path

from google_to_domate.models import SyncMapping


class MappingStore:
    def __init__(self, db_path: Path) -> None:
        self.db_path = db_path
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(str(db_path))
        self._conn.row_factory = sqlite3.Row
        self._init_schema()

    def close(self) -> None:
        self._conn.close()

    def _init_schema(self) -> None:
        self._conn.executescript(
            """
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
            );
            """
        )
        self._conn.commit()

    def get_by_todomate(self, todomate_id: str) -> SyncMapping | None:
        row = self._conn.execute(
            "SELECT * FROM mapping WHERE todomate_id = ?", (todomate_id,)
        ).fetchone()
        return self._row_to_mapping(row) if row else None

    def get_by_google(self, google_event_id: str) -> SyncMapping | None:
        row = self._conn.execute(
            "SELECT * FROM mapping WHERE google_event_id = ?", (google_event_id,)
        ).fetchone()
        return self._row_to_mapping(row) if row else None

    def all_mappings(self) -> list[SyncMapping]:
        rows = self._conn.execute("SELECT * FROM mapping").fetchall()
        return [self._row_to_mapping(r) for r in rows]

    def upsert(self, mapping: SyncMapping) -> None:
        synced = (
            mapping.last_synced_at.astimezone(timezone.utc).isoformat()
            if mapping.last_synced_at
            else None
        )
        self._conn.execute(
            """
            INSERT INTO mapping (
                todomate_id, google_event_id, todomate_fingerprint,
                google_fingerprint, last_synced_at, last_origin
            ) VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(todomate_id) DO UPDATE SET
                google_event_id=excluded.google_event_id,
                todomate_fingerprint=excluded.todomate_fingerprint,
                google_fingerprint=excluded.google_fingerprint,
                last_synced_at=excluded.last_synced_at,
                last_origin=excluded.last_origin
            """,
            (
                mapping.todomate_id,
                mapping.google_event_id,
                mapping.todomate_fingerprint,
                mapping.google_fingerprint,
                synced,
                mapping.last_origin,
            ),
        )
        self._conn.commit()

    def delete_by_todomate(self, todomate_id: str) -> None:
        self._conn.execute(
            "DELETE FROM mapping WHERE todomate_id = ?", (todomate_id,)
        )
        self._conn.commit()

    def delete_by_google(self, google_event_id: str) -> None:
        self._conn.execute(
            "DELETE FROM mapping WHERE google_event_id = ?", (google_event_id,)
        )
        self._conn.commit()

    def get_state(self, key: str) -> str | None:
        row = self._conn.execute(
            "SELECT value FROM sync_state WHERE key = ?", (key,)
        ).fetchone()
        return row["value"] if row else None

    def set_state(self, key: str, value: str) -> None:
        self._conn.execute(
            """
            INSERT INTO sync_state (key, value) VALUES (?, ?)
            ON CONFLICT(key) DO UPDATE SET value=excluded.value
            """,
            (key, value),
        )
        self._conn.commit()

    @staticmethod
    def _row_to_mapping(row: sqlite3.Row) -> SyncMapping:
        synced = None
        if row["last_synced_at"]:
            synced = datetime.fromisoformat(row["last_synced_at"])
        return SyncMapping(
            todomate_id=row["todomate_id"],
            google_event_id=row["google_event_id"],
            todomate_fingerprint=row["todomate_fingerprint"] or "",
            google_fingerprint=row["google_fingerprint"] or "",
            last_synced_at=synced,
            last_origin=row["last_origin"],
        )
