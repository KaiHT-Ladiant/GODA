# Todomate unofficial API notes

Observed from the Todomate Flutter web client (`https://www.todomate.net/main.dart.js`, project `mate-914f3`).

This is **not** an official API. Endpoints and fields may change without notice. Personal use only.

## Stack

- Auth: Firebase Authentication (Identity Toolkit)
- Data: Cloud Firestore
- Project ID: `mate-914f3`
- Auth domain: `mate-914f3.firebaseapp.com`
- Web API key (embedded in client): see `config.example.toml`

## Auth

### Email / password sign-in

`POST https://identitytoolkit.googleapis.com/v1/accounts:signInWithPassword?key={API_KEY}`

```json
{
  "email": "user@example.com",
  "password": "...",
  "returnSecureToken": true
}
```

Response includes `idToken`, `refreshToken`, `localId` (Firebase UID), `expiresIn`.

### Refresh

`POST https://securetoken.googleapis.com/v1/token?key={API_KEY}`

`grant_type=refresh_token&refresh_token=...`

## Firestore collections

### `Goal` (category / list)

Filter: `userID == <uid>`

Fields:

| Field | Type | Notes |
| --- | --- | --- |
| id | string | document id |
| userID | string | owner uid |
| title | string | list name |
| createTime | int (ms) | |
| priority | int | sort order |
| color | string/null | |
| isPublic | bool | |
| finishType | any/null | **null = active**; non-null = finished/inactive |
| viewerIDs | array | |
| isViewerIDsFollowers | bool | |
| crewId | string/null | |

GOogletoDomAte only syncs todos whose goal has `finishType == null` (active).

Closed-in-feed state (`userFeedClosedGoalIDs`) is a **local preference**, not reliably available via Firestore. Use `include_goal_ids` / `exclude_goal_ids` in config if needed.

### `TodoItem`

Filter: `writerID == <uid>` (optionally also by `date`)

Fields:

| Field | Type | Notes |
| --- | --- | --- |
| id | string | |
| writerID | string | owner uid |
| goalID | string | parent Goal |
| content | string | title text |
| createTime | int (ms) | |
| date | int (ms) | day of the todo |
| isDone | bool | |
| remindAt | int/null | |
| doneTime | int/null | |
| memo | string/null | |
| routineID | string/null | links to `Routine` when generated from a routine |
| photoURL / hasPhoto / timer / … | | ignored by sync |

### `Routine`

Filter: `userID == <uid>`

Fields:

| Field | Type | Notes |
| --- | --- | --- |
| id | string | document id |
| userID | string | owner uid |
| title | string | routine title (also used as todo content) |
| goalID | string | parent Goal |
| repeatType | int | `0=daily`, `1=weekly`, `2=monthly`, `3=biweekly`, `4=yearly` |
| weekdays | array/null | used for weekly |
| daysOfMonth | array/null | used for monthly |
| daysOfYear | map/null | used for yearly |
| startDate | int (ms)/null | inclusive range start |
| endDate | int (ms)/null | inclusive range end |
| createTime | int (ms) | |
| time | any/null | optional time-of-day |
| isManual | bool | client flag |

GODA Google→Todomate multi-day (all-day span > 1 day), only when nothing is mapped yet:

1. Create `Routine` with `repeatType=0 (daily)`, `startDate`, `endDate` (Google all-day `end` is exclusive → Todomate end = end−1 day, calendar days in Asia/Seoul)
2. Create one `TodoItem` per day in that inclusive range with the same `routineID`
3. Map the **start-day** TodoItem ↔ Google event (siblings are not pushed back as new Google events)
4. Do not delete an existing todo or routine to rebuild this

A Google recurring series (`recurrence` / `recurringEventId`) is not expanded into per-instance todos and is never patched or deleted as instances. With import on, and only when no todo already uses that title, GODA creates one `Routine` (`repeatType` from `FREQ`, `daysOfMonth` from `BYMONTHDAY`) plus one representative todo, mapped to the **master** event id.

## REST patterns used by this project

Base:

`https://firestore.googleapis.com/v1/projects/mate-914f3/databases/(default)/documents`

- Query: `POST …/documents:runQuery` with `Authorization: Bearer <idToken>`
- Create: `POST …/TodoItem?documentId=<id>`
- Create: `POST …/Routine?documentId=<id>`
- Update: `PATCH …/TodoItem/<id>?updateMask.fieldPaths=…`
- Delete: `DELETE …/TodoItem/<id>`
- Delete: `DELETE …/Routine/<id>`

## Mapping to Google Calendar

- `content` → event `summary`
- `date` → all-day event start, as an Asia/Seoul calendar day
- `memo` → event `description`
- Multi-day Google all-day → Todomate `Routine` (daily) + per-day todos, without deleting an existing item
- Recurring Google series → one Routine mapped to the master id, or skipped when the title already exists. Instances are not written
- Google HTML descriptions are converted to plain text before writing Todomate `memo`
- `g2d_owned=1` is set only when GODA creates the Google event. Updates never add it, and events without it are not patched or deleted
- A 403, 404, or other lookup error is not treated as "todo deleted"
- Completing a todo, or moving its goal to inactive (`finishType != null`), does not delete the Google event
- See `docs/sync-safety.md` for the deletion threshold and rollout
