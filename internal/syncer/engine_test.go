package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/local/google-to-domate/internal/config"
	"github.com/local/google-to-domate/internal/googlecal"
	"github.com/local/google-to-domate/internal/models"
	"github.com/local/google-to-domate/internal/seoul"
	"github.com/local/google-to-domate/internal/store"
	"github.com/local/google-to-domate/internal/todomate"
)

type writeCall struct {
	ID string
	W  googlecal.EventWrite
}

type fakeGoogle struct {
	events    []models.CalendarEvent
	masters   map[string]models.CalendarEvent
	gets      []string
	creates   []googlecal.EventWrite
	updates   []writeCall
	deletes   []string
	updateErr error
}

func (f *fakeGoogle) ListEvents(context.Context, time.Time, time.Time) ([]models.CalendarEvent, string, error) {
	return f.events, "unused-token", nil
}

func (f *fakeGoogle) GetEvent(_ context.Context, eventID string) (models.CalendarEvent, error) {
	f.gets = append(f.gets, eventID)
	if m, ok := f.masters[eventID]; ok {
		return m, nil
	}
	return models.CalendarEvent{}, errors.New("master missing")
}

func (f *fakeGoogle) CreateEvent(_ context.Context, w googlecal.EventWrite) (models.CalendarEvent, error) {
	f.creates = append(f.creates, w)
	return models.CalendarEvent{
		ID:         "created",
		Summary:    w.Summary,
		Start:      w.Start,
		End:        w.End,
		AllDay:     w.AllDay,
		Owned:      true,
		ETag:       "\"created\"",
		TodomateID: w.TodomateID,
		Status:     "confirmed",
	}, nil
}

func (f *fakeGoogle) UpdateEvent(_ context.Context, eventID string, w googlecal.EventWrite) (models.CalendarEvent, error) {
	f.updates = append(f.updates, writeCall{ID: eventID, W: w})
	if f.updateErr != nil {
		return models.CalendarEvent{}, f.updateErr
	}
	return models.CalendarEvent{
		ID: eventID, Summary: w.Summary, Start: w.Start, End: w.End, AllDay: w.AllDay,
		Owned: w.Owned, ETag: "\"updated\"", Description: w.Description, Status: "confirmed",
		TodomateID: w.TodomateID,
	}, nil
}

func (f *fakeGoogle) DeleteEvent(_ context.Context, eventID, _ string) error {
	f.deletes = append(f.deletes, eventID)
	return nil
}

type seriesCall struct {
	Title       string
	Repeat      int
	DaysOfMonth []int
	OnDate      time.Time
}

type fakeTodo struct {
	goals   []models.Goal
	items   []models.TodoItem
	getErr  error
	delErr  error
	deleted []string
	created []models.TodoItem
	series  []seriesCall
	updated int
}

func (f *fakeTodo) ListActiveGoals([]string, []string) ([]models.Goal, error) {
	return f.goals, nil
}

func (f *fakeTodo) ListTodoItems(*time.Time, *time.Time) ([]models.TodoItem, error) {
	return f.items, nil
}

func (f *fakeTodo) GetTodoItem(id string) (*models.TodoItem, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	for i := range f.items {
		if f.items[i].ID == id {
			cp := f.items[i]
			return &cp, nil
		}
	}
	return nil, todomate.ErrNotFound
}

func (f *fakeTodo) CreateTodoItem(content, goalID string, onDate time.Time, memo, routineID string) (models.TodoItem, error) {
	item := models.TodoItem{
		ID: "todo-new", Content: content, GoalID: goalID, Date: &onDate, Memo: memo, RoutineID: routineID,
		UpdatedAt: time.Now().UTC(),
	}
	f.created = append(f.created, item)
	return item, nil
}

func (f *fakeTodo) UpdateTodoItem(id string, content *string, onDate *time.Time, memo *string) (models.TodoItem, error) {
	f.updated++
	for i := range f.items {
		if f.items[i].ID != id {
			continue
		}
		if content != nil {
			f.items[i].Content = *content
		}
		if onDate != nil {
			d := *onDate
			f.items[i].Date = &d
		}
		if memo != nil {
			f.items[i].Memo = *memo
		}
		return f.items[i], nil
	}
	return models.TodoItem{ID: id}, nil
}

func (f *fakeTodo) DeleteTodoItem(id string) error {
	f.deleted = append(f.deleted, id)
	if f.delErr != nil {
		return f.delErr
	}
	return nil
}

