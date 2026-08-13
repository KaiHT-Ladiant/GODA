from __future__ import annotations

import logging
from datetime import date, datetime, timedelta, timezone
from typing import Any

from google_to_domate.models import CalendarEvent

logger = logging.getLogger(__name__)

PRIVATE_PROP_TODO_ID = "g2d_todomate_id"
PRIVATE_PROP_ORIGIN = "g2d_origin"
ORIGIN_SYNC = "sync"


def _parse_event(item: dict[str, Any]) -> CalendarEvent:
    start_raw = item.get("start", {})
    end_raw = item.get("end", {})
    all_day = "date" in start_raw and "dateTime" not in start_raw

    if all_day:
        start: datetime | date = date.fromisoformat(start_raw["date"])
        end = date.fromisoformat(end_raw["date"]) if end_raw.get("date") else None
    else:
        start = datetime.fromisoformat(start_raw["dateTime"].replace("Z", "+00:00"))
        end = (
            datetime.fromisoformat(end_raw["dateTime"].replace("Z", "+00:00"))
            if end_raw.get("dateTime")
            else None
        )

    updated = None
    if item.get("updated"):
        updated = datetime.fromisoformat(item["updated"].replace("Z", "+00:00"))

    private = (item.get("extendedProperties") or {}).get("private") or {}
    return CalendarEvent(
        id=item["id"],
        summary=item.get("summary") or "",
        start=start,
        end=end,
        updated=updated,
        status=item.get("status") or "confirmed",
        description=item.get("description"),
        all_day=all_day,
        etag=item.get("etag"),
        todomate_id=private.get(PRIVATE_PROP_TODO_ID),
        origin_sync=private.get(PRIVATE_PROP_ORIGIN) == ORIGIN_SYNC,
        raw=item,
    )


def _event_body(
    *,
    summary: str,
    start: date | datetime,
    end: date | datetime | None,
    description: str | None,
    todomate_id: str | None,
    all_day: bool,
) -> dict[str, Any]:
    if all_day or (isinstance(start, date) and not isinstance(start, datetime)):
        start_date = (
            start
            if isinstance(start, date) and not isinstance(start, datetime)
            else start.date()  # type: ignore[union-attr]
        )
        if end is None:
            end_date = start_date + timedelta(days=1)
        elif isinstance(end, datetime):
            end_date = end.date()
        else:
            end_date = end
        if end_date <= start_date:
            end_date = start_date + timedelta(days=1)
        body_start = {"date": start_date.isoformat()}
        body_end = {"date": end_date.isoformat()}
    else:
        assert isinstance(start, datetime)
        end_dt = end if isinstance(end, datetime) else start + timedelta(hours=1)
        body_start = {"dateTime": start.isoformat(), "timeZone": "Asia/Seoul"}
        body_end = {"dateTime": end_dt.isoformat(), "timeZone": "Asia/Seoul"}

    private: dict[str, str] = {PRIVATE_PROP_ORIGIN: ORIGIN_SYNC}
    if todomate_id:
        private[PRIVATE_PROP_TODO_ID] = todomate_id

    body: dict[str, Any] = {
        "summary": summary,
        "start": body_start,
        "end": body_end,
        "extendedProperties": {"private": private},
    }
    if description:
        body["description"] = description
    return body


class GoogleCalendarClient:
    def __init__(self, service, calendar_id: str = "primary") -> None:
        self.service = service
        self.calendar_id = calendar_id

    def list_events(
        self,
        *,
        time_min: datetime,
        time_max: datetime,
        sync_token: str | None = None,
    ) -> tuple[list[CalendarEvent], str | None]:
        events: list[CalendarEvent] = []
        page_token = None
        new_sync_token = None

        while True:
            kwargs: dict[str, Any] = {
                "calendarId": self.calendar_id,
                "singleEvents": True,
                "showDeleted": True,
                "maxResults": 250,
                "pageToken": page_token,
            }
            if sync_token:
                kwargs["syncToken"] = sync_token
            else:
                kwargs["timeMin"] = time_min.astimezone(timezone.utc).isoformat()
                kwargs["timeMax"] = time_max.astimezone(timezone.utc).isoformat()
                kwargs["orderBy"] = "startTime"

            try:
                result = self.service.events().list(**kwargs).execute()
            except Exception as exc:  # noqa: BLE001
                if sync_token and "410" in str(exc):
                    logger.warning("Google syncToken expired; full resync")
                    return self.list_events(
                        time_min=time_min, time_max=time_max, sync_token=None
                    )
                raise

            for item in result.get("items", []):
                events.append(_parse_event(item))

            page_token = result.get("nextPageToken")
            new_sync_token = result.get("nextSyncToken", new_sync_token)
            if not page_token:
                break

        return events, new_sync_token

    def create_event(
        self,
        *,
        summary: str,
        start: date | datetime,
        end: date | datetime | None = None,
        description: str | None = None,
        todomate_id: str | None = None,
        all_day: bool = True,
    ) -> CalendarEvent:
        body = _event_body(
            summary=summary,
            start=start,
            end=end,
            description=description,
            todomate_id=todomate_id,
            all_day=all_day,
        )
        created = (
            self.service.events()
            .insert(calendarId=self.calendar_id, body=body)
            .execute()
        )
        return _parse_event(created)

    def update_event(
        self,
        event_id: str,
        *,
        summary: str,
        start: date | datetime,
        end: date | datetime | None = None,
        description: str | None = None,
        todomate_id: str | None = None,
        all_day: bool = True,
    ) -> CalendarEvent:
        body = _event_body(
            summary=summary,
            start=start,
            end=end,
            description=description,
            todomate_id=todomate_id,
            all_day=all_day,
        )
        updated = (
            self.service.events()
            .patch(calendarId=self.calendar_id, eventId=event_id, body=body)
            .execute()
        )
        return _parse_event(updated)

    def delete_event(self, event_id: str) -> None:
        self.service.events().delete(
            calendarId=self.calendar_id, eventId=event_id
        ).execute()
