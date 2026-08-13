from __future__ import annotations

import logging
from collections import Counter
from datetime import date, datetime, timedelta, timezone

from google_to_domate.clients.google_cal import GoogleCalendarClient
from google_to_domate.clients.todomate import TodomateClient
from google_to_domate.config import Settings
from google_to_domate.models import CalendarEvent, SyncMapping, TodoItem
from google_to_domate.sync.mapping import MappingStore

logger = logging.getLogger(__name__)


def _as_utc(dt: datetime | None) -> datetime:
    if dt is None:
        return datetime.min.replace(tzinfo=timezone.utc)
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def _event_date(event: CalendarEvent) -> date:
    if isinstance(event.start, datetime):
        return event.start.date()
    return event.start


class SyncEngine:
    def __init__(
        self,
        *,
        settings: Settings,
        google: GoogleCalendarClient,
        todomate: TodomateClient,
        store: MappingStore,
    ) -> None:
        self.settings = settings
        self.google = google
        self.todomate = todomate
        self.store = store
        self._stats: Counter[str] = Counter()

    def run_once(self) -> None:
        self._stats = Counter()
        today = date.today()
        start = today - timedelta(days=self.settings.lookback_days)
        end = today + timedelta(days=self.settings.lookahead_days)
        time_min = datetime.combine(start, datetime.min.time(), tzinfo=timezone.utc)
        time_max = datetime.combine(end, datetime.max.time(), tzinfo=timezone.utc)

        active_goals = self.todomate.list_active_goals(
            include_goal_ids=self.settings.include_goal_ids,
            exclude_goal_ids=self.settings.exclude_goal_ids,
        )
        active_goal_ids = {g.id for g in active_goals}
        for goal in active_goals:
            logger.info("Active goal: %s (%s)", goal.title, goal.id)
        logger.info("Active Todomate goals: %d", len(active_goal_ids))

        todos = self.todomate.list_todo_items(start_date=start, end_date=end)
        active_todos = [t for t in todos if t.goal_id in active_goal_ids and t.date]
        if self.settings.skip_completed_todos:
            before = len(active_todos)
            active_todos = [t for t in active_todos if not t.is_done]
            skipped = before - len(active_todos)
            if skipped:
                logger.info("Skipped completed Todomate items: %d", skipped)

        logger.info(
            "Sync window %s ~ %s | candidates=%d",
            start,
            end,
            len(active_todos),
        )

        events, new_token = self.google.list_events(
            time_min=time_min, time_max=time_max, sync_token=None
        )
        if new_token:
            self.store.set_state("google_sync_token", new_token)

        live_events = [e for e in events if e.status != "cancelled"]
        cancelled = [e for e in events if e.status == "cancelled"]

        todo_by_id = {t.id: t for t in active_todos}
        event_by_id = {e.id: e for e in live_events}

        # 1) Prune only when Todomate item is deleted or goal became inactive
        self._prune_inactive(active_goal_ids, todo_by_id)

        # 2) Todomate → Google
        for todo in active_todos:
            self._sync_todo_to_google(todo, event_by_id)

        # 3) Google → Todomate
        default_goal = self.settings.default_goal_id
        if not default_goal and active_goals:
            default_goal = sorted(active_goals, key=lambda g: g.priority)[0].id

        for event in live_events:
            self._sync_google_to_todo(event, todo_by_id, default_goal)

        # 4) Google cancellations → delete Todomate + mapping
        for event in cancelled:
            mapping = self.store.get_by_google(event.id)
            if not mapping:
                continue
            logger.info(
                "Google cancelled %s → delete Todomate %s",
                event.id,
                mapping.todomate_id,
            )
            self._stats["google_cancelled_delete_todo"] += 1
            if not self.settings.dry_run:
                try:
                    self.todomate.delete_todo_item(mapping.todomate_id)
                except Exception as exc:  # noqa: BLE001
                    logger.warning("Failed deleting Todomate item: %s", exc)
                self.store.delete_by_google(event.id)

        logger.info(
            "Sync summary (dry_run=%s): create_google=%d update_google=%d "
            "prune_google=%d create_todo=%d update_todo=%d delete_todo=%d",
            self.settings.dry_run,
            self._stats["create_google"],
            self._stats["update_google"],
            self._stats["prune_google"],
            self._stats["create_todo"],
            self._stats["update_todo"],
            self._stats["google_cancelled_delete_todo"],
        )
        logger.info("Sync cycle complete (dry_run=%s)", self.settings.dry_run)

    def _prune_inactive(
        self,
        active_goal_ids: set[str],
        active_todo_by_id: dict[str, TodoItem],
    ) -> None:
        """Remove Google events only when source todo is gone or goal inactive.

        Todos outside the lookback/lookahead window are kept (not pruned).
        """
        for mapping in self.store.all_mappings():
            if mapping.todomate_id in active_todo_by_id:
                continue

            try:
                remote = self.todomate.get_todo_item(mapping.todomate_id)
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    "Skip prune for %s (lookup failed: %s)",
                    mapping.todomate_id,
                    exc,
                )
                continue

            if remote is None:
                reason = "deleted"
            elif remote.goal_id not in active_goal_ids:
                reason = "inactive_goal"
            elif self.settings.skip_completed_todos and remote.is_done:
                reason = "completed"
            else:
                # Still a valid active todo (likely outside sync window)
                logger.debug(
                    "Keep mapping for out-of-window todo %s", mapping.todomate_id
                )
                continue

            logger.info(
                "Prune Todomate %s (%s) → delete Google %s",
                mapping.todomate_id,
                reason,
                mapping.google_event_id,
            )
            self._stats["prune_google"] += 1
            if not self.settings.dry_run:
                try:
                    self.google.delete_event(mapping.google_event_id)
                except Exception as exc:  # noqa: BLE001
                    logger.warning("Failed deleting Google event: %s", exc)
                self.store.delete_by_todomate(mapping.todomate_id)

    def _sync_todo_to_google(
        self, todo: TodoItem, event_by_id: dict[str, CalendarEvent]
    ) -> None:
        assert todo.date is not None
        mapping = self.store.get_by_todomate(todo.id)
        fp = todo.fingerprint()

        if mapping and mapping.todomate_fingerprint == fp:
            return

        if mapping is None:
            logger.info(
                "Create Google event for Todomate %s (%s)", todo.id, todo.content
            )
            self._stats["create_google"] += 1
            if self.settings.dry_run:
                return
            created = self.google.create_event(
                summary=todo.content,
                start=todo.date,
                description=todo.memo,
                todomate_id=todo.id,
                all_day=True,
            )
            self.store.upsert(
                SyncMapping(
                    todomate_id=todo.id,
                    google_event_id=created.id,
                    todomate_fingerprint=fp,
                    google_fingerprint=created.fingerprint(),
                    last_synced_at=datetime.now(tz=timezone.utc),
                    last_origin="todomate",
                )
            )
            return

        google_event = event_by_id.get(mapping.google_event_id)
        if google_event is not None:
            google_changed = google_event.fingerprint() != mapping.google_fingerprint
            todo_changed = fp != mapping.todomate_fingerprint
            if google_changed and todo_changed:
                google_newer = _as_utc(google_event.updated) >= _as_utc(todo.updated_at)
                if google_newer or (
                    _as_utc(google_event.updated) == _as_utc(todo.updated_at)
                    and self.settings.prefer_google_on_tie
                ):
                    logger.debug(
                        "Skip Todomate→Google for %s (Google newer)", todo.id
                    )
                    return
            elif google_changed and not todo_changed:
                return

        logger.info(
            "Update Google event %s from Todomate %s", mapping.google_event_id, todo.id
        )
        self._stats["update_google"] += 1
        if self.settings.dry_run:
            return
        updated = self.google.update_event(
            mapping.google_event_id,
            summary=todo.content,
            start=todo.date,
            description=todo.memo,
            todomate_id=todo.id,
            all_day=True,
        )
        self.store.upsert(
            SyncMapping(
                todomate_id=todo.id,
                google_event_id=updated.id,
                todomate_fingerprint=fp,
                google_fingerprint=updated.fingerprint(),
                last_synced_at=datetime.now(tz=timezone.utc),
                last_origin="todomate",
            )
        )

    def _sync_google_to_todo(
        self,
        event: CalendarEvent,
        todo_by_id: dict[str, TodoItem],
        default_goal: str,
    ) -> None:
        if event.status == "cancelled":
            return

        mapping = self.store.get_by_google(event.id)
        if mapping is None and event.todomate_id:
            mapping = self.store.get_by_todomate(event.todomate_id)

        fp = event.fingerprint()
        on_date = _event_date(event)

        if mapping:
            if mapping.google_fingerprint == fp:
                return
            todo = todo_by_id.get(mapping.todomate_id)
            if todo:
                google_newer = _as_utc(event.updated) >= _as_utc(todo.updated_at)
                if not google_newer and not self.settings.prefer_google_on_tie:
                    return
                if todo.fingerprint() == (
                    f"{event.summary}|{on_date.isoformat()}|{todo.goal_id}|"
                    f"{'1' if todo.is_done else '0'}|{event.description or ''}|"
                    f"{todo.remind_at or ''}"
                ):
                    self.store.upsert(
                        SyncMapping(
                            todomate_id=todo.id,
                            google_event_id=event.id,
                            todomate_fingerprint=todo.fingerprint(),
                            google_fingerprint=fp,
                            last_synced_at=datetime.now(tz=timezone.utc),
                            last_origin="google",
                        )
                    )
                    return

                logger.info(
                    "Update Todomate %s from Google %s", mapping.todomate_id, event.id
                )
                self._stats["update_todo"] += 1
                if self.settings.dry_run:
                    return
                updated = self.todomate.update_todo_item(
                    mapping.todomate_id,
                    content=event.summary,
                    on_date=on_date,
                    memo=event.description,
                )
                self.store.upsert(
                    SyncMapping(
                        todomate_id=updated.id,
                        google_event_id=event.id,
                        todomate_fingerprint=updated.fingerprint(),
                        google_fingerprint=fp,
                        last_synced_at=datetime.now(tz=timezone.utc),
                        last_origin="google",
                    )
                )
                return

            if not default_goal:
                logger.warning(
                    "No default_goal_id; skip creating Todomate for %s", event.id
                )
                return
            logger.info("Recreate Todomate for Google %s", event.id)
            self._stats["create_todo"] += 1
            if self.settings.dry_run:
                return
            created = self.todomate.create_todo_item(
                content=event.summary or "(untitled)",
                goal_id=default_goal,
                on_date=on_date,
                memo=event.description,
            )
            self.google.update_event(
                event.id,
                summary=event.summary,
                start=event.start,
                end=event.end,
                description=event.description,
                todomate_id=created.id,
                all_day=event.all_day,
            )
            self.store.upsert(
                SyncMapping(
                    todomate_id=created.id,
                    google_event_id=event.id,
                    todomate_fingerprint=created.fingerprint(),
                    google_fingerprint=fp,
                    last_synced_at=datetime.now(tz=timezone.utc),
                    last_origin="google",
                )
            )
            return

        if event.origin_sync and event.todomate_id:
            if event.todomate_id in todo_by_id:
                todo = todo_by_id[event.todomate_id]
                self.store.upsert(
                    SyncMapping(
                        todomate_id=todo.id,
                        google_event_id=event.id,
                        todomate_fingerprint=todo.fingerprint(),
                        google_fingerprint=fp,
                        last_synced_at=datetime.now(tz=timezone.utc),
                        last_origin="recover",
                    )
                )
            return

        if not self.settings.import_unmapped_google_events:
            return

        if not default_goal:
            logger.warning(
                "Skip Google→Todomate create for %s (set todomate.default_goal_id)",
                event.id,
            )
            return

        logger.info("Create Todomate item from Google %s (%s)", event.id, event.summary)
        self._stats["create_todo"] += 1
        if self.settings.dry_run:
            return
        created = self.todomate.create_todo_item(
            content=event.summary or "(untitled)",
            goal_id=default_goal,
            on_date=on_date,
            memo=event.description,
        )
        linked = self.google.update_event(
            event.id,
            summary=event.summary,
            start=event.start,
            end=event.end,
            description=event.description,
            todomate_id=created.id,
            all_day=event.all_day,
        )
        self.store.upsert(
            SyncMapping(
                todomate_id=created.id,
                google_event_id=linked.id,
                todomate_fingerprint=created.fingerprint(),
                google_fingerprint=linked.fingerprint(),
                last_synced_at=datetime.now(tz=timezone.utc),
                last_origin="google",
            )
        )
