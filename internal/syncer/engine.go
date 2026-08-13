package syncer

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/local/google-to-domate/internal/config"
	"github.com/local/google-to-domate/internal/googlecal"
	"github.com/local/google-to-domate/internal/models"
	"github.com/local/google-to-domate/internal/store"
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
	for _, t := range activeTodos {
		if err := e.syncTodoToGoogle(ctx, t, live); err != nil {
			return err
		}
	}

	defaultGoal := e.Settings.DefaultGoalID
	if defaultGoal == "" && len(activeGoals) > 0 {
		sort.SliceStable(activeGoals, func(i, j int) bool {
			return activeGoals[i].Priority < activeGoals[j].Priority
		})
		defaultGoal = activeGoals[0].ID
	}
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
			_ = e.Todo.DeleteTodoItem(m.TodomateID)
			_ = e.Store.DeleteByGoogle(ev.ID)
		}
	}

	e.logf(
		"Sync summary (dry_run=%v): create_google=%d update_google=%d prune_google=%d create_todo=%d update_todo=%d delete_todo=%d",
		e.Settings.DryRun,
		e.stats["create_google"],
		e.stats["update_google"],
		e.stats["prune_google"],
		e.stats["create_todo"],
		e.stats["update_todo"],
		e.stats["google_cancelled_delete_todo"],
	)
	e.logf("Sync cycle complete (dry_run=%v)", e.Settings.DryRun)
	return nil
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
			reason = "deleted"
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
	updated, err := e.Google.UpdateEvent(ctx, m.GoogleEventID, todo.Content, *todo.Date, time.Time{}, todo.Memo, todo.ID, true)
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

	if m != nil {
		if m.GoogleFingerprint == fp {
			return nil
		}
		if todo, ok := todoByID[m.TodomateID]; ok {
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
			desc := event.Description
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
		e.logf("Recreate Todomate for Google %s", event.ID)
		e.stats["create_todo"]++
		if e.Settings.DryRun {
			return nil
		}
		created, err := e.Todo.CreateTodoItem(orUntitled(event.Summary), defaultGoal, onDate, event.Description)
		if err != nil {
			return err
		}
		_, _ = e.Google.UpdateEvent(ctx, event.ID, event.Summary, event.Start, event.End, event.Description, created.ID, event.AllDay)
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          created.ID,
			GoogleEventID:       event.ID,
			TodomateFingerprint: created.Fingerprint(),
			GoogleFingerprint:   fp,
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "google",
		})
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
	e.logf("Create Todomate item from Google %s (%s)", event.ID, event.Summary)
	e.stats["create_todo"]++
	if e.Settings.DryRun {
		return nil
	}
	created, err := e.Todo.CreateTodoItem(orUntitled(event.Summary), defaultGoal, onDate, event.Description)
	if err != nil {
		return err
	}
	linked, err := e.Google.UpdateEvent(ctx, event.ID, event.Summary, event.Start, event.End, event.Description, created.ID, event.AllDay)
	if err != nil {
		return err
	}
	return e.Store.Upsert(models.SyncMapping{
		TodomateID:          created.ID,
		GoogleEventID:       linked.ID,
		TodomateFingerprint: created.Fingerprint(),
		GoogleFingerprint:   linked.Fingerprint(),
		LastSyncedAt:        time.Now().UTC(),
		LastOrigin:          "google",
	})
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

func SummaryLine(stats map[string]int, dryRun bool) string {
	return fmt.Sprintf("dry_run=%v create_google=%d", dryRun, stats["create_google"])
}
