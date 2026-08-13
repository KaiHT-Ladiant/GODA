package syncer

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/local/google-to-domate/internal/config"
	"github.com/local/google-to-domate/internal/googlecal"
	"github.com/local/google-to-domate/internal/models"
	"github.com/local/google-to-domate/internal/store"
	"github.com/local/google-to-domate/internal/textutil"
	"github.com/local/google-to-domate/internal/todomate"
)

type Engine struct {
	Settings *config.Settings
	Google   *googlecal.Client
	Todo     *todomate.Client
	Store    *store.Store
	Logf     func(format string, args ...any)
	stats    map[string]int
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (e *Engine) RunOnce(ctx context.Context) error {
	e.stats = map[string]int{}
	today := time.Now()
	start := dateOnly(today.AddDate(0, 0, -e.Settings.LookbackDays))
	end := dateOnly(today.AddDate(0, 0, e.Settings.LookaheadDays))
	timeMin := start
	timeMax := end.Add(24*time.Hour - time.Nanosecond)

	activeGoals, err := e.Todo.ListActiveGoals(e.Settings.IncludeGoalIDs, e.Settings.ExcludeGoalIDs)
	if err != nil {
		return err
	}
	activeGoalIDs := map[string]bool{}
	for _, g := range activeGoals {
		activeGoalIDs[g.ID] = true
		e.logf("Active goal: %s (%s)", g.Title, g.ID)
	}
	e.logf("Active Todomate goals: %d", len(activeGoals))

	todos, err := e.Todo.ListTodoItems(&start, &end)
	if err != nil {
		return err
	}
	activeTodos := make([]models.TodoItem, 0, len(todos))
	for _, t := range todos {
		if t.Date == nil || !activeGoalIDs[t.GoalID] {
			continue
		}
		if e.Settings.SkipCompletedTodos && t.IsDone {
			continue
		}
		activeTodos = append(activeTodos, t)
	}
	skipped := 0
	for _, t := range todos {
		if t.Date != nil && activeGoalIDs[t.GoalID] && e.Settings.SkipCompletedTodos && t.IsDone {
			skipped++
		}
	}
	if skipped > 0 {
		e.logf("Skipped completed Todomate items: %d", skipped)
	}
	e.logf("Sync window %s ~ %s | candidates=%d", start.Format("2006-01-02"), end.Format("2006-01-02"), len(activeTodos))

	events, syncToken, err := e.Google.ListEvents(ctx, timeMin, timeMax)
	if err != nil {
		return err
	}
	if syncToken != "" {
		_ = e.Store.SetState("google_sync_token", syncToken)
	}

	live := map[string]models.CalendarEvent{}
	var cancelled []models.CalendarEvent
	for _, ev := range events {
		if ev.Status == "cancelled" {
			cancelled = append(cancelled, ev)
			continue
		}
		live[ev.ID] = ev
	}
	todoByID := map[string]models.TodoItem{}
	for _, t := range activeTodos {
		todoByID[t.ID] = t
	}

	if err := e.pruneInactive(ctx, activeGoalIDs, todoByID); err != nil {
		return err
	}

	defaultGoal := e.Settings.DefaultGoalID
	defaultGoalTitle := ""
	if defaultGoal == "" && len(activeGoals) > 0 {
		sort.SliceStable(activeGoals, func(i, j int) bool {
			return activeGoals[i].Priority < activeGoals[j].Priority
		})
		defaultGoal = activeGoals[0].ID
		defaultGoalTitle = activeGoals[0].Title
	} else if defaultGoal != "" {
		for _, g := range activeGoals {
			if g.ID == defaultGoal {
				defaultGoalTitle = g.Title
				break
			}
		}
	}
	if e.Settings.ImportUnmappedGoogleEvents {
		e.logf("Google→Todomate import ON (default goal: %s [%s])", defaultGoalTitle, defaultGoal)
	} else {
		e.logf("Google→Todomate import OFF (import_unmapped_google_events=false)")
	}

	// Google first so existing calendar events are mapped before Todomate→Google create.
	for _, ev := range live {
		if err := e.syncGoogleToTodo(ctx, ev, todoByID, defaultGoal); err != nil {
			return err
		}
	}
	for _, ev := range cancelled {
		m, err := e.Store.GetByGoogle(ev.ID)
		if err != nil || m == nil {
			continue
		}
		e.logf("Google cancelled %s → delete Todomate %s", ev.ID, m.TodomateID)
		e.stats["google_cancelled_delete_todo"]++
		if !e.Settings.DryRun {
			e.deleteTodomateMapped(m.TodomateID)
			_ = e.Store.DeleteByGoogle(ev.ID)
		}
	}

	// Refresh mappings after Google→Todo; skip unmapped routine day-items.
	for _, t := range activeTodos {
		if t.RoutineID != "" {
			m, _ := e.Store.GetByTodomate(t.ID)
			if m == nil {
				continue
			}
		}
		if err := e.syncTodoToGoogle(ctx, t, live); err != nil {
			return err
		}
	}

	e.logf(
		"Sync summary (dry_run=%v): create_google=%d update_google=%d prune_google=%d create_todo=%d create_routine=%d update_todo=%d delete_todo=%d google_link_skipped=%d recover_mapping=%d",
		e.Settings.DryRun,
		e.stats["create_google"],
		e.stats["update_google"],
		e.stats["prune_google"],
		e.stats["create_todo"],
		e.stats["create_routine"],
		e.stats["update_todo"],
		e.stats["google_cancelled_delete_todo"],
		e.stats["google_link_skipped"],
		e.stats["recover_mapping"],
	)
	e.logf("Sync cycle complete (dry_run=%v)", e.Settings.DryRun)
	return nil
}

func (e *Engine) deleteTodomateMapped(todoID string) {
	remote, err := e.Todo.GetTodoItem(todoID)
	if err != nil {
		_ = e.Todo.DeleteTodoItem(todoID)
		return
	}
	if remote != nil && remote.RoutineID != "" {
		_ = e.Todo.DeleteRoutineAndTodos(remote.RoutineID)
		return
	}
	_ = e.Todo.DeleteTodoItem(todoID)
}

func (e *Engine) pruneInactive(ctx context.Context, activeGoalIDs map[string]bool, activeTodoByID map[string]models.TodoItem) error {
	mappings, err := e.Store.All()
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if _, ok := activeTodoByID[m.TodomateID]; ok {
			continue
		}
		remote, err := e.Todo.GetTodoItem(m.TodomateID)
		if err != nil {
			e.logf("Skip prune for %s (lookup failed: %v)", m.TodomateID, err)
			continue
		}
		reason := ""
		switch {
		case remote == nil:
			reason = "deleted_or_inaccessible"
		case !activeGoalIDs[remote.GoalID]:
			reason = "inactive_goal"
		case e.Settings.SkipCompletedTodos && remote.IsDone:
			reason = "completed"
		default:
			continue
		}
		e.logf("Prune Todomate %s (%s) → delete Google %s", m.TodomateID, reason, m.GoogleEventID)
		e.stats["prune_google"]++
		if !e.Settings.DryRun {
			_ = e.Google.DeleteEvent(ctx, m.GoogleEventID)
			_ = e.Store.DeleteByTodomate(m.TodomateID)
		}
	}
	return nil
}

