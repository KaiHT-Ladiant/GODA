package todomate

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/google-to-domate/internal/seoul"
)

func TestParseDateAsiaSeoul(t *testing.T) {
	// 2026-10-14 00:00 in Seoul is 2026-10-13 15:00 UTC. A UTC calendar-day
	// split would report the 13th.
	ms := time.Date(2026, 10, 14, 0, 0, 0, 0, seoul.Location).UnixMilli()
	got := parseDate(ms)
	if got == nil || seoul.DateString(*got) != "2026-10-14" {
		t.Fatalf("parseDate(%d) = %v", ms, got)
	}
	if got.Location().String() != seoul.Location.String() {
		t.Fatalf("location = %s", got.Location())
	}

	written := dateMillis(*got)
	again := parseDate(written)
	if again == nil || !again.Equal(*got) {
		t.Fatalf("round trip %v -> %v", got, again)
	}
}

func TestGetTodoItemDoesNotHideForbiddenOrNotFound(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{status: http.StatusForbidden, want: ErrForbidden},
		{status: http.StatusNotFound, want: ErrNotFound},
		{status: http.StatusBadGateway, want: nil},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", tc.status)
		}))
		c := NewClient("key", "proj", Session{IDToken: "tok"})
		c.baseURL = srv.URL
		_, err := c.GetTodoItem("abc")
		srv.Close()
		if tc.want != nil {
			if err != tc.want {
				t.Fatalf("status %d err = %v", tc.status, err)
			}
			continue
		}
		if err == nil || err == ErrNotFound || err == ErrForbidden {
			t.Fatalf("status %d err = %v", tc.status, err)
		}
	}
}

func TestGetTodoItemSuccess(t *testing.T) {
	ms := time.Date(2026, 10, 14, 0, 0, 0, 0, seoul.Location).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %s", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{
			"name": "projects/p/databases/(default)/documents/TodoItem/abc",
			"fields": {
				"id": {"stringValue": "abc"},
				"content": {"stringValue": "OSCP"},
				"goalID": {"stringValue": "g"},
				"date": {"integerValue": "`+fmt.Sprintf("%d", ms)+`"},
				"isDone": {"booleanValue": false}
			}
		}`)
	}))
	defer srv.Close()
	c := NewClient("key", "proj", Session{IDToken: "tok"})
	c.baseURL = srv.URL
	item, err := c.GetTodoItem("abc")
	if err != nil || item == nil {
		t.Fatal(err)
	}
	if item.Date == nil || seoul.DateString(*item.Date) != "2026-10-14" {
		t.Fatalf("date = %v", item.Date)
	}
}