func (f *fakeTodo) DeleteRoutineAndTodos(id string) error {
	f.deleted = append(f.deleted, "routine:"+id)
	return f.delErr
}

func (f *fakeTodo) CreateRoutineWithDailyTodos(title, goalID, memo string, start, _ time.Time) (todomate.Routine, models.TodoItem, int, error) {
	item, err := f.CreateTodoItem(title, goalID, start, memo, "routine")
	return todomate.Routine{ID: "routine"}, item, 1, err
}

func (f *fakeTodo) CreateSeriesRoutine(title, goalID, memo string, onDate time.Time, repeatType int, daysOfMonth []int) (todomate.Routine, models.TodoItem, error) {
	f.series = append(f.series, seriesCall{Title: title, Repeat: repeatType, DaysOfMonth: append([]int(nil), daysOfMonth...), OnDate: onDate})
	item, err := f.CreateTodoItem(title, goalID, onDate, memo, "series")
	return todomate.Routine{ID: "series", RepeatType: repeatType}, item, err
}

func testSettings() *config.Settings {
	return &config.Settings{
		LookbackDays:       14,
		LookaheadDays:      60,
		DryRun:             false,
		SkipCompletedTodos: true,
		PreferGoogleOnTie:  true,
		MaxDeletionsPerRun: 3,
		HardDelete:         false,
	}
}

func newTestEngine(t *testing.T, g *fakeGoogle, td *fakeTodo, settings *config.Settings) (*Engine, *store.Store) {
	t.Helper()
	if settings == nil {
		settings = testSettings()
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Engine{
		Settings: settings,
		Google:   g,
		Todo:     td,
		Store:    st,
		Logf:     func(string, ...any) {},
	}, st
}

func goal() models.Goal {
	return models.Goal{ID: "goal-oscp", Title: "OSCP", Priority: 1}
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, seoul.Location)
}

func mustUpsert(t *testing.T, st *store.Store, m models.SyncMapping) {
	t.Helper()
	if err := st.Upsert(m); err != nil {
		t.Fatal(err)
	}
}