func (e *Engine) syncTodoToGoogle(ctx context.Context, todo models.TodoItem, live map[string]models.CalendarEvent) error {
	if todo.Date == nil {
		return nil
	}
	m, err := e.Store.GetByTodomate(todo.ID)
	if err != nil {
		return err
	}
	fp := todo.Fingerprint()
	if m != nil && m.TodomateFingerprint == fp {
		return nil
	}
	if m == nil {
		// Already present on Google (imported earlier / link lost) → rematerialize mapping, never duplicate.
		if existing := findLiveGoogleForTodo(todo, live); existing != nil {
			e.logf("Recover mapping Todomate %s → Google %s (skip duplicate create)", todo.ID, existing.ID)
			e.stats["recover_mapping"]++
			if e.Settings.DryRun {
				return nil
			}
			return e.Store.Upsert(models.SyncMapping{
				TodomateID:          todo.ID,
				GoogleEventID:       existing.ID,
				TodomateFingerprint: fp,
				GoogleFingerprint:   existing.Fingerprint(),
				LastSyncedAt:        time.Now().UTC(),
				LastOrigin:          "recover",
			})
		}
		e.logf("Create Google event for Todomate %s (%s)", todo.ID, todo.Content)
		e.stats["create_google"]++
		if e.Settings.DryRun {
			return nil
		}
		created, err := e.Google.CreateEvent(ctx, todo.Content, *todo.Date, todo.Memo, todo.ID, true)
		if err != nil {
			return err
		}
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          todo.ID,
			GoogleEventID:       created.ID,
			TodomateFingerprint: fp,
			GoogleFingerprint:   created.Fingerprint(),
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "todomate",
		})
	}

	if ge, ok := live[m.GoogleEventID]; ok {
		googleChanged := ge.Fingerprint() != m.GoogleFingerprint
		todoChanged := fp != m.TodomateFingerprint
		if googleChanged && todoChanged {
			if !ge.Updated.Before(todo.UpdatedAt) || (ge.Updated.Equal(todo.UpdatedAt) && e.Settings.PreferGoogleOnTie) {
				return nil
			}
		} else if googleChanged && !todoChanged {
			return nil
		}
	}

	e.logf("Update Google event %s from Todomate %s", m.GoogleEventID, todo.ID)
	e.stats["update_google"]++
	if e.Settings.DryRun {
		return nil
	}
	start := *todo.Date
	end := time.Time{}
	allDay := true
	memo := todo.Memo
	if ge, ok := live[m.GoogleEventID]; ok {
		// Never shrink a multi-day Google event down to a single day from one todo.
		if googlecal.EventSpanDays(ge) > 1 {
			start = ge.Start
			end = ge.End
			allDay = ge.AllDay
		} else {
			allDay = ge.AllDay
			end = ge.End
		}
	}
	updated, err := e.Google.UpdateEvent(ctx, m.GoogleEventID, todo.Content, start, end, memo, todo.ID, allDay)
	if err != nil {
		return err
	}
	return e.Store.Upsert(models.SyncMapping{
		TodomateID:          todo.ID,
		GoogleEventID:       updated.ID,
		TodomateFingerprint: fp,
		GoogleFingerprint:   updated.Fingerprint(),
		LastSyncedAt:        time.Now().UTC(),
		LastOrigin:          "todomate",
	})
}

