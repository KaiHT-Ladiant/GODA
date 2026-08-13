from __future__ import annotations

import argparse
import logging
import os
import sys
import time
from pathlib import Path

from google_to_domate.auth_google import build_calendar_service
from google_to_domate.auth_todomate import ensure_todomate_session
from google_to_domate.clients.google_cal import GoogleCalendarClient
from google_to_domate.clients.todomate import TodomateClient
from google_to_domate.config import ROOT, load_settings
from google_to_domate.sync.engine import SyncEngine
from google_to_domate.sync.mapping import MappingStore


def setup_logging(verbose: bool) -> None:
    level = logging.DEBUG if verbose else logging.INFO
    # Avoid mojibake for Korean titles on Windows consoles.
    if hasattr(sys.stdout, "reconfigure"):
        try:
            sys.stdout.reconfigure(encoding="utf-8")
            sys.stderr.reconfigure(encoding="utf-8")
        except Exception:  # noqa: BLE001
            pass
    logging.basicConfig(
        level=level,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="google-to-domate",
        description="Local bidirectional sync: Google Calendar ↔ Todomate",
    )
    parser.add_argument(
        "--config",
        type=Path,
        default=None,
        help="Path to config.toml / config.local.toml",
    )
    parser.add_argument(
        "--once",
        action="store_true",
        help="Run a single sync cycle and exit",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Log actions without writing changes",
    )
    parser.add_argument(
        "--interval",
        type=int,
        default=None,
        help="Poll interval seconds (overrides config)",
    )
    parser.add_argument("-v", "--verbose", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    setup_logging(args.verbose)
    log = logging.getLogger("google_to_domate")

    settings = load_settings(args.config)
    if args.dry_run:
        settings.dry_run = True
    if args.interval is not None:
        settings.interval_seconds = args.interval

    log.info("Project root: %s", ROOT)
    log.info("Dry-run=%s interval=%ss", settings.dry_run, settings.interval_seconds)

    # Startup auth — Google
    log.info("Checking Google Calendar login…")
    service = build_calendar_service(
        settings.google_credentials, settings.google_token
    )
    google = GoogleCalendarClient(service, settings.google_calendar_id)

    # Startup auth — Todomate
    # Prefer env TODOMATE_EMAIL / TODOMATE_PASSWORD for non-interactive runs.
    log.info("Checking Todomate login…")
    session = ensure_todomate_session(
        settings.todomate_api_key,
        settings.todomate_session,
        email=os.environ.get("TODOMATE_EMAIL") or None,
        password=os.environ.get("TODOMATE_PASSWORD") or None,
        interactive=True,
    )

    store = MappingStore(settings.sqlite_db)
    todomate = TodomateClient(
        session,
        api_key=settings.todomate_api_key,
        project_id=settings.todomate_project_id,
    )

    engine = SyncEngine(
        settings=settings,
        google=google,
        todomate=todomate,
        store=store,
    )

    try:
        if args.once:
            engine.run_once()
            return 0

        log.info("Starting poll loop (Ctrl+C to stop)")
        while True:
            try:
                engine.run_once()
            except Exception:  # noqa: BLE001
                log.exception("Sync cycle failed; will retry after interval")
            time.sleep(settings.interval_seconds)
    finally:
        todomate.close()
        store.close()


if __name__ == "__main__":
    sys.exit(main())
