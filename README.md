# GODA

**GODA** (고다) — short for **GOogletoDomAte**.

Local bidirectional sync between **Google Calendar** and **Todomate**.

![GODA](screenshots/001.png)

The primary client is a **Go desktop GUI** that opens a local control UI in your browser. No public webhook endpoint or reverse proxy is required for this local polling mode.

## Features

- Sync active Todomate goals only (`finishType` empty)
- Skip completed Todomate items by default (`skip_completed_todos`)
- Optional Google → Todomate import (`import_unmapped_google_events`)
- Local SQLite mapping store
- Dry-run on by default (`dry_run`; omitting the key stays on)
- Ownership marker so GODA only writes Google events it created
- Recurring series are not split into exceptions
- Deletions are tombstoned, counted, and aborted when they exceed a threshold

## Safety

GODA used to list recurring events as instances (`singleEvents=true`) and then PATCH or DELETE those instance ids. It also treated a completed todo, an inactive goal, and a Todomate 403/404 as “delete the Google event”, and it stamped `g2d_*` private properties onto events the user had created.

That combination dropped occurrences of a monthly series such as “월세 지급 💲” and rebuilt OSCP study todos that only matched the calendar by title and day. The current rules:

1. **Ownership.** `g2d_owned=1` is written only by `events.insert`. An event without it is never patched or deleted. `g2d_origin=sync` is not ownership; older builds wrote it while linking.
2. **Recurrence.** Instances (`recurringEventId`) are never written. The series is skipped, or — when import is on and no todo already has that title — stored as one Todomate Routine mapped to the master id.
3. **Completion and errors.** Marking a todo done does not delete a calendar event. HTTP 403, 404, and transport or 5xx errors are not “the todo was deleted”. The link stays.
4. **Deletions.** The only automatic retirement is a non-recurring event GODA created that Google reports as cancelled. The link is tombstoned (the row remains). `hard_delete` defaults to false, so the Todomate item is kept. More than `max_deletions_per_run` proposals (default 3) abort the cycle before any tombstone or remote delete. A failed delete does not drop the link. Google Calendar events are not deleted by sync.
5. **Dates.** Reads and writes use the Asia/Seoul calendar day. A Todomate timestamp at Seoul midnight no longer becomes the previous UTC date.
6. **Preconditions.** Updates and deletes send `If-Match` when an etag is known. A 412 leaves the mapping in place.

The legacy Python CLI raises immediately. It does not have these guards.

Details and the rollout checklist: [docs/sync-safety.md](docs/sync-safety.md).

## Requirements

- Go 1.22+ (to build)
- Google Cloud **Desktop OAuth** client JSON (`credentials.json`) — you create this manually
- Todomate account (email/password)

## Setup

```powershell
copy config.example.toml config.local.toml
```

1. Create a Google Cloud project
2. Enable **Google Calendar API**
3. Configure the OAuth consent screen (add yourself as a test user)
4. Create an OAuth client ID of type **Desktop app**
5. Download the JSON and save it as `credentials.json` in the project root
6. In `config.local.toml`, set `todomate.api_key` to the Todomate Firebase web API key (do not commit this file)

Tune `config.local.toml` as needed. Keep `dry_run = true` for the first runs.

## Auth files created by the client

| File | Created by login? | Notes |
| --- | --- | --- |
| `credentials.json` | **No** | Must be downloaded from Google Cloud Console and placed manually |
| `token.json` | **Yes** (Google login) | Created/updated after OAuth succeeds |
| `todomate_session.json` | **Yes** (Todomate login) | Created/updated after email/password login |
| `config.local.toml` | **No** | Copy from `config.example.toml` yourself |

## Run the GUI

```powershell
go build -buildvcs=false -o bin/goda.exe ./cmd/g2d
.\bin\goda.exe
```

The UI opens automatically and supports:

- Google login / Todomate login
- Dry-run toggle
- One-shot sync / polling start & stop
- Live logs

## Optional Python CLI

```powershell
py -3 -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -r requirements.txt
pip install -e .
google-to-domate --once -v
```

## Project layout

```
cmd/g2d/main.go
internal/
  config/ googleauth/ googlecal/ todomate/ store/ syncer/ ui/
assets/goda-camo-icon.png
screenshots/001.png
src/google_to_domate/          # Python CLI (legacy)
docs/todomate-api-notes.md
config.example.toml
```

## Notes

- Todomate has no official public API. This project uses an unofficial Firebase/Firestore integration that may break when Todomate changes.
- Webhooks are not used in the local GUI mode. Google push would need an always-on public HTTPS endpoint; Todomate still has no webhook support.
- Do not commit secrets: `credentials.json`, `token.json`, `todomate_session.json`, `config.local.toml`.