func TestRecurringInstanceIsNotSplitOrDeleted(t *testing.T) {
	const (
		masterID   = "rent-series"
		instanceID = "rent-series_20261014"
		title      = "월세 지급 💲"
	)
	start := day(2026, 10, 14)
	end := day(2026, 10, 15)
	instance := models.CalendarEvent{
		ID:               instanceID,
		RecurringEventID: masterID,
		Summary:          title,
		Status:           "cancelled",
		Start:            start,
		End:              end,
		AllDay:           true,
		ETag:             "\"inst\"",
	}
	confirmed := instance
	confirmed.ID = masterID + "_20261114"
	confirmed.Status = "confirmed"
	confirmed.Start = day(2026, 11, 14)
	confirmed.End = day(2026, 11, 15)
	master := models.CalendarEvent{
		ID:         masterID,
		Summary:    title,
		Status:     "confirmed",
		Start:      day(2024, 1, 14),
		End:        day(2024, 1, 15),
		AllDay:     true,
		Recurrence: []string{"RRULE:FREQ=MONTHLY;BYMONTHDAY=14"},
		ETag:       "\"master\"",
	}
	todoDate := start
	study := models.TodoItem{
		ID: "oscp-1", GoalID: goal().ID, Content: "OSCP study", Date: &todoDate, UpdatedAt: time.Now().UTC(),
	}
	rentTodo := models.TodoItem{
		ID: "rent-todo", GoalID: goal().ID, Content: title, Date: &todoDate, UpdatedAt: time.Now().UTC(),
	}

	t.Run("mapped instance is left alone", func(t *testing.T) {
		g := &fakeGoogle{events: []models.CalendarEvent{instance, confirmed}, masters: map[string]models.CalendarEvent{masterID: master}}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{study, rentTodo}}
		settings := testSettings()
		settings.ImportUnmappedGoogleEvents = true
		settings.DefaultGoalID = goal().ID
		eng, st := newTestEngine(t, g, td, settings)
		mustUpsert(t, st, models.SyncMapping{
			TodomateID: study.ID, GoogleEventID: instanceID, GoogleOwned: false,
			TodomateFingerprint: "old", GoogleFingerprint: "old",
		})
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(g.updates) != 0 || len(g.deletes) != 0 || len(g.creates) != 0 {
			t.Fatalf("google writes updates=%d deletes=%d creates=%d", len(g.updates), len(g.deletes), len(g.creates))
		}
		if len(td.deleted) != 0 || len(td.series) != 0 {
			t.Fatalf("todomate deleted=%v series=%d", td.deleted, len(td.series))
		}
		got, err := st.GetByGoogle(instanceID)
		if err != nil || got == nil || got.Tombstoned() || got.TodomateID != study.ID {
			t.Fatalf("instance mapping = %+v err=%v", got, err)
		}
		for _, id := range g.gets {
			if id == instanceID {
				t.Fatalf("fetched instance %s instead of the master", id)
			}
		}
	})

	t.Run("import maps the series once and does not touch google", func(t *testing.T) {
		g := &fakeGoogle{events: []models.CalendarEvent{confirmed}, masters: map[string]models.CalendarEvent{masterID: master}}
		td := &fakeTodo{goals: []models.Goal{goal()}}
		settings := testSettings()
		settings.ImportUnmappedGoogleEvents = true
		settings.DefaultGoalID = goal().ID
		eng, _ := newTestEngine(t, g, td, settings)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(g.updates) != 0 || len(g.deletes) != 0 || len(g.creates) != 0 {
			t.Fatalf("google mutated: updates=%d deletes=%d creates=%d", len(g.updates), len(g.deletes), len(g.creates))
		}
		if len(td.series) != 1 {
			t.Fatalf("series imports = %d", len(td.series))
		}
		if td.series[0].Repeat != todomate.RoutineRepeatMonthly || len(td.series[0].DaysOfMonth) != 1 || td.series[0].DaysOfMonth[0] != 14 {
			t.Fatalf("series shape = %+v", td.series[0])
		}
		if seoul.DateString(td.series[0].OnDate) != "2026-11-14" {
			t.Fatalf("series date = %s", seoul.DateString(td.series[0].OnDate))
		}
	})

	t.Run("existing same title is not rebuilt", func(t *testing.T) {
		matched := rentTodo
		on := confirmed.Start
		matched.Date = &on
		g := &fakeGoogle{events: []models.CalendarEvent{confirmed}, masters: map[string]models.CalendarEvent{masterID: master}}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{matched}}
		settings := testSettings()
		settings.ImportUnmappedGoogleEvents = true
		settings.DefaultGoalID = goal().ID
		eng, _ := newTestEngine(t, g, td, settings)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(td.series) != 0 || len(td.created) != 0 || len(td.deleted) != 0 {
			t.Fatalf("rebuilt series=%d created=%d deleted=%v", len(td.series), len(td.created), td.deleted)
		}
		if len(g.creates) != 0 || len(g.updates) != 0 || len(g.deletes) != 0 {
			t.Fatalf("google writes creates=%d updates=%d deletes=%d", len(g.creates), len(g.updates), len(g.deletes))
		}
	})
}

func TestCompletedTodoDoesNotDeleteCalendarEvent(t *testing.T) {
	when := day(2026, 10, 14)
	item := models.TodoItem{ID: "done-1", GoalID: goal().ID, Content: "OSCP study", Date: &when, IsDone: true, UpdatedAt: time.Now().UTC()}
	g := &fakeGoogle{events: []models.CalendarEvent{{
		ID: "owned-ev", Summary: "OSCP study", Start: when, End: when.AddDate(0, 0, 1), AllDay: true, Owned: true, Status: "confirmed",
	}}}
	td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{item}}
	eng, st := newTestEngine(t, g, td, nil)
	mustUpsert(t, st, models.SyncMapping{
		TodomateID: item.ID, GoogleEventID: "owned-ev", GoogleOwned: true,
		GoogleFingerprint:   g.events[0].Fingerprint(),
		TodomateFingerprint: item.Fingerprint(),
	})
	if err := eng.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.deletes) != 0 || len(g.updates) != 0 || len(td.deleted) != 0 {
		t.Fatalf("destructive calls googleDelete=%v googleUpdate=%d todoDelete=%v", g.deletes, len(g.updates), td.deleted)
	}
	got, err := st.GetByTodomate(item.ID)
	if err != nil || got == nil || got.Tombstoned() {
		t.Fatalf("mapping = %+v err=%v", got, err)
	}
	if eng.stats["completed_kept"] != 1 {
		t.Fatalf("completed_kept = %d", eng.stats["completed_kept"])
	}
}

