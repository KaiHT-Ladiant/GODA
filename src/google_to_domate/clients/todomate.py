from __future__ import annotations

import logging
import uuid
from datetime import date, datetime, timezone
from typing import Any

import httpx

from google_to_domate.auth_todomate import TodomateSession
from google_to_domate.models import Goal, TodoItem

logger = logging.getLogger(__name__)


def _firestore_base(project_id: str) -> str:
    return (
        f"https://firestore.googleapis.com/v1/projects/{project_id}"
        "/databases/(default)/documents"
    )


def _from_value(value: dict[str, Any]) -> Any:
    if "stringValue" in value:
        return value["stringValue"]
    if "integerValue" in value:
        return int(value["integerValue"])
    if "doubleValue" in value:
        return float(value["doubleValue"])
    if "booleanValue" in value:
        return value["booleanValue"]
    if "nullValue" in value:
        return None
    if "timestampValue" in value:
        return value["timestampValue"]
    if "arrayValue" in value:
        return [_from_value(v) for v in value["arrayValue"].get("values", [])]
    if "mapValue" in value:
        fields = value["mapValue"].get("fields", {})
        return {k: _from_value(v) for k, v in fields.items()}
    return value


def _to_value(value: Any) -> dict[str, Any]:
    if value is None:
        return {"nullValue": None}
    if isinstance(value, bool):
        return {"booleanValue": value}
    if isinstance(value, int):
        return {"integerValue": str(value)}
    if isinstance(value, float):
        return {"doubleValue": value}
    if isinstance(value, str):
        return {"stringValue": value}
    if isinstance(value, date) and not isinstance(value, datetime):
        # Todomate stores date as epoch ms at local midnight-ish; use noon UTC ms
        dt = datetime(value.year, value.month, value.day, tzinfo=timezone.utc)
        return {"integerValue": str(int(dt.timestamp() * 1000))}
    if isinstance(value, datetime):
        return {"integerValue": str(int(value.timestamp() * 1000))}
    if isinstance(value, list):
        return {"arrayValue": {"values": [_to_value(v) for v in value]}}
    if isinstance(value, dict):
        return {
            "mapValue": {
                "fields": {k: _to_value(v) for k, v in value.items()}
            }
        }
    return {"stringValue": str(value)}


def _doc_fields(document: dict[str, Any]) -> dict[str, Any]:
    return {
        key: _from_value(val)
        for key, val in (document.get("fields") or {}).items()
    }


def _parse_date(value: Any) -> date | None:
    if value is None:
        return None
    if isinstance(value, date) and not isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        # millis
        ts = value / 1000 if value > 10_000_000_000 else value
        return datetime.fromtimestamp(ts, tz=timezone.utc).date()
    if isinstance(value, str):
        if value.isdigit():
            return _parse_date(int(value))
        try:
            return date.fromisoformat(value[:10])
        except ValueError:
            return None
    return None


def _parse_goal(doc: dict[str, Any]) -> Goal:
    fields = _doc_fields(doc)
    doc_id = doc.get("name", "").rsplit("/", 1)[-1]
    return Goal(
        id=str(fields.get("id") or doc_id),
        user_id=str(fields.get("userID") or ""),
        title=str(fields.get("title") or ""),
        create_time=int(fields["createTime"]) if fields.get("createTime") is not None else None,
        priority=int(fields.get("priority") or 0),
        color=fields.get("color"),
        is_public=bool(fields.get("isPublic") or False),
        finish_type=fields.get("finishType"),
        crew_id=fields.get("crewId"),
    )


def _parse_todo(doc: dict[str, Any]) -> TodoItem:
    fields = _doc_fields(doc)
    doc_id = doc.get("name", "").rsplit("/", 1)[-1]
    create_time = int(fields.get("createTime") or 0)
    updated = None
    if create_time:
        updated = datetime.fromtimestamp(create_time / 1000, tz=timezone.utc)
    # Prefer doneTime / remindAt as freshness hints when present
    for key in ("doneTime", "remindAt"):
        val = fields.get(key)
        if isinstance(val, (int, float)) and val:
            candidate = datetime.fromtimestamp(
                (val / 1000 if val > 10_000_000_000 else val),
                tz=timezone.utc,
            )
            if updated is None or candidate > updated:
                updated = candidate

    return TodoItem(
        id=str(fields.get("id") or doc_id),
        writer_id=str(fields.get("writerID") or ""),
        goal_id=str(fields.get("goalID") or ""),
        content=str(fields.get("content") or ""),
        date=_parse_date(fields.get("date")),
        create_time=create_time,
        is_done=bool(fields.get("isDone") or False),
        remind_at=int(fields["remindAt"]) if fields.get("remindAt") is not None else None,
        done_time=int(fields["doneTime"]) if fields.get("doneTime") is not None else None,
        memo=fields.get("memo"),
        routine_id=fields.get("routineID"),
        updated_at=updated,
    )


