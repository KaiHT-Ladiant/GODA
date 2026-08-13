from __future__ import annotations

import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

if sys.version_info >= (3, 11):
    import tomllib
else:
    import tomli as tomllib


ROOT = Path(__file__).resolve().parents[2]


@dataclass(slots=True)
class Settings:
    interval_seconds: int = 60
    lookback_days: int = 14
    lookahead_days: int = 60
    dry_run: bool = False
    google_calendar_id: str = "primary"
    prefer_google_on_tie: bool = True
    import_unmapped_google_events: bool = True
    # Do not sync completed (isDone) Todomate items to Google
    skip_completed_todos: bool = True
    data_dir: Path = field(default_factory=lambda: ROOT / "data")
    google_credentials: Path = field(default_factory=lambda: ROOT / "credentials.json")
    google_token: Path = field(default_factory=lambda: ROOT / "token.json")
    todomate_session: Path = field(
        default_factory=lambda: ROOT / "todomate_session.json"
    )
    sqlite_db: Path = field(default_factory=lambda: ROOT / "data" / "sync.sqlite3")
    todomate_api_key: str = ""
    todomate_project_id: str = "mate-914f3"
    include_goal_ids: list[str] = field(default_factory=list)
    exclude_goal_ids: list[str] = field(default_factory=list)
    default_goal_id: str = ""

    def ensure_dirs(self) -> None:
        self.data_dir.mkdir(parents=True, exist_ok=True)
        self.sqlite_db.parent.mkdir(parents=True, exist_ok=True)


def _resolve(path: str | Path, base: Path = ROOT) -> Path:
    p = Path(path)
    return p if p.is_absolute() else (base / p).resolve()


def load_settings(config_path: Path | None = None) -> Settings:
    candidates = []
    if config_path:
        candidates.append(config_path)
    candidates.extend(
        [
            ROOT / "config.local.toml",
            ROOT / "config.toml",
            ROOT / "config.example.toml",
        ]
    )

    raw: dict[str, Any] = {}
    for path in candidates:
        if path.is_file():
            with path.open("rb") as fh:
                raw = tomllib.load(fh)
            break

    sync = raw.get("sync", {})
    paths = raw.get("paths", {})
    todo = raw.get("todomate", {})

    settings = Settings(
        interval_seconds=int(sync.get("interval_seconds", 60)),
        lookback_days=int(sync.get("lookback_days", 14)),
        lookahead_days=int(sync.get("lookahead_days", 60)),
        dry_run=bool(sync.get("dry_run", False)),
        google_calendar_id=str(sync.get("google_calendar_id", "primary")),
        prefer_google_on_tie=bool(sync.get("prefer_google_on_tie", True)),
        import_unmapped_google_events=bool(
            sync.get("import_unmapped_google_events", True)
        ),
        skip_completed_todos=bool(sync.get("skip_completed_todos", True)),
        data_dir=_resolve(paths.get("data_dir", "data")),
        google_credentials=_resolve(paths.get("google_credentials", "credentials.json")),
        google_token=_resolve(paths.get("google_token", "token.json")),
        todomate_session=_resolve(
            paths.get("todomate_session", "todomate_session.json")
        ),
        sqlite_db=_resolve(paths.get("sqlite_db", "data/sync.sqlite3")),
        todomate_api_key=str(todo.get("api_key", "")),
        todomate_project_id=str(todo.get("project_id", "mate-914f3")),
        include_goal_ids=list(todo.get("include_goal_ids") or []),
        exclude_goal_ids=list(todo.get("exclude_goal_ids") or []),
        default_goal_id=str(todo.get("default_goal_id") or ""),
    )
    settings.ensure_dirs()
    return settings
