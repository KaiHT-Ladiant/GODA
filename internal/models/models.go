package models

import (
	"fmt"
	"time"
)

type Goal struct {
	ID         string
	UserID     string
	Title      string
	CreateTime int64
	Priority   int
	Color      string
	IsPublic   bool
	FinishType any
	CrewID     string
}

func (g Goal) IsActive() bool {
	return g.FinishType == nil
}

type TodoItem struct {
	ID         string
	WriterID   string
	GoalID     string
	Content    string
	Date       *time.Time // date-only (UTC midnight of calendar day)
	CreateTime int64
	IsDone     bool
	RemindAt   *int64
	DoneTime   *int64
	Memo       string
	RoutineID  string
	UpdatedAt  time.Time
}

func (t TodoItem) Fingerprint() string {
	dateStr := ""
	if t.Date != nil {
		dateStr = t.Date.Format("2006-01-02")
	}
	done := "0"
	if t.IsDone {
		done = "1"
	}
	remind := ""
	if t.RemindAt != nil {
		remind = fmt.Sprintf("%d", *t.RemindAt)
	}
	return stringsJoin([]string{t.Content, dateStr, t.GoalID, done, t.Memo, remind}, "|")
}

type CalendarEvent struct {
	ID          string
	Summary     string
	Start       time.Time
	End         time.Time
	Updated     time.Time
	Status      string
	Description string
	AllDay      bool
	ETag        string
	TodomateID  string
	OriginSync  bool
}

func (e CalendarEvent) Fingerprint() string {
	start := e.Start.Format(time.RFC3339)
	if e.AllDay {
		start = e.Start.Format("2006-01-02")
	}
	allDay := "0"
	if e.AllDay {
		allDay = "1"
	}
	return stringsJoin([]string{e.Summary, start, allDay, e.Description, e.Status}, "|")
}

type SyncMapping struct {
	TodomateID          string
	GoogleEventID       string
	TodomateFingerprint string
	GoogleFingerprint   string
	LastSyncedAt        time.Time
	LastOrigin          string
}

func stringsJoin(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