func TestLookupErrorsAreNotDeletions(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "403", err: todomate.ErrForbidden},
		{name: "404", err: todomate.ErrNotFound},
		{name: "transient", err: errors.New("dial tcp: timeout")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGoogle{}
			td := &fakeTodo{goals: []models.Goal{goal()}, getErr: tc.err}
			eng, st := newTestEngine(t, g, td, nil)
			mustUpsert(t, st, models.SyncMapping{
				TodomateID: "missing", GoogleEventID: "ev-1", GoogleOwned: true,
			})
			if err := eng.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(g.deletes) != 0 || len(td.deleted) != 0 {
				t.Fatalf("deleted google=%v todo=%v", g.deletes, td.deleted)
			}
			got, err := st.GetByTodomate("missing")
			if err != nil || got == nil || got.Tombstoned() {
				t.Fatalf("mapping = %+v err=%v", got, err)
			}
			if eng.stats["lookup_kept"] != 1 {
				t.Fatalf("lookup_kept = %d", eng.stats["lookup_kept"])
			}
		})
	}
}

func TestDeletionThreshold(t *testing.T) {
	cancelled := func(n int) ([]models.CalendarEvent, []models.TodoItem) {
		var events []models.CalendarEvent
		var items []models.TodoItem
		for i := 0; i < n; i++ {
			id := string(rune('a' + i))
			when := day(2026, 10, 10+i)
			events = append(events, models.CalendarEvent{
				ID: "g-" + id, Status: "cancelled", Owned: true, Summary: id, Start: when, AllDay: true,
			})
			items = append(items, models.TodoItem{ID: "t-" + id, GoalID: goal().ID, Content: id, Date: &when})
		}
		return events, items
	}
	seed := func(t *testing.T, st *store.Store, items []models.TodoItem) {
		t.Helper()
		for _, item := range items {
			mustUpsert(t, st, models.SyncMapping{
				TodomateID: item.ID, GoogleEventID: "g-" + item.Content, GoogleOwned: true,
			})
		}
	}

	t.Run("aborts before any tombstone or delete", func(t *testing.T) {
		events, items := cancelled(4)
		g := &fakeGoogle{events: events}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: items}
		settings := testSettings()
		settings.MaxDeletionsPerRun = 3
		eng, st := newTestEngine(t, g, td, settings)
		seed(t, st, items)
		err := eng.RunOnce(context.Background())
		var thresh *DeletionThresholdError
		if !errors.As(err, &thresh) {
			t.Fatalf("err = %v", err)
		}
		if thresh.Proposed != 4 || thresh.Threshold != 3 {
			t.Fatalf("threshold = %+v", thresh)
		}
		if len(g.deletes) != 0 || len(td.deleted) != 0 {
			t.Fatalf("applied deletes google=%v todo=%v", g.deletes, td.deleted)
		}
		for _, item := range items {
			got, err := st.GetByTodomate(item.ID)
			if err != nil || got == nil || got.Tombstoned() {
				t.Fatalf("mapping %s = %+v err=%v", item.ID, got, err)
			}
		}
	})

	t.Run("under threshold tombstones without remote delete", func(t *testing.T) {
		events, items := cancelled(2)
		g := &fakeGoogle{events: events}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: items}
		eng, st := newTestEngine(t, g, td, nil)
		seed(t, st, items)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(g.deletes) != 0 || len(td.deleted) != 0 {
			t.Fatalf("remote deletes google=%v todo=%v", g.deletes, td.deleted)
		}
		for _, item := range items {
			got, err := st.GetByTodomate(item.ID)
			if err != nil || got == nil || !got.Tombstoned() || got.TombstoneReason != "google_cancelled" {
				t.Fatalf("mapping %s = %+v err=%v", item.ID, got, err)
			}
		}
	})

	t.Run("hard delete error keeps the link", func(t *testing.T) {
		events, items := cancelled(1)
		g := &fakeGoogle{events: events}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: items, delErr: errors.New("firestore 500")}
		settings := testSettings()
		settings.HardDelete = true
		eng, st := newTestEngine(t, g, td, settings)
		seed(t, st, items)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(td.deleted) != 1 {
			t.Fatalf("delete attempts = %v", td.deleted)
		}
		got, err := st.GetByTodomate(items[0].ID)
		if err != nil || got == nil || got.Tombstoned() {
			t.Fatalf("mapping = %+v err=%v", got, err)
		}
		if eng.stats["delete_failed"] != 1 {
			t.Fatalf("delete_failed = %d", eng.stats["delete_failed"])
		}
	})

	t.Run("unowned cancellation is not a deletion", func(t *testing.T) {
		when := day(2026, 10, 14)
		g := &fakeGoogle{events: []models.CalendarEvent{{
			ID: "user-ev", Status: "cancelled", Owned: false, OriginSync: true, Summary: "월세 지급 💲", Start: when,
		}}}
		item := models.TodoItem{ID: "keep", GoalID: goal().ID, Content: "월세 지급 💲", Date: &when}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{item}}
		eng, st := newTestEngine(t, g, td, nil)
		mustUpsert(t, st, models.SyncMapping{TodomateID: item.ID, GoogleEventID: "user-ev", GoogleOwned: false})
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetByTodomate(item.ID)
		if err != nil || got == nil || got.Tombstoned() {
			t.Fatalf("mapping = %+v err=%v", got, err)
		}
		if len(td.deleted) != 0 || len(g.deletes) != 0 {
			t.Fatalf("deleted todo=%v google=%v", td.deleted, g.deletes)
		}
	})

	t.Run("dry run records nothing", func(t *testing.T) {
		events, items := cancelled(2)
		g := &fakeGoogle{events: events}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: items}
		settings := testSettings()
		settings.DryRun = true
		eng, st := newTestEngine(t, g, td, settings)
		seed(t, st, items)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			got, err := st.GetByTodomate(item.ID)
			if err != nil || got == nil || got.Tombstoned() {
				t.Fatalf("dry-run wrote mapping %s = %+v err=%v", item.ID, got, err)
			}
		}
		if len(g.deletes) != 0 || len(td.deleted) != 0 {
			t.Fatalf("dry-run deletes google=%v todo=%v", g.deletes, td.deleted)
		}
	})
}

