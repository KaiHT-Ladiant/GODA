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
	"github.com/local/google-to-domate/internal/seoul"
	"github.com/local/google-to-domate/internal/store"
	"github.com/local/google-to-domate/internal/textutil"
	"github.com/local/google-to-domate/internal/todomate"
)

// CalendarClient is the slice of Google Calendar used by the engine.
type CalendarClient interface {
	ListEvents(ctx context.Context, timeMin, timeMax time.Time) ([]models.CalendarEvent, string, error)
	GetEvent(ctx context.Context, eventID string) (models.CalendarEvent, error)
	CreateEvent(ctx context.Context, w googlecal.EventWrite) (models.CalendarEvent, error)
	UpdateEvent(ctx context.Context, eventID string, w googlecal.EventWrite) (models.CalendarEvent, error)
	DeleteEvent(ctx context.Context, eventID, etag string) error
}

// TodoClient is the slice of Todomate used by the engine.
type TodoClient interface {
	ListActiveGoals(include, exclude []string) ([]models.Goal, error)
	ListTodoItems(start, end *time.Time) ([]models.TodoItem, error)
	GetTodoItem(id string) (*models.TodoItem, error)
	CreateTodoItem(content, goalID string, onDate time.Time, memo, routineID string) (models.TodoItem, error)
	UpdateTodoItem(id string, content *string, onDate *time.Time, memo *string) (models.TodoItem, error)
	DeleteTodoItem(id string) error
	DeleteRoutineAndTodos(routineID string) error
	CreateRoutineWithDailyTodos(title, goalID, memo string, start, endInclusive time.Time) (todomate.Routine, models.TodoItem, int, error)
	CreateSeriesRoutine(title, goalID, memo string, onDate time.Time, repeatType int, daysOfMonth []int) (todomate.Routine, models.TodoItem, error)
}

// DeletionThresholdError aborts a cycle before any tombstone or remote delete.
type DeletionThresholdError struct {
	Proposed  int
	Threshold int
}

func (e *DeletionThresholdError) Error() string {
	return fmt.Sprintf("aborting sync: %d proposed deletions exceed threshold %d", e.Proposed, e.Threshold)
}

type proposedDeletion struct {
	Mapping models.SyncMapping
	Event   models.CalendarEvent
	Reason  string
}

type Engine struct {
	Settings *config.Settings
	Google   CalendarClient
	Todo     TodoClient
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
	start, end, timeMin, timeMax := e.bounds(time.Now())

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
	skipped := 0
	for _, t := range todos {
		if t.Date == nil || !activeGoalIDs[t.GoalID] {
			continue
		}
		if e.Settings.SkipCompletedTodos && t.IsDone {
			skipped++
			continue
		}
		activeTodos = append(activeTodos, t)
	}
	if skipped > 0 {
		e.logf("Skipped completed Todomate items: %d", skipped)
	}
	e.logf("Sync window %s ~ %s | candidates=%d", start.Format("2006-01-02"), end.Format("2006-01-02"), len(activeTodos))

	events, _, err := e.Google.ListEvents(ctx, timeMin, timeMax)
	if err != nil {
		return err
	}
	live, cancelled, series := partitionEvents(events)

	todoByID := map[string]models.TodoItem{}
	for _, t := range activeTodos {
		todoByID[t.ID] = t
	}

	// Count destructive work before any write. Over the threshold, do nothing.
	proposals, err := e.deletionProposals(cancelled)
	if err != nil {
		return err
	}
	if err := e.guardDeletions(proposals); err != nil {
		return err
	}

	if err := e.reviewUnlistedMappings(activeGoalIDs, todoByID); err != nil {
		return err
	}

	defaultGoal, defaultGoalTitle := e.pickDefaultGoal(activeGoals)
	if e.Settings.ImportUnmappedGoogleEvents {
		e.logf("Google→Todomate import ON (default goal: %s [%s])", defaultGoalTitle, defaultGoal)
	} else {
		e.logf("Google→Todomate import OFF (import_unmapped_google_events=false)")
	}

	for masterID, instances := range series {
		if err := e.syncRecurringSeries(ctx, masterID, instances, todos, defaultGoal); err != nil {
			return err
		}
	}
	for _, ev := range live {
		if err := e.syncGoogleToTodo(ctx, ev, todoByID, defaultGoal); err != nil {
			return err
		}
	}
	for _, t := range activeTodos {
		if t.RoutineID != "" {
			m, _ := e.Store.GetByTodomate(t.ID)
			if m == nil {
				continue
			}
		}
		if err := e.syncTodoToGoogle(ctx, t, live, series); err != nil {
			return err
		}
	}
	if err := e.applyDeletions(ctx, proposals); err != nil {
		return err
	}

	e.logf(
		"Sync summary (dry_run=%v hard_delete=%v): create_google=%d update_google=%d create_todo=%d create_routine=%d update_todo=%d tombstone=%d delete_failed=%d skip_google_write=%d skip_recurring=%d lookup_kept=%d completed_kept=%d google_link_skipped=%d recover_mapping=%d",
		e.Settings.DryRun,
		e.Settings.HardDelete,
		e.stats["create_google"],
		e.stats["update_google"],
		e.stats["create_todo"],
		e.stats["create_routine"],
		e.stats["update_todo"],
		e.stats["tombstone"],
		e.stats["delete_failed"],
		e.stats["skip_google_write"],
		e.stats["skip_recurring"],
		e.stats["lookup_kept"],
		e.stats["completed_kept"],
		e.stats["google_link_skipped"],
		e.stats["recover_mapping"],
	)
	e.logf("Sync cycle complete (dry_run=%v)", e.Settings.DryRun)
	return nil
}

