package googlecal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/local/google-to-domate/internal/models"
	"github.com/local/google-to-domate/internal/seoul"
)

const (
	// PrivatePropTodoID links a GODA-created event back to a Todomate id.
	PrivatePropTodoID = "g2d_todomate_id"
	// PrivatePropOrigin is historical. It is NOT ownership: older builds
	// stamped it onto the user's own events while linking.
	PrivatePropOrigin = "g2d_origin"
	// PrivatePropOwned is set only by CreateEvent. Updates preserve it on
	// events that already have it and never add it to a user's event.
	PrivatePropOwned = "g2d_owned"
	OriginSync       = "sync"
	OwnedYes         = "1"
	baseURL          = "https://www.googleapis.com/calendar/v3"
)

// HTTPError is a non-2xx response from the Calendar API.
type HTTPError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// EventWrite is the user-facing payload for create/update.
// Owned must be true only for events GODA itself created.
type EventWrite struct {
	Summary     string
	Start       time.Time
	End         time.Time
	Description string
	TodomateID  string
	AllDay      bool
	ETag        string
	Owned       bool
}

type Client struct {
	HTTP       *http.Client
	CalendarID string
	// BaseURL overrides the Calendar API host in tests.
	BaseURL string
}

func New(httpClient *http.Client, calendarID string) *Client {
	if calendarID == "" {
		calendarID = "primary"
	}
	return &Client{HTTP: httpClient, CalendarID: calendarID}
}