func TestUnownedEventIsNotPatched(t *testing.T) {
	when := day(2026, 10, 18)
	ev := models.CalendarEvent{
		ID: "user-block", Summary: "OSCP study", Start: when, End: when.AddDate(0, 0, 1),
		AllDay: true, Status: "confirmed", OriginSync: true, Owned: false, ETag: "\"user\"",
	}
	item := models.TodoItem{
		ID: "study", GoalID: goal().ID, Content: "OSCP study - chapter 4", Date: &when, UpdatedAt: time.Now().UTC(),
	}
	g := &fakeGoogle{events: []models.CalendarEvent{ev}}
	td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{item}}
	eng, st := newTestEngine(t, g, td, nil)
	mustUpsert(t, st, models.SyncMapping{
		TodomateID: item.ID, GoogleEventID: ev.ID, GoogleOwned: false,
		GoogleFingerprint: ev.Fingerprint(), TodomateFingerprint: "stale", GoogleETag: ev.ETag,
	})
	if err := eng.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.updates) != 0 || len(g.deletes) != 0 || len(g.creates) != 0 {
		t.Fatalf("google writes updates=%d deletes=%d creates=%d", len(g.updates), len(g.deletes), len(g.creates))
	}
	got, err := st.GetByTodomate(item.ID)
	if err != nil || got == nil || got.GoogleEventID != ev.ID || got.Tombstoned() {
		t.Fatalf("mapping = %+v err=%v", got, err)
	}
}

func TestUpdateUsesETagAndKeepsLinkOnFailure(t *testing.T) {
	when := day(2026, 10, 18)
	ev := models.CalendarEvent{
		ID: "owned-1", Summary: "old", Start: when, End: when.AddDate(0, 0, 1),
		AllDay: true, Status: "confirmed", Owned: true, ETag: "\"live\"",
	}
	item := models.TodoItem{
		ID: "todo-1", GoalID: goal().ID, Content: "new title", Date: &when, UpdatedAt: time.Now().UTC(),
	}
	g := &fakeGoogle{events: []models.CalendarEvent{ev}, updateErr: &googlecal.HTTPError{Status: 412, Body: "precondition"}}
	td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{item}}
	eng, st := newTestEngine(t, g, td, nil)
	mustUpsert(t, st, models.SyncMapping{
		TodomateID: item.ID, GoogleEventID: ev.ID, GoogleOwned: true,
		GoogleFingerprint: ev.Fingerprint(), TodomateFingerprint: "previous", GoogleETag: "\"stored\"",
	})
	err := eng.RunOnce(context.Background())
	var httpErr *googlecal.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 412 {
		t.Fatalf("err = %v", err)
	}
	if len(g.updates) != 1 {
		t.Fatalf("updates = %+v", g.updates)
	}
	if g.updates[0].ID != ev.ID || g.updates[0].W.ETag != "\"live\"" || !g.updates[0].W.Owned {
		t.Fatalf("update = %+v", g.updates[0])
	}
	got, err := st.GetByTodomate(item.ID)
	if err != nil || got == nil || got.TodomateFingerprint != "previous" || got.GoogleEventID != ev.ID {
		t.Fatalf("link changed: %+v err=%v", got, err)
	}
}