func (e *Engine) bounds(now time.Time) (start, end, timeMin, timeMax time.Time) {
	today := seoul.Midnight(now)
	start = today.AddDate(0, 0, -e.Settings.LookbackDays)
	end = today.AddDate(0, 0, e.Settings.LookaheadDays)
	// Calendar timeMin is exclusive, so step back to keep the first day.
	timeMin = start.Add(-time.Second)
	timeMax = end.AddDate(0, 0, 1)
	return start, end, timeMin, timeMax
}

func (e *Engine) pickDefaultGoal(activeGoals []models.Goal) (id, title string) {
	id = e.Settings.DefaultGoalID
	if id == "" && len(activeGoals) > 0 {
		sort.SliceStable(activeGoals, func(i, j int) bool {
			return activeGoals[i].Priority < activeGoals[j].Priority
		})
		return activeGoals[0].ID, activeGoals[0].Title
	}
	for _, g := range activeGoals {
		if g.ID == id {
			return id, g.Title
		}
	}
	return id, ""
}

func partitionEvents(events []models.CalendarEvent) (live map[string]models.CalendarEvent, cancelled []models.CalendarEvent, series map[string][]models.CalendarEvent) {
	live = map[string]models.CalendarEvent{}
	series = map[string][]models.CalendarEvent{}
	for _, ev := range events {
		if ev.RecurringEventID != "" {
			series[ev.RecurringEventID] = append(series[ev.RecurringEventID], ev)
			continue
		}
		if len(ev.Recurrence) > 0 {
			series[ev.ID] = append(series[ev.ID], ev)
			continue
		}
		if ev.Status == "cancelled" {
			cancelled = append(cancelled, ev)
			continue
		}
		live[ev.ID] = ev
	}
	return live, cancelled, series
}

// deletionProposals retires a link only when a non-recurring event GODA
// created was cancelled on Google. Completion, 403, 404, and unowned events
// are not proposals.
func (e *Engine) deletionProposals(cancelled []models.CalendarEvent) ([]proposedDeletion, error) {
	var out []proposedDeletion
	for _, ev := range cancelled {
		if ev.RecurringEventID != "" || len(ev.Recurrence) > 0 {
			e.logf("Skip cancelled recurring event %s", ev.ID)
			e.stats["skip_recurring"]++
			continue
		}
		m, err := e.Store.GetByGoogle(ev.ID)
		if err != nil {
			return nil, err
		}
		if m == nil || m.Tombstoned() {
			continue
		}
		if !m.GoogleOwned && !ev.Owned {
			e.logf("Skip cancelled unowned Google %s; keep Todomate %s", ev.ID, m.TodomateID)
			e.stats["skip_unowned_cancel"]++
			continue
		}
		out = append(out, proposedDeletion{Mapping: *m, Event: ev, Reason: "google_cancelled"})
	}
	return out, nil
}