func (e *Engine) syncGoogleToTodo(ctx context.Context, event models.CalendarEvent, todoByID map[string]models.TodoItem, defaultGoal string) error {
	if event.Status == "cancelled" {
		return nil
	}
	m, err := e.Store.GetByGoogle(event.ID)
	if err != nil {
		return err
	}
	if m == nil && event.TodomateID != "" {
		m, err = e.Store.GetByTodomate(event.TodomateID)
		if err != nil {
			return err
		}
	}
	fp := event.Fingerprint()
	onDate := googlecal.EventDate(event)
	span := googlecal.EventSpanDays(event)
	needsMultiDayRoutine := event.AllDay && span > 1

	if m != nil {
		todo, hasTodo := todoByID[m.TodomateID]
		if !hasTodo {
			if remote, err := e.Todo.GetTodoItem(m.TodomateID); err == nil && remote != nil {
				todo = *remote
				hasTodo = true
			}
		}

		// Already synced as a single day but Google event spans multiple days → rebuild as routine.
		if needsMultiDayRoutine && hasTodo && todo.RoutineID == "" {
			return e.createOrRebuildRoutineFromGoogle(ctx, event, defaultGoalOr(todo.GoalID, defaultGoal), m)
		}

		if m.GoogleFingerprint == fp {
			// Clean up previously synced HTML memos even when fingerprint is unchanged.
			if hasTodo && textutil.LooksLikeHTML(todo.Memo) {
				plain := textutil.FromHTML(event.Description)
				if plain != todo.Memo {
					e.logf("Strip HTML memo on Todomate %s from Google %s", m.TodomateID, event.ID)
					e.stats["update_todo"]++
					if !e.Settings.DryRun {
						if _, err := e.Todo.UpdateTodoItem(m.TodomateID, nil, nil, &plain); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}

		if needsMultiDayRoutine {
			goal := defaultGoal
			if hasTodo {
				goal = defaultGoalOr(todo.GoalID, defaultGoal)
			}
			return e.createOrRebuildRoutineFromGoogle(ctx, event, goal, m)
		}

		if hasTodo {
			googleNewer := !event.Updated.Before(todo.UpdatedAt)
			if !googleNewer && !e.Settings.PreferGoogleOnTie {
				return nil
			}
			e.logf("Update Todomate %s from Google %s", m.TodomateID, event.ID)
			e.stats["update_todo"]++
			if e.Settings.DryRun {
				return nil
			}
			summary := event.Summary
			desc := textutil.FromHTML(event.Description)
			// If previous mapping was a routine but event is now single-day, drop the routine.
			if todo.RoutineID != "" {
				_ = e.Todo.DeleteRoutineAndTodos(todo.RoutineID)
				created, err := e.Todo.CreateTodoItem(orUntitled(event.Summary), defaultGoalOr(todo.GoalID, defaultGoal), onDate, desc, "")
				if err != nil {
					return err
				}
				e.linkGoogleQuietly(ctx, event, created.ID)
				return e.Store.Upsert(models.SyncMapping{
					TodomateID:          created.ID,
					GoogleEventID:       event.ID,
					TodomateFingerprint: created.Fingerprint(),
					GoogleFingerprint:   fp,
					LastSyncedAt:        time.Now().UTC(),
					LastOrigin:          "google",
				})
			}
			updated, err := e.Todo.UpdateTodoItem(m.TodomateID, &summary, &onDate, &desc)
			if err != nil {
				return err
			}
			return e.Store.Upsert(models.SyncMapping{
				TodomateID:          updated.ID,
				GoogleEventID:       event.ID,
				TodomateFingerprint: updated.Fingerprint(),
				GoogleFingerprint:   fp,
				LastSyncedAt:        time.Now().UTC(),
				LastOrigin:          "google",
			})
		}
		if defaultGoal == "" {
			e.logf("No default_goal_id; skip creating Todomate for %s", event.ID)
			return nil
		}
		return e.createOrRebuildRoutineFromGoogle(ctx, event, defaultGoal, m)
	}

	if event.OriginSync && event.TodomateID != "" {
		if todo, ok := todoByID[event.TodomateID]; ok {
			return e.Store.Upsert(models.SyncMapping{
				TodomateID:          todo.ID,
				GoogleEventID:       event.ID,
				TodomateFingerprint: todo.Fingerprint(),
				GoogleFingerprint:   fp,
				LastSyncedAt:        time.Now().UTC(),
				LastOrigin:          "recover",
			})
		}
		return nil
	}
	if !e.Settings.ImportUnmappedGoogleEvents {
		return nil
	}
	if defaultGoal == "" {
		e.logf("Skip Google→Todomate create for %s (set todomate.default_goal_id)", event.ID)
		return nil
	}
	// Matching Todomate item already exists (previous import / manual) → map, don't recreate.
	if existing := findTodoForGoogleEvent(event, todoByID); existing != nil {
		e.logf("Recover mapping Google %s → Todomate %s (skip duplicate create)", event.ID, existing.ID)
		e.stats["recover_mapping"]++
		if e.Settings.DryRun {
			return nil
		}
		e.linkGoogleQuietly(ctx, event, existing.ID)
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          existing.ID,
			GoogleEventID:       event.ID,
			TodomateFingerprint: existing.Fingerprint(),
			GoogleFingerprint:   fp,
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "recover",
		})
	}
	return e.createOrRebuildRoutineFromGoogle(ctx, event, defaultGoal, nil)
}

func defaultGoalOr(goalID, fallback string) string {
	if goalID != "" {
		return goalID
	}
	return fallback
}

func (e *Engine) createOrRebuildRoutineFromGoogle(ctx context.Context, event models.CalendarEvent, defaultGoal string, existing *models.SyncMapping) error {
	onDate := googlecal.EventDate(event)
	span := googlecal.EventSpanDays(event)
	endInclusive := googlecal.EventInclusiveEnd(event)
	title := orUntitled(event.Summary)
	memo := textutil.FromHTML(event.Description)
	fp := event.Fingerprint()

	if existing != nil && !e.Settings.DryRun {
		e.deleteTodomateMapped(existing.TodomateID)
		_ = e.Store.DeleteByGoogle(event.ID)
	}

	if event.AllDay && span > 1 {
		e.logf(
			"Create Todomate routine from Google %s (%s) %s~%s (%d days)",
			event.ID, title, onDate.Format("2006-01-02"), endInclusive.Format("2006-01-02"), span,
		)
		e.stats["create_routine"]++
		e.stats["create_todo"] += span
		if e.Settings.DryRun {
			return nil
		}
		_, first, _, err := e.Todo.CreateRoutineWithDailyTodos(title, defaultGoal, memo, onDate, endInclusive)
		if err != nil {
			return err
		}
		e.linkGoogleQuietly(ctx, event, first.ID)
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          first.ID,
			GoogleEventID:       event.ID,
			TodomateFingerprint: first.Fingerprint(),
			GoogleFingerprint:   fp,
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "google",
		})
	}

	e.logf("Create Todomate item from Google %s (%s)", event.ID, title)
	e.stats["create_todo"]++
	if e.Settings.DryRun {
		return nil
	}
	created, err := e.Todo.CreateTodoItem(title, defaultGoal, onDate, memo, "")
	if err != nil {
		return err
	}
	e.linkGoogleQuietly(ctx, event, created.ID)
	return e.Store.Upsert(models.SyncMapping{
		TodomateID:          created.ID,
		GoogleEventID:       event.ID,
		TodomateFingerprint: created.Fingerprint(),
		GoogleFingerprint:   fp,
		LastSyncedAt:        time.Now().UTC(),
		LastOrigin:          "google",
	})
}

