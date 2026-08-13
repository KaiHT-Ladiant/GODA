from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date, datetime
from typing import Any


@dataclass(slots=True)
class Goal:
    id: str
    user_id: str
    title: str
    create_time: int | None = None
    priority: int = 0
    color: str | None = None
    is_public: bool = False
    finish_type: Any | None = None
    crew_id: str | None = None

    @property
    def is_active(self) -> bool:
        """Goals with a non-null finishType are finished/inactive in Todomate."""
        return self.finish_type is None


@dataclass(slots=True)
class TodoItem:
    id: str
    writer_id: str
    goal_id: str
    content: str
    date: date | None
    create_time: int = 0
    is_done: bool = False
    remind_at: int | None = None
    done_time: int | None = None
    memo: str | None = None
    routine_id: str | None = None
    updated_at: datetime | None = None

    def fingerprint(self) -> str:
        return "|".join(
            [
                self.content or "",
                self.date.isoformat() if self.date else "",
                self.goal_id or "",
                "1" if self.is_done else "0",
                self.memo or "",
                str(self.remind_at or ""),
            ]
        )


@dataclass(slots=True)
class CalendarEvent:
    id: str
    summary: str
    start: datetime | date
    end: datetime | date | None
    updated: datetime | None = None
    status: str = "confirmed"
    description: str | None = None
    all_day: bool = False
    etag: str | None = None
    todomate_id: str | None = None
    origin_sync: bool = False
    raw: dict[str, Any] = field(default_factory=dict)

    def fingerprint(self) -> str:
        start = (
            self.start.isoformat()
            if isinstance(self.start, (datetime, date))
            else str(self.start)
        )
        return "|".join(
            [
                self.summary or "",
                start,
                "1" if self.all_day else "0",
                self.description or "",
                self.status or "",
            ]
        )


@dataclass(slots=True)
class SyncMapping:
    todomate_id: str
    google_event_id: str
    todomate_fingerprint: str = ""
    google_fingerprint: str = ""
    last_synced_at: datetime | None = None
    last_origin: str | None = None