class TodomateClient:
    """Unofficial Todomate client via Firebase Auth + Firestore REST."""

    def __init__(
        self,
        session: TodomateSession,
        *,
        api_key: str,
        project_id: str,
    ) -> None:
        self.session = session
        self.api_key = api_key
        self.project_id = project_id
        self._client = httpx.Client(timeout=45.0)

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> TodomateClient:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    @property
    def uid(self) -> str:
        return self.session.local_id

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.session.id_token}"}

    def _run_query(self, structured_query: dict[str, Any]) -> list[dict[str, Any]]:
        url = f"{_firestore_base(self.project_id)}:runQuery"
        resp = self._client.post(
            url,
            headers=self._headers(),
            json={"structuredQuery": structured_query},
        )
        resp.raise_for_status()
        rows = resp.json()
        docs = []
        for row in rows:
            if "document" in row:
                docs.append(row["document"])
        return docs

    def list_goals(self) -> list[Goal]:
        docs = self._run_query(
            {
                "from": [{"collectionId": "Goal"}],
                "where": {
                    "fieldFilter": {
                        "field": {"fieldPath": "userID"},
                        "op": "EQUAL",
                        "value": {"stringValue": self.uid},
                    }
                },
            }
        )
        goals = [_parse_goal(d) for d in docs]
        logger.info("Fetched %d Todomate goals", len(goals))
        return goals

    def list_active_goals(
        self,
        *,
        include_goal_ids: list[str] | None = None,
        exclude_goal_ids: list[str] | None = None,
    ) -> list[Goal]:
        include = set(include_goal_ids or [])
        exclude = set(exclude_goal_ids or [])
        goals = []
        for goal in self.list_goals():
            if not goal.is_active:
                continue
            if include and goal.id not in include:
                continue
            if goal.id in exclude:
                continue
            goals.append(goal)
        return goals

    def get_todo_item(self, todo_id: str) -> TodoItem | None:
        url = f"{_firestore_base(self.project_id)}/TodoItem/{todo_id}"
        resp = self._client.get(url, headers=self._headers())
        if resp.status_code == 404:
            return None
        resp.raise_for_status()
        return _parse_todo(resp.json())

    def list_todo_items(
        self,
        *,
        start_date: date | None = None,
        end_date: date | None = None,
    ) -> list[TodoItem]:
        # Firestore may require composite indexes for multi-filter; filter date in Python.
        docs = self._run_query(
            {
                "from": [{"collectionId": "TodoItem"}],
                "where": {
                    "fieldFilter": {
                        "field": {"fieldPath": "writerID"},
                        "op": "EQUAL",
                        "value": {"stringValue": self.uid},
                    }
                },
            }
        )
        items = [_parse_todo(d) for d in docs]
        if start_date or end_date:
            filtered = []
            for item in items:
                if item.date is None:
                    continue
                if start_date and item.date < start_date:
                    continue
                if end_date and item.date > end_date:
                    continue
                filtered.append(item)
            items = filtered
        logger.info("Fetched %d Todomate todo items (windowed)", len(items))
        return items

    def create_todo_item(
        self,
        *,
        content: str,
        goal_id: str,
        on_date: date,
        memo: str | None = None,
        is_done: bool = False,
    ) -> TodoItem:
        doc_id = uuid.uuid4().hex[:20]
        now_ms = int(datetime.now(tz=timezone.utc).timestamp() * 1000)
        date_ms = int(
            datetime(on_date.year, on_date.month, on_date.day, tzinfo=timezone.utc)
            .timestamp()
            * 1000
        )
        fields = {
            "id": doc_id,
            "writerID": self.uid,
            "goalID": goal_id,
            "content": content,
            "createTime": now_ms,
            "date": date_ms,
            "isDone": is_done,
            "memo": memo,
            "routineID": None,
            "remindAt": None,
            "doneTime": None,
            "hasPhoto": False,
            "hasTimer": False,
            "isMemoPublic": False,
        }
        url = f"{_firestore_base(self.project_id)}/TodoItem?documentId={doc_id}"
        resp = self._client.post(
            url,
            headers=self._headers(),
            json={"fields": {k: _to_value(v) for k, v in fields.items()}},
        )
        resp.raise_for_status()
        return _parse_todo(resp.json())

    def update_todo_item(
        self,
        todo_id: str,
        *,
        content: str | None = None,
        on_date: date | None = None,
        memo: str | None = None,
        is_done: bool | None = None,
        goal_id: str | None = None,
    ) -> TodoItem:
        updates: dict[str, Any] = {}
        if content is not None:
            updates["content"] = content
        if on_date is not None:
            updates["date"] = int(
                datetime(
                    on_date.year, on_date.month, on_date.day, tzinfo=timezone.utc
                ).timestamp()
                * 1000
            )
        if memo is not None:
            updates["memo"] = memo
        if is_done is not None:
            updates["isDone"] = is_done
        if goal_id is not None:
            updates["goalID"] = goal_id

        field_paths = list(updates.keys())
        params = [("updateMask.fieldPaths", p) for p in field_paths]
        url = f"{_firestore_base(self.project_id)}/TodoItem/{todo_id}"
        resp = self._client.patch(
            url,
            headers=self._headers(),
            params=params,
            json={"fields": {k: _to_value(v) for k, v in updates.items()}},
        )
        resp.raise_for_status()
        return _parse_todo(resp.json())

    def delete_todo_item(self, todo_id: str) -> None:
        url = f"{_firestore_base(self.project_id)}/TodoItem/{todo_id}"
        resp = self._client.delete(url, headers=self._headers())
        resp.raise_for_status()