// linkGoogleQuietly attaches Todomate id to the Google event when permitted.
// Some calendars/events reject private extendedProperties (PERMISSION_DENIED);
// Todomate-side sync must still succeed in that case.
func (e *Engine) linkGoogleQuietly(ctx context.Context, event models.CalendarEvent, todomateID string) {
	_, err := e.Google.UpdateEvent(ctx, event.ID, event.Summary, event.Start, event.End, event.Description, todomateID, event.AllDay)
	if err != nil {
		e.logf("Warn: Google link skipped for %s (%s): %v", event.ID, event.Summary, err)
		e.stats["google_link_skipped"]++
	}
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func orUntitled(s string) string {
	if s == "" {
		return "(untitled)"
	}
	return s
}

func sameTitle(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func findLiveGoogleForTodo(todo models.TodoItem, live map[string]models.CalendarEvent) *models.CalendarEvent {
	if todo.Date == nil {
		return nil
	}
	day := dateOnly(*todo.Date)
	var byTitle *models.CalendarEvent
	for _, ev := range live {
		evCopy := ev
		if ev.TodomateID != "" && ev.TodomateID == todo.ID {
			return &evCopy
		}
		if !sameTitle(ev.Summary, todo.Content) {
			continue
		}
		start := googlecal.EventDate(ev)
		end := googlecal.EventInclusiveEnd(ev)
		if !day.Before(start) && !day.After(end) {
			// Prefer events already marked as sync-origin.
			if ev.OriginSync || ev.TodomateID != "" {
				return &evCopy
			}
			if byTitle == nil {
				byTitle = &evCopy
			}
		}
	}
	return byTitle
}

func findTodoForGoogleEvent(event models.CalendarEvent, todoByID map[string]models.TodoItem) *models.TodoItem {
	if event.TodomateID != "" {
		if t, ok := todoByID[event.TodomateID]; ok {
			return &t
		}
	}
	start := googlecal.EventDate(event)
	end := googlecal.EventInclusiveEnd(event)
	title := orUntitled(event.Summary)
	var match *models.TodoItem
	for _, t := range todoByID {
		if t.Date == nil || !sameTitle(t.Content, title) {
			continue
		}
		day := dateOnly(*t.Date)
		if day.Before(start) || day.After(end) {
			continue
		}
		tCopy := t
		// Prefer start-day item / mapped-looking primary.
		if day.Equal(start) {
			return &tCopy
		}
		if match == nil {
			match = &tCopy
		}
	}
	return match
}

func SummaryLine(stats map[string]int, dryRun bool) string {
	return fmt.Sprintf("dry_run=%v create_google=%d", dryRun, stats["create_google"])
}
