from __future__ import annotations

import logging
from pathlib import Path

from google.auth.transport.requests import Request
from google.oauth2.credentials import Credentials
from google_auth_oauthlib.flow import InstalledAppFlow
from googleapiclient.discovery import build

logger = logging.getLogger(__name__)

SCOPES = ["https://www.googleapis.com/auth/calendar"]


def get_google_credentials(credentials_path: Path, token_path: Path) -> Credentials:
    creds: Credentials | None = None
    if token_path.is_file():
        creds = Credentials.from_authorized_user_file(str(token_path), SCOPES)

    if creds and creds.valid:
        return creds

    if creds and creds.expired and creds.refresh_token:
        logger.info("Refreshing Google OAuth token")
        creds.refresh(Request())
    else:
        if not credentials_path.is_file():
            raise FileNotFoundError(
                f"Google OAuth client secrets not found: {credentials_path}\n"
                "Download a Desktop OAuth client JSON from Google Cloud Console "
                "and save it as credentials.json"
            )
        logger.info("Starting Google OAuth browser login")
        flow = InstalledAppFlow.from_client_secrets_file(str(credentials_path), SCOPES)
        creds = flow.run_local_server(port=0)

    token_path.parent.mkdir(parents=True, exist_ok=True)
    token_path.write_text(creds.to_json(), encoding="utf-8")
    logger.info("Saved Google token to %s", token_path)
    return creds


def build_calendar_service(credentials_path: Path, token_path: Path):
    creds = get_google_credentials(credentials_path, token_path)
    return build("calendar", "v3", credentials=creds, cache_discovery=False)
