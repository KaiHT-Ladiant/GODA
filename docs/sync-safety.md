# Sync safety

GODA is a local two-way sync between Google Calendar and Todomate. A cycle could previously destroy data that the user did not create inside GODA:

- Monthly series were expanded with `singleEvents=true`. PATCH and DELETE then used the **instance** id (`recurringEventId_YYYYMMDD`), which drops or exceptions a single occurrence. A series such as “월세 지급 💲” (14th of each month) lost occurrences that way.
- Matching by title + calendar day attached an existing Todomate item (for example an OSCP study block) to a calendar event GODA did not create. A later “rebuild” deleted that todo and created another one.
- `linkGoogleQuietly` wrote `g2d_origin` / `g2d_todomate_id` onto the user’s own events, so the next cycle treated them as sync-owned.
- Prune deleted the Google event when the linked todo was completed, its goal was finished, or Todomate returned 403 or 404 (both were collapsed to “missing”).
- Delete calls ignored errors and removed the local link anyway, so a failed delete looked like success.
- All-day and timed dates were read as UTC while timed writes sent `timeZone=Asia/Seoul`.

## Rules

| Situation | What GODA does |
| --- | --- |
| Google event has no `g2d_owned=1` | No PATCH, no DELETE. A local read-only mapping is allowed so a second event is not created. |
| Event is a recurring instance, or the master has `recurrence` | No Google write. Import, if enabled, creates one Routine for the master id, unless a todo with that title already exists — then it does nothing. |
| Todo is completed | The todo is skipped as a sync source. The calendar event stays. |
| `GetTodoItem` returns 403, 404, 5xx, or a network error | The link stays. Nothing is deleted or recreated. |
| Goal `finishType` is set | The Google event stays. |
| Google reports a non-recurring **owned** event as `cancelled` | The mapping is a deletion candidate. |
| Deletion candidates > `max_deletions_per_run` (default 3) | The whole cycle aborts. No tombstone, no remote write. |
| Deletion candidates within the threshold, `dry_run=false`, `hard_delete=false` | Tombstone the mapping row (`tombstoned_at`). Do not delete either remote object. |
| Same, `hard_delete=true` | Delete the Todomate item, and only tombstone after that delete succeeds. |
| Update or delete | Send `If-Match` with the newest etag. On failure, leave the mapping row as it was. |

`g2d_owned` is not backfilled onto events that already exist. Events created before this change are treated as not owned, so GODA will stop pushing edits to them instead of risking another overwrite. Create a new todo (or delete the old Google event yourself and let a dry-run show a create) if you want GODA to own a fresh copy.

`nextSyncToken` is still returned by the list API and is not used as the cursor. A token cannot be combined with `timeMin`, and a full window is what makes instance detection possible. The window itself is Asia/Seoul calendar days.

## Rollout

1. Copy `sync.sqlite3` and, in Google Calendar, export or duplicate the calendar you sync. GODA will not delete Google events, but a bad local mapping is easier to undo from a copy.
2. Keep `dry_run = true`. If the key is missing, the default is also true. An existing `config.local.toml` that sets `dry_run = false` is unchanged — set it back to true for the first cycles after upgrading.
3. Leave `hard_delete = false` and `max_deletions_per_run = 3`.
4. Run one cycle from the Go GUI (`cmd/g2d`) and read the log. You want `skip_recurring` for series such as rent, `completed_kept` / `lookup_kept` instead of deletes, and no `update_google` against events you created by hand.
5. The Python command `google-to-domate` now refuses to run. Do not re-enable it against a live calendar.
6. Turn `dry_run` off only after the preview matches what you expect. The first non-dry-run cycle still will not delete Google events. It can create new owned events for todos that are not already mapped and that do not collide with an unowned series.

If a cycle aborts with `aborting sync: N proposed deletions exceed threshold`, do not raise the threshold to push the run through. The candidates are owned events Google already shows as cancelled. Inspect them, then raise the threshold only if retiring those links is intentional.