func (e *Engine) guardDeletions(proposals []proposedDeletion) error {
	max := e.Settings.MaxDeletionsPerRun
	if max < 0 {
		max = 0
	}
	if len(proposals) > max {
		for _, p := range proposals {
			e.logf("Deletion blocked (%s): todo=%s google=%s", p.Reason, p.Mapping.TodomateID, p.Mapping.GoogleEventID)
		}
		e.logf("Aborting sync: %d proposed deletions exceed threshold %d", len(proposals), max)
		return &DeletionThresholdError{Proposed: len(proposals), Threshold: max}
	}
	return nil
}

func (e *Engine) applyDeletions(ctx context.Context, proposals []proposedDeletion) error {
	for _, p := range proposals {
		e.logf("Tombstone link todo=%s google=%s (%s) hard_delete=%v dry_run=%v", p.Mapping.TodomateID, p.Mapping.GoogleEventID, p.Reason, e.Settings.HardDelete, e.Settings.DryRun)
		e.stats["tombstone"]++
		if e.Settings.DryRun {
			continue
		}
		if e.Settings.HardDelete {
			if err := e.hardDeleteTodo(p.Mapping.TodomateID); err != nil {
				e.logf("Delete failed, keeping link todo=%s google=%s: %v", p.Mapping.TodomateID, p.Mapping.GoogleEventID, err)
				e.stats["delete_failed"]++
				continue
			}
		}
		if err := e.Store.Tombstone(p.Mapping.TodomateID, p.Reason); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) hardDeleteTodo(todoID string) error {
	remote, err := e.Todo.GetTodoItem(todoID)
	if err != nil {
		return err
	}
	if remote == nil {
		return fmt.Errorf("todomate: empty todo %s", todoID)
	}
	if remote.RoutineID != "" {
		return e.Todo.DeleteRoutineAndTodos(remote.RoutineID)
	}
	return e.Todo.DeleteTodoItem(todoID)
}

// reviewUnlistedMappings explains why a mapped todo is absent from the active
// set. It never deletes a Google event or a Todomate item.
func (e *Engine) reviewUnlistedMappings(activeGoalIDs map[string]bool, activeTodoByID map[string]models.TodoItem) error {
	mappings, err := e.Store.All()
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.Tombstoned() {
			continue
		}
		if _, ok := activeTodoByID[m.TodomateID]; ok {
			continue
		}
		remote, err := e.Todo.GetTodoItem(m.TodomateID)
		if err != nil {
			e.logf("Keep link todo=%s google=%s (lookup error is not a deletion: %v)", m.TodomateID, m.GoogleEventID, err)
			e.stats["lookup_kept"]++
			continue
		}
		if remote == nil {
			e.logf("Keep link todo=%s google=%s (empty lookup is not a deletion)", m.TodomateID, m.GoogleEventID)
			e.stats["lookup_kept"]++
			continue
		}
		switch {
		case remote.IsDone:
			e.logf("Completed todo %s does not delete Google %s", m.TodomateID, m.GoogleEventID)
			e.stats["completed_kept"]++
		case !activeGoalIDs[remote.GoalID]:
			e.logf("Inactive goal for todo %s does not delete Google %s", m.TodomateID, m.GoogleEventID)
			e.stats["inactive_goal_kept"]++
		default:
			e.logf("Mapped todo %s is outside the active set; leaving Google %s", m.TodomateID, m.GoogleEventID)
		}
	}
	return nil
}

