from __future__ import annotations

import getpass
import json
import logging
import time
from dataclasses import asdict, dataclass
from pathlib import Path

import httpx

logger = logging.getLogger(__name__)

IDENTITY_URL = "https://identitytoolkit.googleapis.com/v1/accounts:signInWithPassword"
REFRESH_URL = "https://securetoken.googleapis.com/v1/token"


@dataclass(slots=True)
class TodomateSession:
    email: str
    local_id: str
    id_token: str
    refresh_token: str
    expires_at: float

    @property
    def expired(self) -> bool:
        return time.time() >= self.expires_at - 60


def _save(path: Path, session: TodomateSession) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(asdict(session), indent=2), encoding="utf-8")


def _load(path: Path) -> TodomateSession | None:
    if not path.is_file():
        return None
    data = json.loads(path.read_text(encoding="utf-8"))
    return TodomateSession(**data)


def _sign_in(api_key: str, email: str, password: str) -> TodomateSession:
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            IDENTITY_URL,
            params={"key": api_key},
            json={
                "email": email,
                "password": password,
                "returnSecureToken": True,
            },
        )
        resp.raise_for_status()
        data = resp.json()
    expires_in = int(data.get("expiresIn", "3600"))
    return TodomateSession(
        email=email,
        local_id=data["localId"],
        id_token=data["idToken"],
        refresh_token=data["refreshToken"],
        expires_at=time.time() + expires_in,
    )


def _refresh(api_key: str, session: TodomateSession) -> TodomateSession:
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            REFRESH_URL,
            params={"key": api_key},
            data={
                "grant_type": "refresh_token",
                "refresh_token": session.refresh_token,
            },
        )
        resp.raise_for_status()
        data = resp.json()
    expires_in = int(data.get("expires_in", "3600"))
    return TodomateSession(
        email=session.email,
        local_id=data.get("user_id", session.local_id),
        id_token=data["id_token"],
        refresh_token=data.get("refresh_token", session.refresh_token),
        expires_at=time.time() + expires_in,
    )


def ensure_todomate_session(
    api_key: str,
    session_path: Path,
    *,
    email: str | None = None,
    password: str | None = None,
    interactive: bool = True,
) -> TodomateSession:
    session = _load(session_path)
    if session and not session.expired:
        logger.info("Using saved Todomate session for %s", session.email)
        return session

    if session and session.expired:
        try:
            logger.info("Refreshing Todomate session")
            session = _refresh(api_key, session)
            _save(session_path, session)
            return session
        except Exception as exc:  # noqa: BLE001
            logger.warning("Todomate refresh failed (%s); need login again", exc)

    if not interactive and (not email or not password):
        raise RuntimeError("Todomate session missing and interactive login disabled")

    email = email or input("Todomate email: ").strip()
    password = password or getpass.getpass("Todomate password: ")
    logger.info("Signing in to Todomate (Firebase Auth)")
    session = _sign_in(api_key, email, password)
    _save(session_path, session)
    logger.info("Saved Todomate session to %s", session_path)
    return session