func TestAsiaSeoulCalendarDayOnCreateAndImport(t *testing.T) {
	t.Run("todo seoul midnight creates that calendar day", func(t *testing.T) {
		when := day(2026, 10, 14) // 2026-10-13 15:00 UTC
		item := models.TodoItem{ID: "rent", GoalID: goal().ID, Content: "월세 지급 💲", Date: &when, UpdatedAt: time.Now().UTC()}
		g := &fakeGoogle{}
		td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{item}}
		eng, _ := newTestEngine(t, g, td, nil)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(g.creates) != 1 {
			t.Fatalf("creates = %d", len(g.creates))
		}
		if seoul.DateString(g.creates[0].Start) != "2026-10-14" {
			t.Fatalf("create start = %s (utc %s)", seoul.DateString(g.creates[0].Start), g.creates[0].Start.UTC().Format(time.RFC3339))
		}
		if !g.creates[0].AllDay {
			t.Fatal("expected all-day create")
		}
	})

	t.Run("utc instant imports as seoul day", func(t *testing.T) {
		start := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC) // 2026-10-14 00:00 KST
		ev := models.CalendarEvent{
			ID: "timed", Summary: "late night", Start: start, End: start.Add(time.Hour),
			Status: "confirmed", Owned: false,
		}
		g := &fakeGoogle{events: []models.CalendarEvent{ev}}
		td := &fakeTodo{goals: []models.Goal{goal()}}
		settings := testSettings()
		settings.ImportUnmappedGoogleEvents = true
		settings.DefaultGoalID = goal().ID
		eng, _ := newTestEngine(t, g, td, settings)
		if err := eng.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(g.updates) != 0 || len(g.deletes) != 0 {
			t.Fatalf("unowned event was written updates=%d deletes=%d", len(g.updates), len(g.deletes))
		}
		if len(td.created) != 1 {
			t.Fatalf("created = %d", len(td.created))
		}
		if td.created[0].Date == nil || seoul.DateString(*td.created[0].Date) != "2026-10-14" {
			t.Fatalf("imported date = %v", td.created[0].Date)
		}
	})
}

func TestSecondTodoDoesNotDuplicateUnownedEvent(t *testing.T) {
	when := day(2026, 10, 18)
	ev := models.CalendarEvent{
		ID: "user-block", Summary: "OSCP study", Start: when, End: when.AddDate(0, 0, 1),
		AllDay: true, Status: "confirmed", Owned: false,
	}
	first := models.TodoItem{ID: "a", GoalID: goal().ID, Content: "OSCP study", Date: &when, UpdatedAt: time.Now().UTC()}
	second := first
	second.ID = "b"
	g := &fakeGoogle{events: []models.CalendarEvent{ev}}
	td := &fakeTodo{goals: []models.Goal{goal()}, items: []models.TodoItem{first, second}}
	eng, st := newTestEngine(t, g, td, nil)
	if err := eng.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.creates) != 0 || len(g.updates) != 0 || len(g.deletes) != 0 {
		t.Fatalf("google writes creates=%d updates=%d deletes=%d", len(g.creates), len(g.updates), len(g.deletes))
	}
	linked, err := st.GetByGoogle(ev.ID)
	if err != nil || linked == nil || linked.GoogleOwned || linked.TodomateID != first.ID {
		t.Fatalf("link = %+v err=%v", linked, err)
	}
	other, err := st.GetByTodomate(second.ID)
	if err != nil || other != nil {
		t.Fatalf("second mapping = %+v err=%v", other, err)
	}
}

func TestSeriesRepeatMonthlyByMonthDay(t *testing.T) {
	repeat, days := seriesRepeat([]string{"RRULE:FREQ=MONTHLY;BYMONTHDAY=14"}, day(2026, 10, 14))
	if repeat != todomate.RoutineRepeatMonthly || len(days) != 1 || days[0] != 14 {
		t.Fatalf("repeat=%d days=%v", repeat, days)
	}
}