func (e *Engine) syncRecurringSeries(ctx context.Context, masterID string, instances []models.CalendarEvent, todos []models.TodoItem, defaultGoal string) error {
	// Instances are never patched or deleted. A failed master lookup still
	// leaves every instance alone.
	master, err := e.Google.GetEvent(ctx, masterID)
	if err != nil {
		e.logf("Skip recurring series %s (master lookup failed: %v); instances not modified", masterID, err)
		e.stats["skip_recurring"]++
		return nil
	}
	e.stats["skip_recurring"]++
	e.logf("Skip Google writes for recurring series %s (%s, owned=%v)", masterID, master.Summary, master.Owned)

	if m, err := e.Store.GetByGoogle(masterID); err != nil {
		return err
	} else if m != nil {
		return nil
	}
	for _, inst := range instances {
		m, err := e.Store.GetByGoogle(inst.ID)
		if err != nil {
			return err
		}
		if m != nil {
			e.logf("Recurring instance %s stays mapped to Todomate %s; not rebuilding", inst.ID, m.TodomateID)
			return nil
		}
	}
	if !e.Settings.ImportUnmappedGoogleEvents {
		return nil
	}
	title := orUntitled(master.Summary)
	if title == "(untitled)" && len(instances) > 0 {
		title = orUntitled(instances[0].Summary)
	}
	for _, t := range todos {
		if sameTitle(t.Content, title) {
			e.logf("Recurring series %s matches existing Todomate %q; not importing or rebuilding", masterID, title)
			e.stats["skip_series_existing"]++
			return nil
		}
	}
	if defaultGoal == "" {
		e.logf("Skip series import %s (set todomate.default_goal_id)", masterID)
		return nil
	}
	onDate := time.Time{}
	for _, inst := range instances {
		d := googlecal.EventDate(inst)
		if onDate.IsZero() || d.Before(onDate) {
			onDate = d
		}
	}
	if onDate.IsZero() {
		onDate = googlecal.EventDate(master)
	}
	repeat, days := seriesRepeat(master.Recurrence, onDate)
	e.logf("Create one Todomate routine for recurring series %s (%s)", masterID, title)
	e.stats["create_routine"]++
	e.stats["create_todo"]++
	if e.Settings.DryRun {
		return nil
	}
	_, first, err := e.Todo.CreateSeriesRoutine(title, defaultGoal, textutil.FromHTML(master.Description), onDate, repeat, days)
	if err != nil {
		return err
	}
	return e.Store.Upsert(models.SyncMapping{
		TodomateID:          first.ID,
		GoogleEventID:       masterID,
		TodomateFingerprint: first.Fingerprint(),
		GoogleFingerprint:   master.Fingerprint(),
		LastSyncedAt:        time.Now().UTC(),
		LastOrigin:          "google",
		GoogleETag:          master.ETag,
		GoogleOwned:         master.Owned,
	})
}

func seriesRepeat(recurrence []string, sample time.Time) (int, []int) {
	repeat := todomate.RoutineRepeatMonthly
	var days []int
	for _, line := range recurrence {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if !strings.HasPrefix(upper, "RRULE:") {
			continue
		}
		freq := ""
		for _, part := range strings.Split(strings.TrimPrefix(upper, "RRULE:"), ";") {
			k, v, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			switch k {
			case "FREQ":
				freq = v
			case "BYMONTHDAY":
				for _, piece := range strings.Split(v, ",") {
					var n int
					if _, err := fmt.Sscanf(piece, "%d", &n); err == nil && n != 0 {
						days = append(days, n)
					}
				}
			}
		}
		switch freq {
		case "DAILY":
			repeat = todomate.RoutineRepeatDaily
		case "WEEKLY":
			repeat = todomate.RoutineRepeatWeekly
		case "YEARLY":
			repeat = todomate.RoutineRepeatYearly
		default:
			repeat = todomate.RoutineRepeatMonthly
		}
	}
	if repeat == todomate.RoutineRepeatMonthly && len(days) == 0 && !sample.IsZero() {
		days = []int{sample.In(seoul.Location).Day()}
	}
	return repeat, days
}