func (c *Client) ListEvents(ctx context.Context, timeMin, timeMax time.Time) ([]models.CalendarEvent, string, error) {
	var out []models.CalendarEvent
	var pageToken, syncToken string
	for {
		q := url.Values{}
		// singleEvents expands series so the window includes instances whose
		// master started years ago. The sync engine must not PATCH or DELETE
		// those instance ids; it groups them by recurringEventId.
		q.Set("singleEvents", "true")
		q.Set("showDeleted", "true")
		q.Set("maxResults", "250")
		q.Set("timeMin", timeMin.In(seoul.Location).Format(time.RFC3339))
		q.Set("timeMax", timeMax.In(seoul.Location).Format(time.RFC3339))
		q.Set("orderBy", "startTime")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			Items         []map[string]any `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
			NextSyncToken string           `json:"nextSyncToken"`
		}
		endpoint := fmt.Sprintf("%s/calendars/%s/events?%s", c.apiBase(), url.PathEscape(c.CalendarID), q.Encode())
		if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &res, nil); err != nil {
			return nil, "", err
		}
		for _, item := range res.Items {
			out = append(out, parseEvent(item))
		}
		if res.NextSyncToken != "" {
			syncToken = res.NextSyncToken
		}
		pageToken = res.NextPageToken
		if pageToken == "" {
			break
		}
	}
	// nextSyncToken is returned for callers that want to record it, but the
	// engine keeps doing a windowed list. Mixing a sync token with timeMin
	// is rejected by the API, and a token-only list would hide the ownership
	// and recurrence checks this client now depends on.
	return out, syncToken, nil
}

func (c *Client) GetEvent(ctx context.Context, eventID string) (models.CalendarEvent, error) {
	var raw map[string]any
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s", c.apiBase(), url.PathEscape(c.CalendarID), url.PathEscape(eventID))
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &raw, nil); err != nil {
		return models.CalendarEvent{}, err
	}
	return parseEvent(raw), nil
}

// CreateEvent inserts a single event and marks it g2d_owned=1.
// Recurring series are never created here.
func (c *Client) CreateEvent(ctx context.Context, w EventWrite) (models.CalendarEvent, error) {
	w.Owned = true
	body := eventBody(w)
	var raw map[string]any
	endpoint := fmt.Sprintf("%s/calendars/%s/events", c.apiBase(), url.PathEscape(c.CalendarID))
	if err := c.doJSON(ctx, http.MethodPost, endpoint, body, &raw, nil); err != nil {
		return models.CalendarEvent{}, err
	}
	return parseEvent(raw), nil
}

// UpdateEvent patches an event. Caller must pass Owned=true only when the
// event already carries the ownership marker; this method will not add it.
// A non-empty ETag is sent as If-Match.
func (c *Client) UpdateEvent(ctx context.Context, eventID string, w EventWrite) (models.CalendarEvent, error) {
	body := eventBody(w)
	var raw map[string]any
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s", c.apiBase(), url.PathEscape(c.CalendarID), url.PathEscape(eventID))
	headers := map[string]string{}
	if w.ETag != "" {
		headers["If-Match"] = w.ETag
	}
	if err := c.doJSON(ctx, http.MethodPatch, endpoint, body, &raw, headers); err != nil {
		return models.CalendarEvent{}, err
	}
	return parseEvent(raw), nil
}

// DeleteEvent removes an event. A non-empty etag is sent as If-Match.
// The sync engine must not call this for events GODA did not create.
func (c *Client) DeleteEvent(ctx context.Context, eventID, etag string) error {
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s", c.apiBase(), url.PathEscape(c.CalendarID), url.PathEscape(eventID))
	headers := map[string]string{}
	if etag != "" {
		headers["If-Match"] = etag
	}
	return c.doJSON(ctx, http.MethodDelete, endpoint, nil, nil, headers)
}

func (c *Client) apiBase() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return baseURL
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, payload, out any, headers map[string]string) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return &HTTPError{Method: method, URL: endpoint, Status: res.StatusCode, Body: string(b)}
	}
	if out == nil || len(b) == 0 || method == http.MethodDelete {
		return nil
	}
	return json.Unmarshal(b, out)
}

func parseEvent(item map[string]any) models.CalendarEvent {
	ev := models.CalendarEvent{
		ID:               asString(item["id"]),
		Summary:          asString(item["summary"]),
		Status:           asString(item["status"]),
		Description:      asString(item["description"]),
		ETag:             asString(item["etag"]),
		RecurringEventID: asString(item["recurringEventId"]),
	}
	if raw, ok := item["recurrence"].([]any); ok {
		for _, r := range raw {
			if s := asString(r); s != "" {
				ev.Recurrence = append(ev.Recurrence, s)
			}
		}
	}
	if u := asString(item["updated"]); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			ev.Updated = t
		}
	}
	if start, ok := item["start"].(map[string]any); ok {
		ev.Start, ev.AllDay = parseWhen(start)
	}
	if end, ok := item["end"].(map[string]any); ok {
		ev.End, _ = parseWhen(end)
	}
	if ext, ok := item["extendedProperties"].(map[string]any); ok {
		if priv, ok := ext["private"].(map[string]any); ok {
			ev.TodomateID = asString(priv[PrivatePropTodoID])
			ev.OriginSync = asString(priv[PrivatePropOrigin]) == OriginSync
			ev.Owned = asString(priv[PrivatePropOwned]) == OwnedYes
		}
	}
	return ev
}

func parseWhen(node map[string]any) (time.Time, bool) {
	if d := asString(node["date"]); d != "" {
		if t, err := time.ParseInLocation("2006-01-02", d, seoul.Location); err == nil {
			return t, true
		}
	}
	if dt := asString(node["dateTime"]); dt != "" {
		if t, err := time.Parse(time.RFC3339, dt); err == nil {
			return t.In(seoul.Location), false
		}
	}
	return time.Time{}, false
}

func eventBody(w EventWrite) map[string]any {
	priv := map[string]string{}
	if w.Owned {
		priv[PrivatePropOwned] = OwnedYes
		priv[PrivatePropOrigin] = OriginSync
	}
	if w.TodomateID != "" {
		priv[PrivatePropTodoID] = w.TodomateID
	}
	body := map[string]any{
		"summary": w.Summary,
	}
	if len(priv) > 0 {
		body["extendedProperties"] = map[string]any{"private": priv}
	}
	if w.Description != "" {
		body["description"] = w.Description
	}
	if w.AllDay {
		startDate := seoul.DateString(w.Start)
		endDate := seoul.Midnight(w.Start).AddDate(0, 0, 1).Format("2006-01-02")
		if !w.End.IsZero() {
			ed := seoul.DateString(w.End)
			if ed > startDate {
				endDate = ed
			}
		}
		body["start"] = map[string]string{"date": startDate}
		body["end"] = map[string]string{"date": endDate}
		return body
	}
	start := w.Start
	end := w.End
	if end.IsZero() {
		end = start.Add(time.Hour)
	}
	body["start"] = map[string]string{
		"dateTime": start.In(seoul.Location).Format(time.RFC3339),
		"timeZone": "Asia/Seoul",
	}
	body["end"] = map[string]string{
		"dateTime": end.In(seoul.Location).Format(time.RFC3339),
		"timeZone": "Asia/Seoul",
	}
	return body
}

// EventDate is the Asia/Seoul calendar day of the event start.
func EventDate(ev models.CalendarEvent) time.Time {
	return seoul.Midnight(ev.Start)
}

// EventInclusiveEnd returns the last calendar day included in the event.
// Google all-day ends are exclusive, so the inclusive end is End-1 day.
func EventInclusiveEnd(ev models.CalendarEvent) time.Time {
	start := EventDate(ev)
	if ev.End.IsZero() {
		return start
	}
	endDay := seoul.Midnight(ev.End)
	if ev.AllDay {
		inclusive := endDay.AddDate(0, 0, -1)
		if inclusive.Before(start) {
			return start
		}
		return inclusive
	}
	if endDay.Before(start) {
		return start
	}
	return endDay
}

// EventSpanDays is the number of calendar days covered (inclusive).
func EventSpanDays(ev models.CalendarEvent) int {
	start := EventDate(ev)
	end := EventInclusiveEnd(ev)
	days := int(end.Sub(start).Hours()/24) + 1
	if days < 1 {
		return 1
	}
	return days
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
