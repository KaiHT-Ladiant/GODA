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
)

const (
	PrivatePropTodoID = "g2d_todomate_id"
	PrivatePropOrigin = "g2d_origin"
	OriginSync        = "sync"
	baseURL           = "https://www.googleapis.com/calendar/v3"
)

type Client struct {
	HTTP       *http.Client
	CalendarID string
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
		q.Set("singleEvents", "true")
		q.Set("showDeleted", "true")
		q.Set("maxResults", "250")
		q.Set("timeMin", timeMin.UTC().Format(time.RFC3339))
		q.Set("timeMax", timeMax.UTC().Format(time.RFC3339))
		q.Set("orderBy", "startTime")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			Items         []map[string]any `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
			NextSyncToken string           `json:"nextSyncToken"`
		}
		endpoint := fmt.Sprintf("%s/calendars/%s/events?%s", baseURL, url.PathEscape(c.CalendarID), q.Encode())
		if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &res); err != nil {
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
	return out, syncToken, nil
}

func (c *Client) CreateEvent(ctx context.Context, summary string, start time.Time, description, todomateID string, allDay bool) (models.CalendarEvent, error) {
	body := eventBody(summary, start, time.Time{}, description, todomateID, allDay)
	var raw map[string]any
	endpoint := fmt.Sprintf("%s/calendars/%s/events", baseURL, url.PathEscape(c.CalendarID))
	if err := c.doJSON(ctx, http.MethodPost, endpoint, body, &raw); err != nil {
		return models.CalendarEvent{}, err
	}
	return parseEvent(raw), nil
}

func (c *Client) UpdateEvent(ctx context.Context, eventID, summary string, start, end time.Time, description, todomateID string, allDay bool) (models.CalendarEvent, error) {
	body := eventBody(summary, start, end, description, todomateID, allDay)
	var raw map[string]any
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s", baseURL, url.PathEscape(c.CalendarID), url.PathEscape(eventID))
	if err := c.doJSON(ctx, http.MethodPatch, endpoint, body, &raw); err != nil {
		return models.CalendarEvent{}, err
	}
	return parseEvent(raw), nil
}

func (c *Client) DeleteEvent(ctx context.Context, eventID string) error {
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s", baseURL, url.PathEscape(c.CalendarID), url.PathEscape(eventID))
	return c.doJSON(ctx, http.MethodDelete, endpoint, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, payload, out any) error {
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
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, endpoint, string(b))
	}
	if out == nil || len(b) == 0 || method == http.MethodDelete {
		return nil
	}
	return json.Unmarshal(b, out)
}

func parseEvent(item map[string]any) models.CalendarEvent {
	ev := models.CalendarEvent{
		ID:          asString(item["id"]),
		Summary:     asString(item["summary"]),
		Status:      asString(item["status"]),
		Description: asString(item["description"]),
		ETag:        asString(item["etag"]),
	}
	if u := asString(item["updated"]); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			ev.Updated = t
		}
	}
	if start, ok := item["start"].(map[string]any); ok {
		if d := asString(start["date"]); d != "" {
			ev.AllDay = true
			if t, err := time.Parse("2006-01-02", d); err == nil {
				ev.Start = t
			}
		} else if dt := asString(start["dateTime"]); dt != "" {
			if t, err := time.Parse(time.RFC3339, dt); err == nil {
				ev.Start = t
			}
		}
	}
	if end, ok := item["end"].(map[string]any); ok {
		if d := asString(end["date"]); d != "" {
			if t, err := time.Parse("2006-01-02", d); err == nil {
				ev.End = t
			}
		} else if dt := asString(end["dateTime"]); dt != "" {
			if t, err := time.Parse(time.RFC3339, dt); err == nil {
				ev.End = t
			}
		}
	}
	if ext, ok := item["extendedProperties"].(map[string]any); ok {
		if priv, ok := ext["private"].(map[string]any); ok {
			ev.TodomateID = asString(priv[PrivatePropTodoID])
			ev.OriginSync = asString(priv[PrivatePropOrigin]) == OriginSync
		}
	}
	return ev
}

func eventBody(summary string, start, end time.Time, description, todomateID string, allDay bool) map[string]any {
	priv := map[string]string{PrivatePropOrigin: OriginSync}
	if todomateID != "" {
		priv[PrivatePropTodoID] = todomateID
	}
	body := map[string]any{
		"summary": summary,
		"extendedProperties": map[string]any{
			"private": priv,
		},
	}
	if description != "" {
		body["description"] = description
	}
	if allDay {
		startDate := start.Format("2006-01-02")
		endDate := start.AddDate(0, 0, 1).Format("2006-01-02")
		if !end.IsZero() {
			ed := end.Format("2006-01-02")
			if ed > startDate {
				endDate = ed
			}
		}
		body["start"] = map[string]string{"date": startDate}
		body["end"] = map[string]string{"date": endDate}
		return body
	}
	if end.IsZero() {
		end = start.Add(time.Hour)
	}
	body["start"] = map[string]string{"dateTime": start.Format(time.RFC3339), "timeZone": "Asia/Seoul"}
	body["end"] = map[string]string{"dateTime": end.Format(time.RFC3339), "timeZone": "Asia/Seoul"}
	return body
}

func EventDate(ev models.CalendarEvent) time.Time {
	y, m, d := ev.Start.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// EventInclusiveEnd returns the last calendar day included in the event.
// Google all-day ends are exclusive, so the inclusive end is End-1 day.
func EventInclusiveEnd(ev models.CalendarEvent) time.Time {
	start := EventDate(ev)
	if ev.End.IsZero() {
		return start
	}
	if ev.AllDay {
		y, m, d := ev.End.Date()
		endExclusive := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		inclusive := endExclusive.AddDate(0, 0, -1)
		if inclusive.Before(start) {
			return start
		}
		return inclusive
	}
	y, m, d := ev.End.In(time.UTC).Date()
	endDay := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
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