func (e *Engine) syncTodoToGoogle(ctx context.Context, todo models.TodoItem, live map[string]models.CalendarEvent, series map[string][]models.CalendarEvent) error {
	if todo.Date == nil {
		return nil
	}
	m, err := e.Store.GetByTodomate(todo.ID)
	if err != nil {
		return err
	}
	if m != nil && m.Tombstoned() {
		return nil
	}
	fp := todo.Fingerprint()
	if m != nil && m.TodomateFingerprint == fp {
		return nil
	}
	if m == nil {
		if existing := findLiveForTodo(todo, live, true); existing != nil {
			prev, err := e.Store.GetByGoogle(existing.ID)
			if err != nil {
				return err
			}
			if prev != nil && prev.TodomateID != todo.ID {
				e.logf("Owned Google %s already mapped; not creating another event for Todomate %s", existing.ID, todo.ID)
				return nil
			}
			e.logf("Recover mapping Todomate %s → owned Google %s", todo.ID, existing.ID)
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
				GoogleETag:          existing.ETag,
				GoogleOwned:         true,
			})
		}
		if matchesUnownedSeries(todo, series) {
			e.logf("Skip Google create for Todomate %s; title/date matches an unowned recurring series", todo.ID)
			e.stats["skip_unowned_recurring"]++
			return nil
		}
		if existing := findLiveForTodo(todo, live, false); existing != nil {
			prev, err := e.Store.GetByGoogle(existing.ID)
			if err != nil {
				return err
			}
			if prev != nil {
				e.logf("Unowned Google %s already linked to Todomate %s; not creating another event for %s", existing.ID, prev.TodomateID, todo.ID)
				e.stats["skip_google_write"]++
				return nil
			}
			e.logf("Read-only map Todomate %s → unowned Google %s (no Google write)", todo.ID, existing.ID)
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
				GoogleETag:          existing.ETag,
				GoogleOwned:         false,
			})
		}
		e.logf("Create Google event for Todomate %s (%s)", todo.ID, todo.Content)
		e.stats["create_google"]++
		if e.Settings.DryRun {
			return nil
		}
		created, err := e.Google.CreateEvent(ctx, googlecal.EventWrite{
			Summary:     todo.Content,
			Start:       *todo.Date,
			Description: todo.Memo,
			TodomateID:  todo.ID,
			AllDay:      true,
		})
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
			GoogleETag:          created.ETag,
			GoogleOwned:         true,
		})
	}

	ge, inLive := live[m.GoogleEventID]
	if !inLive || !canWriteGoogle(ge, m) {
		e.logf("Skip Google write for %s (not an owned single event created by GODA)", m.GoogleEventID)
		e.stats["skip_google_write"]++
		return nil
	}

	googleChanged := ge.Fingerprint() != m.GoogleFingerprint
	todoChanged := fp != m.TodomateFingerprint
	if googleChanged && todoChanged {
		if !ge.Updated.Before(todo.UpdatedAt) || (ge.Updated.Equal(todo.UpdatedAt) && e.Settings.PreferGoogleOnTie) {
			return nil
		}
	} else if googleChanged && !todoChanged {
		return nil
	}

	e.logf("Update owned Google event %s from Todomate %s", m.GoogleEventID, todo.ID)
	e.stats["update_google"]++
	if e.Settings.DryRun {
		return nil
	}
	start := *todo.Date
	end := time.Time{}
	allDay := true
	memo := todo.Memo
	if googlecal.EventSpanDays(ge) > 1 {
		start = ge.Start
		end = ge.End
		allDay = ge.AllDay
	} else {
		allDay = ge.AllDay
		end = ge.End
	}
	etag := m.GoogleETag
	if ge.ETag != "" {
		etag = ge.ETag
	}
	updated, err := e.Google.UpdateEvent(ctx, m.GoogleEventID, googlecal.EventWrite{
		Summary:     todo.Content,
		Start:       start,
		End:         end,
		Description: memo,
		TodomateID:  todo.ID,
		AllDay:      allDay,
		ETag:        etag,
		Owned:       true,
	})
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
		GoogleETag:          updated.ETag,
		GoogleOwned:         true,
	})
}

func canWriteGoogle(ge models.CalendarEvent, m *models.SyncMapping) bool {
	if ge.RecurringEventID != "" || len(ge.Recurrence) > 0 || ge.Status == "cancelled" {
		return false
	}
	if ge.Owned {
		return true
	}
	return m != nil && m.GoogleOwned
}

