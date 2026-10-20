package googlecal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/google-to-domate/internal/seoul"
)

func TestParseRecurringInstanceAndOwnership(t *testing.T) {
	ev := parseEvent(map[string]any{
		"id":               "series_20261014",
		"recurringEventId": "series",
		"summary":          "월세 지급 💲",
		"status":           "confirmed",
		"etag":             "\"e\"",
		"start":            map[string]any{"date": "2026-10-14"},
		"end":              map[string]any{"date": "2026-10-15"},
		"extendedProperties": map[string]any{
			"private": map[string]any{
				PrivatePropOrigin: OriginSync,
				PrivatePropTodoID: "todo-1",
			},
		},
	})
	if ev.RecurringEventID != "series" || ev.Owned || !ev.OriginSync || ev.TodomateID != "todo-1" {
		t.Fatalf("instance = %+v", ev)
	}
	if !ev.AllDay || seoul.DateString(EventDate(ev)) != "2026-10-14" || EventSpanDays(ev) != 1 {
		t.Fatalf("date span start=%s days=%d", seoul.DateString(EventDate(ev)), EventSpanDays(ev))
	}

	master := parseEvent(map[string]any{
		"id":         "series",
		"summary":    "월세 지급 💲",
		"recurrence": []any{"RRULE:FREQ=MONTHLY;BYMONTHDAY=14"},
		"extendedProperties": map[string]any{
			"private": map[string]any{PrivatePropOwned: OwnedYes},
		},
	})
	if !master.Owned || len(master.Recurrence) != 1 || master.RecurringEventID != "" {
		t.Fatalf("master = %+v", master)
	}
}

func TestEventDateUsesAsiaSeoul(t *testing.T) {
	ev := parseEvent(map[string]any{
		"id": "timed",
		"start": map[string]any{
			"dateTime": "2026-10-13T15:00:00Z",
		},
		"end": map[string]any{
			"dateTime": "2026-10-13T16:00:00Z",
		},
	})
	if seoul.DateString(EventDate(ev)) != "2026-10-14" {
		t.Fatalf("seoul day = %s (start %s)", seoul.DateString(EventDate(ev)), ev.Start.Format(time.RFC3339))
	}
}

func TestEventBodyUsesAsiaSeoul(t *testing.T) {
	seoulMidnight := time.Date(2026, 10, 14, 0, 0, 0, 0, seoul.Location)
	allDay := eventBody(EventWrite{Summary: "월세 지급 💲", Start: seoulMidnight, AllDay: true, Owned: true, TodomateID: "t1"})
	start := allDay["start"].(map[string]string)
	end := allDay["end"].(map[string]string)
	if start["date"] != "2026-10-14" || end["date"] != "2026-10-15" {
		t.Fatalf("all-day = %v .. %v", start, end)
	}
	priv := allDay["extendedProperties"].(map[string]any)["private"].(map[string]string)
	if priv[PrivatePropOwned] != OwnedYes {
		t.Fatalf("owned marker missing: %v", priv)
	}

	utcEvening := time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)
	timed := eventBody(EventWrite{Summary: "study", Start: utcEvening, AllDay: false})
	got := timed["start"].(map[string]string)
	if got["dateTime"] != "2026-10-14T00:00:00+09:00" || got["timeZone"] != "Asia/Seoul" {
		t.Fatalf("timed start = %v", got)
	}
	if _, ok := timed["extendedProperties"]; ok {
		t.Fatal("unowned write must not stamp extended properties")
	}
}

func TestCreateUpdateDeleteHTTP(t *testing.T) {
	var methods, bodies, matches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		matches = append(matches, r.Header.Get("If-Match"))
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"e1","etag":"\"next\"","summary":"s","status":"confirmed","start":{"date":"2026-10-14"},"end":{"date":"2026-10-15"},"extendedProperties":{"private":{"g2d_owned":"1"}}}`)
	}))
	defer srv.Close()
	c := New(srv.Client(), "primary")
	c.BaseURL = srv.URL

	if _, err := c.CreateEvent(context.Background(), EventWrite{Summary: "s", Start: time.Date(2026, 10, 14, 0, 0, 0, 0, seoul.Location), AllDay: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateEvent(context.Background(), "e1", EventWrite{
		Summary: "s", Start: time.Date(2026, 10, 14, 0, 0, 0, 0, seoul.Location), AllDay: true, ETag: "\"live\"", Owned: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteEvent(context.Background(), "e1", "\"live\""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "POST,PATCH,DELETE" {
		t.Fatalf("methods = %v", methods)
	}
	if !strings.Contains(bodies[0], `"g2d_owned":"1"`) {
		t.Fatalf("create body = %s", bodies[0])
	}
	if strings.Contains(bodies[1], "g2d_owned") {
		t.Fatalf("unowned update stamped ownership: %s", bodies[1])
	}
	if matches[1] != "\"live\"" || matches[2] != "\"live\"" {
		t.Fatalf("If-Match = %v", matches)
	}
}