func (e *Engine) syncGoogleToTodo(ctx context.Context, event models.CalendarEvent, todoByID map[string]models.TodoItem, defaultGoal string) error {
	if event.Status == "cancelled" || event.RecurringEventID != "" || len(event.Recurrence) > 0 {
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
	if m != nil && m.Tombstoned() {
		return nil
	}
	fp := event.Fingerprint()
	onDate := googlecal.EventDate(event)
	span := googlecal.EventSpanDays(event)
	needsMultiDayRoutine := event.AllDay && span > 1

	if m != nil {
		todo, hasTodo := todoByID[m.TodomateID]
		if !hasTodo {
			remote, lookupErr := e.Todo.GetTodoItem(m.TodomateID)
			if lookupErr != nil || remote == nil {
				e.logf("Keep mapping Google %s → Todomate %s (todo lookup is not a reason to recreate: %v)", event.ID, m.TodomateID, lookupErr)
				e.stats["lookup_kept"]++
				return nil
			}
			todo = *remote
			hasTodo = true
		}
		if m.GoogleFingerprint == fp {
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
		if needsMultiDayRoutine && hasTodo && todo.RoutineID == "" {
			e.logf("Google %s spans %d days; updating Todomate %s in place (not rebuilding)", event.ID, span, m.TodomateID)
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
			updated, err := e.Todo.UpdateTodoItem(m.TodomateID, &summary, &onDate, &desc)
			if err != nil {
				return err
			}
			owned := event.Owned || m.GoogleOwned
			etag := event.ETag
			if owned {
				if linked := e.linkGoogleQuietly(ctx, event, updated.ID); linked != "" {
					etag = linked
				}
			}
			return e.Store.Upsert(models.SyncMapping{
				TodomateID:          updated.ID,
				GoogleEventID:       event.ID,
				TodomateFingerprint: updated.Fingerprint(),
				GoogleFingerprint:   fp,
				LastSyncedAt:        time.Now().UTC(),
				LastOrigin:          "google",
				GoogleETag:          etag,
				GoogleOwned:         owned,
			})
		}
		e.logf("Mapped Google %s has no confirmed Todomate item; leaving both sides", event.ID)
		return nil
	}

	if event.TodomateID != "" {
		if todo, ok := todoByID[event.TodomateID]; ok {
			if e.Settings.DryRun {
				return nil
			}
			return e.Store.Upsert(models.SyncMapping{
				TodomateID:          todo.ID,
				GoogleEventID:       event.ID,
				TodomateFingerprint: todo.Fingerprint(),
				GoogleFingerprint:   fp,
				LastSyncedAt:        time.Now().UTC(),
				LastOrigin:          "recover",
				GoogleETag:          event.ETag,
				GoogleOwned:         event.Owned,
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
	if existing := findTodoForGoogleEvent(event, todoByID); existing != nil {
		if prev, err := e.Store.GetByTodomate(existing.ID); err != nil {
			return err
		} else if prev != nil {
			return nil
		}
		e.logf("Recover mapping Google %s → Todomate %s (no rebuild)", event.ID, existing.ID)
		e.stats["recover_mapping"]++
		if e.Settings.DryRun {
			return nil
		}
		etag := event.ETag
		if event.Owned {
			if linked := e.linkGoogleQuietly(ctx, event, existing.ID); linked != "" {
				etag = linked
			}
		} else {
			e.logf("Skip Google link for unowned event %s", event.ID)
			e.stats["google_link_skipped"]++
		}
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          existing.ID,
			GoogleEventID:       event.ID,
			TodomateFingerprint: existing.Fingerprint(),
			GoogleFingerprint:   fp,
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "recover",
			GoogleETag:          etag,
			GoogleOwned:         event.Owned,
		})
	}
	return e.createFromGoogle(ctx, event, defaultGoal)
}

func (e *Engine) createFromGoogle(ctx context.Context, event models.CalendarEvent, goalID string) error {
	onDate := googlecal.EventDate(event)
	span := googlecal.EventSpanDays(event)
	endInclusive := googlecal.EventInclusiveEnd(event)
	title := orUntitled(event.Summary)
	memo := textutil.FromHTML(event.Description)
	fp := event.Fingerprint()

	if event.AllDay && span > 1 {
		e.logf("Create Todomate routine from Google %s (%s) %s~%s (%d days)", event.ID, title, onDate.Format("2006-01-02"), endInclusive.Format("2006-01-02"), span)
		e.stats["create_routine"]++
		e.stats["create_todo"] += span
		if e.Settings.DryRun {
			return nil
		}
		_, first, _, err := e.Todo.CreateRoutineWithDailyTodos(title, goalID, memo, onDate, endInclusive)
		if err != nil {
			return err
		}
		etag := event.ETag
		if event.Owned {
			if linked := e.linkGoogleQuietly(ctx, event, first.ID); linked != "" {
				etag = linked
			}
		}
		return e.Store.Upsert(models.SyncMapping{
			TodomateID:          first.ID,
			GoogleEventID:       event.ID,
			TodomateFingerprint: first.Fingerprint(),
			GoogleFingerprint:   fp,
			LastSyncedAt:        time.Now().UTC(),
			LastOrigin:          "google",
			GoogleETag:          etag,
			GoogleOwned:         event.Owned,
		})
	}

	e.logf("Create Todomate item from Google %s (%s)", event.ID, title)
	e.stats["create_todo"]++
	if e.Settings.DryRun {
		return nil
	}
	created, err := e.Todo.CreateTodoItem(title, goalID, onDate, memo, "")
	if err != nil {
		return err
	}
	etag := event.ETag
	if event.Owned {
		if linked := e.linkGoogleQuietly(ctx, event, created.ID); linked != "" {
			etag = linked
		}
	}
	return e.Store.Upsert(models.SyncMapping{
		TodomateID:          created.ID,
		GoogleEventID:       event.ID,
		TodomateFingerprint: created.Fingerprint(),
		GoogleFingerprint:   fp,
		LastSyncedAt:        time.Now().UTC(),
		LastOrigin:          "google",
		GoogleETag:          etag,
		GoogleOwned:         event.Owned,
	})
}

// linkGoogleQuietly writes the Todomate id onto an event GODA created.
// Unowned events are never patched. The returned etag is empty when the
// write did not happen or failed; the caller keeps the previous link.
func (e *Engine) linkGoogleQuietly(ctx context.Context, event models.CalendarEvent, todomateID string) string {
	if !event.Owned {
		e.logf("Skip Google link for %s (%s); event is not owned by GODA", event.ID, event.Summary)
		e.stats["google_link_skipped"]++
		return ""
	}
	updated, err := e.Google.UpdateEvent(ctx, event.ID, googlecal.EventWrite{
		Summary:     event.Summary,
		Start:       event.Start,
		End:         event.End,
		Description: event.Description,
		TodomateID:  todomateID,
		AllDay:      event.AllDay,
		ETag:        event.ETag,
		Owned:       true,
	})
	if err != nil {
		e.logf("Warn: Google link skipped for %s (%s): %v", event.ID, event.Summary, err)
		e.stats["google_link_skipped"]++
		return ""
	}
	return updated.ETag
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

func findLiveForTodo(todo models.TodoItem, live map[string]models.CalendarEvent, ownedOnly bool) *models.CalendarEvent {
	if todo.Date == nil {
		return nil
	}
	day := seoul.Midnight(*todo.Date)
	var byTitle *models.CalendarEvent
	for _, ev := range live {
		evCopy := ev
		if ev.RecurringEventID != "" || len(ev.Recurrence) > 0 {
			continue
		}
		if ownedOnly != ev.Owned {
			continue
		}
		if ev.TodomateID != "" && ev.TodomateID == todo.ID {
			return &evCopy
		}
		if !sameTitle(ev.Summary, todo.Content) {
			continue
		}
		start := googlecal.EventDate(ev)
		end := googlecal.EventInclusiveEnd(ev)
		if !day.Before(start) && !day.After(end) && byTitle == nil {
			byTitle = &evCopy
		}
	}
	return byTitle
}

func matchesUnownedSeries(todo models.TodoItem, series map[string][]models.CalendarEvent) bool {
	if todo.Date == nil {
		return false
	}
	day := seoul.Midnight(*todo.Date)
	for _, instances := range series {
		for _, ev := range instances {
			if ev.Owned || !sameTitle(ev.Summary, todo.Content) {
				continue
			}
			if googlecal.EventDate(ev).Equal(day) {
				return true
			}
		}
	}
	return false
}

func findTodoForGoogleEvent(event models.CalendarEvent, todoByID map[string]models.TodoItem) *models.TodoItem {
	if event.TodomateID != "" {
		if t, ok := todoByID[event.TodomateID]; ok {
			cp := t
			return &cp
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
		day := seoul.Midnight(*t.Date)
		if day.Before(start) || day.After(end) {
			continue
		}
		cp := t
		if day.Equal(start) {
			return &cp
		}
		if match == nil {
			match = &cp
		}
	}
	return match
}

func SummaryLine(stats map[string]int, dryRun bool) string {
	return fmt.Sprintf("dry_run=%v create_google=%d", dryRun, stats["create_google"])
}

var (
	_ CalendarClient = (*googlecal.Client)(nil)
	_ TodoClient     = (*todomate.Client)(nil)
)
