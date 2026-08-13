package todomate

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/local/google-to-domate/internal/models"
)

type Session struct {
	Email        string  `json:"email"`
	LocalID      string  `json:"local_id"`
	IDToken      string  `json:"id_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresAt    float64 `json:"expires_at"`
}

func (s Session) Expired() bool {
	return time.Now().Unix() >= int64(s.ExpiresAt)-60
}

type Client struct {
	http      *http.Client
	APIKey    string
	ProjectID string
	Session   Session
}

func LoadSession(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func SaveSession(path string, s Session) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func HasSession(path string) bool {
	s, err := LoadSession(path)
	return err == nil && s != nil && !s.Expired()
}

func SignIn(apiKey, email, password string) (Session, error) {
	payload := map[string]any{
		"email":             email,
		"password":          password,
		"returnSecureToken": true,
	}
	var resp struct {
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		LocalID      string `json:"localId"`
		ExpiresIn    string `json:"expiresIn"`
	}
	if err := postJSON(fmt.Sprintf("https://identitytoolkit.googleapis.com/v1/accounts:signInWithPassword?key=%s", url.QueryEscape(apiKey)), payload, &resp); err != nil {
		return Session{}, err
	}
	exp := 3600
	fmt.Sscanf(resp.ExpiresIn, "%d", &exp)
	return Session{
		Email:        email,
		LocalID:      resp.LocalID,
		IDToken:      resp.IDToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    float64(time.Now().Unix() + int64(exp)),
	}, nil
}

func Refresh(apiKey string, s Session) (Session, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", s.RefreshToken)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://securetoken.googleapis.com/v1/token?key=%s", url.QueryEscape(apiKey)), strings.NewReader(form.Encode()))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Session{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return Session{}, fmt.Errorf("refresh failed: %s", string(body))
	}
	var data struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresIn    string `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return Session{}, err
	}
	exp := 3600
	fmt.Sscanf(data.ExpiresIn, "%d", &exp)
	out := s
	out.IDToken = data.IDToken
	if data.RefreshToken != "" {
		out.RefreshToken = data.RefreshToken
	}
	if data.UserID != "" {
		out.LocalID = data.UserID
	}
	out.ExpiresAt = float64(time.Now().Unix() + int64(exp))
	return out, nil
}

func EnsureSession(apiKey, path, email, password string) (Session, error) {
	if s, err := LoadSession(path); err == nil && s != nil {
		if !s.Expired() {
			return *s, nil
		}
		if refreshed, err := Refresh(apiKey, *s); err == nil {
			_ = SaveSession(path, refreshed)
			return refreshed, nil
		}
	}
	if email == "" || password == "" {
		return Session{}, fmt.Errorf("Todomate 로그인이 필요합니다")
	}
	s, err := SignIn(apiKey, email, password)
	if err != nil {
		return Session{}, err
	}
	if err := SaveSession(path, s); err != nil {
		return Session{}, err
	}
	return s, nil
}

func NewClient(apiKey, projectID string, session Session) *Client {
	return &Client{
		http:      &http.Client{Timeout: 45 * time.Second},
		APIKey:    apiKey,
		ProjectID: projectID,
		Session:   session,
	}
}

func (c *Client) base() string {
	return fmt.Sprintf("https://firestore.googleapis.com/v1/projects/%s/databases/(default)/documents", c.ProjectID)
}

func (c *Client) ListGoals() ([]models.Goal, error) {
	docs, err := c.runQuery(map[string]any{
		"from": []map[string]any{{"collectionId": "Goal"}},
		"where": map[string]any{
			"fieldFilter": map[string]any{
				"field": map[string]string{"fieldPath": "userID"},
				"op":    "EQUAL",
				"value": map[string]string{"stringValue": c.Session.LocalID},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.Goal, 0, len(docs))
	for _, d := range docs {
		out = append(out, parseGoal(d))
	}
	return out, nil
}

func (c *Client) ListActiveGoals(include, exclude []string) ([]models.Goal, error) {
	goals, err := c.ListGoals()
	if err != nil {
		return nil, err
	}
	inc := toSet(include)
	exc := toSet(exclude)
	var out []models.Goal
	for _, g := range goals {
		if !g.IsActive() {
			continue
		}
		if len(inc) > 0 && !inc[g.ID] {
			continue
		}
		if exc[g.ID] {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (c *Client) ListTodoItems(start, end *time.Time) ([]models.TodoItem, error) {
	docs, err := c.runQuery(map[string]any{
		"from": []map[string]any{{"collectionId": "TodoItem"}},
		"where": map[string]any{
			"fieldFilter": map[string]any{
				"field": map[string]string{"fieldPath": "writerID"},
				"op":    "EQUAL",
				"value": map[string]string{"stringValue": c.Session.LocalID},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.TodoItem, 0, len(docs))
	for _, d := range docs {
		item := parseTodo(d)
		if item.Date == nil {
			continue
		}
		if start != nil && item.Date.Before(*start) {
			continue
		}
		if end != nil && item.Date.After(*end) {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (c *Client) GetTodoItem(id string) (*models.TodoItem, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/TodoItem/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Session.IDToken)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusForbidden {
		return nil, nil
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("get todo: %s", string(body))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	item := parseTodo(doc)
	return &item, nil
}

func (c *Client) CreateTodoItem(content, goalID string, onDate time.Time, memo, routineID string) (models.TodoItem, error) {
	docID := randomID(20)
	nowMS := time.Now().UTC().UnixMilli()
	dateMS := time.Date(onDate.Year(), onDate.Month(), onDate.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	var routineVal any
	if routineID != "" {
		routineVal = routineID
	}
	fields := map[string]any{
		"id":           toValue(docID),
		"writerID":     toValue(c.Session.LocalID),
		"goalID":       toValue(goalID),
		"content":      toValue(content),
		"createTime":   toValue(nowMS),
		"date":         toValue(dateMS),
		"isDone":       toValue(false),
		"memo":         toValue(memo),
		"routineID":    toValue(routineVal),
		"remindAt":     toValue(nil),
		"doneTime":     toValue(nil),
		"hasPhoto":     toValue(false),
		"hasTimer":     toValue(false),
		"isMemoPublic": toValue(false),
	}
	urlStr := fmt.Sprintf("%s/TodoItem?documentId=%s", c.base(), url.QueryEscape(docID))
	var doc map[string]any
	if err := c.doJSON(http.MethodPost, urlStr, map[string]any{"fields": fields}, &doc); err != nil {
		return models.TodoItem{}, err
	}
	return parseTodo(doc), nil
}

// RoutineRepeatDaily matches Todomate RoutineRepeatType.daily (0).
const RoutineRepeatDaily = 0

type Routine struct {
	ID         string
	UserID     string
	Title      string
	GoalID     string
	RepeatType int
	StartDate  *time.Time
	EndDate    *time.Time
	CreateTime int64
	IsManual   bool
}

func (c *Client) CreateRoutine(title, goalID string, start, endInclusive time.Time) (Routine, error) {
	docID := randomID(20)
	nowMS := time.Now().UTC().UnixMilli()
	startMS := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	endMS := time.Date(endInclusive.Year(), endInclusive.Month(), endInclusive.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	fields := map[string]any{
		"id":         toValue(docID),
		"userID":     toValue(c.Session.LocalID),
		"title":      toValue(title),
		"goalID":     toValue(goalID),
		"repeatType": toValue(int64(RoutineRepeatDaily)),
		"startDate":  toValue(startMS),
		"endDate":    toValue(endMS),
		"createTime": toValue(nowMS),
		"isManual":   toValue(false),
		"time":       toValue(nil),
	}
	urlStr := fmt.Sprintf("%s/Routine?documentId=%s", c.base(), url.QueryEscape(docID))
	var doc map[string]any
	if err := c.doJSON(http.MethodPost, urlStr, map[string]any{"fields": fields}, &doc); err != nil {
		return Routine{}, err
	}
	startCopy := time.UnixMilli(startMS).UTC()
	endCopy := time.UnixMilli(endMS).UTC()
	return Routine{
		ID:         docID,
		UserID:     c.Session.LocalID,
		Title:      title,
		GoalID:     goalID,
		RepeatType: RoutineRepeatDaily,
		StartDate:  &startCopy,
		EndDate:    &endCopy,
		CreateTime: nowMS,
		IsManual:   false,
	}, nil
}

func (c *Client) DeleteRoutine(id string) error {
	req, err := http.NewRequest(http.MethodDelete, c.base()+"/Routine/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Session.IDToken)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("delete routine: %s", string(b))
	}
	return nil
}

func (c *Client) ListTodoItemsByRoutine(routineID string) ([]models.TodoItem, error) {
	if routineID == "" {
		return nil, nil
	}
	docs, err := c.runQuery(map[string]any{
		"from": []map[string]any{{"collectionId": "TodoItem"}},
		"where": map[string]any{
			"compositeFilter": map[string]any{
				"op": "AND",
				"filters": []map[string]any{
					{
						"fieldFilter": map[string]any{
							"field": map[string]string{"fieldPath": "writerID"},
							"op":    "EQUAL",
							"value": toValue(c.Session.LocalID),
						},
					},
					{
						"fieldFilter": map[string]any{
							"field": map[string]string{"fieldPath": "routineID"},
							"op":    "EQUAL",
							"value": toValue(routineID),
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.TodoItem, 0, len(docs))
	for _, doc := range docs {
		out = append(out, parseTodo(doc))
	}
	return out, nil
}

// CreateRoutineWithDailyTodos creates a daily Routine and one TodoItem per day (inclusive).
// Returns the routine and the start-day todo (for Google mapping).
func (c *Client) CreateRoutineWithDailyTodos(title, goalID, memo string, start, endInclusive time.Time) (Routine, models.TodoItem, int, error) {
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	endInclusive = time.Date(endInclusive.Year(), endInclusive.Month(), endInclusive.Day(), 0, 0, 0, 0, time.UTC)
	if endInclusive.Before(start) {
		endInclusive = start
	}
	routine, err := c.CreateRoutine(title, goalID, start, endInclusive)
	if err != nil {
		return Routine{}, models.TodoItem{}, 0, err
	}
	var first models.TodoItem
	count := 0
	for d := start; !d.After(endInclusive); d = d.AddDate(0, 0, 1) {
		item, err := c.CreateTodoItem(title, goalID, d, memo, routine.ID)
		if err != nil {
			return routine, first, count, err
		}
		count++
		if count == 1 {
			first = item
		}
	}
	return routine, first, count, nil
}

func (c *Client) DeleteRoutineAndTodos(routineID string) error {
	if routineID == "" {
		return nil
	}
	items, err := c.ListTodoItemsByRoutine(routineID)
	if err != nil {
		return err
	}
	for _, item := range items {
		_ = c.DeleteTodoItem(item.ID)
	}
	return c.DeleteRoutine(routineID)
}

func (c *Client) UpdateTodoItem(id string, content *string, onDate *time.Time, memo *string) (models.TodoItem, error) {
	fields := map[string]any{}
	var masks []string
	if content != nil {
		fields["content"] = toValue(*content)
		masks = append(masks, "content")
	}
	if onDate != nil {
		ms := time.Date(onDate.Year(), onDate.Month(), onDate.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
		fields["date"] = toValue(ms)
		masks = append(masks, "date")
	}
	if memo != nil {
		fields["memo"] = toValue(*memo)
		masks = append(masks, "memo")
	}
	q := url.Values{}
	for _, m := range masks {
		q.Add("updateMask.fieldPaths", m)
	}
	urlStr := fmt.Sprintf("%s/TodoItem/%s?%s", c.base(), url.PathEscape(id), q.Encode())
	var doc map[string]any
	if err := c.doJSON(http.MethodPatch, urlStr, map[string]any{"fields": fields}, &doc); err != nil {
		return models.TodoItem{}, err
	}
	return parseTodo(doc), nil
}

func (c *Client) DeleteTodoItem(id string) error {
	req, err := http.NewRequest(http.MethodDelete, c.base()+"/TodoItem/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Session.IDToken)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("delete todo: %s", string(b))
	}
	return nil
}

func (c *Client) runQuery(structured map[string]any) ([]map[string]any, error) {
	var rows []map[string]any
	if err := c.doJSON(http.MethodPost, c.base()+":runQuery", map[string]any{"structuredQuery": structured}, &rows); err != nil {
		return nil, err
	}
	var docs []map[string]any
	for _, row := range rows {
		if doc, ok := row["document"].(map[string]any); ok {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

func (c *Client) doJSON(method, urlStr string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Session.IDToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, urlStr, string(b))
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func postJSON(urlStr string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := http.Post(urlStr, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", res.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

func parseGoal(doc map[string]any) models.Goal {
	fields := docFields(doc)
	id := asString(fields["id"])
	if id == "" {
		id = docID(doc)
	}
	return models.Goal{
		ID:         id,
		UserID:     asString(fields["userID"]),
		Title:      asString(fields["title"]),
		CreateTime: asInt64(fields["createTime"]),
		Priority:   int(asInt64(fields["priority"])),
		Color:      asString(fields["color"]),
		IsPublic:   asBool(fields["isPublic"]),
		FinishType: fields["finishType"],
		CrewID:     asString(fields["crewId"]),
	}
}

func parseTodo(doc map[string]any) models.TodoItem {
	fields := docFields(doc)
	id := asString(fields["id"])
	if id == "" {
		id = docID(doc)
	}
	item := models.TodoItem{
		ID:         id,
		WriterID:   asString(fields["writerID"]),
		GoalID:     asString(fields["goalID"]),
		Content:    asString(fields["content"]),
		CreateTime: asInt64(fields["createTime"]),
		IsDone:     asBool(fields["isDone"]),
		Memo:       asString(fields["memo"]),
		RoutineID:  asString(fields["routineID"]),
	}
	if v := fields["date"]; v != nil {
		if d := parseDate(v); d != nil {
			item.Date = d
		}
	}
	if v, ok := fields["remindAt"]; ok && v != nil {
		n := asInt64(v)
		item.RemindAt = &n
	}
	if v, ok := fields["doneTime"]; ok && v != nil {
		n := asInt64(v)
		item.DoneTime = &n
	}
	if item.CreateTime > 0 {
		item.UpdatedAt = time.UnixMilli(item.CreateTime).UTC()
	}
	for _, p := range []*int64{item.DoneTime, item.RemindAt} {
		if p != nil && *p > 0 {
			candidate := time.UnixMilli(*p).UTC()
			if candidate.After(item.UpdatedAt) {
				item.UpdatedAt = candidate
			}
		}
	}
	return item
}

func docFields(doc map[string]any) map[string]any {
	raw, _ := doc["fields"].(map[string]any)
	out := map[string]any{}
	for k, v := range raw {
		out[k] = fromValue(v)
	}
	return out
}

func fromValue(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if x, ok := m["stringValue"]; ok {
		return x
	}
	if x, ok := m["integerValue"]; ok {
		switch t := x.(type) {
		case string:
			var n int64
			fmt.Sscanf(t, "%d", &n)
			return n
		case float64:
			return int64(t)
		}
	}
	if x, ok := m["doubleValue"]; ok {
		return x
	}
	if x, ok := m["booleanValue"]; ok {
		return x
	}
	if _, ok := m["nullValue"]; ok {
		return nil
	}
	return v
}

func toValue(v any) map[string]any {
	if v == nil {
		return map[string]any{"nullValue": nil}
	}
	switch t := v.(type) {
	case bool:
		return map[string]any{"booleanValue": t}
	case int:
		return map[string]any{"integerValue": fmt.Sprintf("%d", t)}
	case int64:
		return map[string]any{"integerValue": fmt.Sprintf("%d", t)}
	case string:
		return map[string]any{"stringValue": t}
	default:
		return map[string]any{"stringValue": fmt.Sprintf("%v", t)}
	}
}

func parseDate(v any) *time.Time {
	switch t := v.(type) {
	case int64:
		sec := t
		if t > 10_000_000_000 {
			sec = t / 1000
		}
		d := time.Unix(sec, 0).UTC()
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		return &day
	case float64:
		return parseDate(int64(t))
	case string:
		if n, err := parseInt64(t); err == nil {
			return parseDate(n)
		}
		if d, err := time.Parse("2006-01-02", t[:min(10, len(t))]); err == nil {
			return &d
		}
	}
	return nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
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

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case string:
		n, _ := parseInt64(t)
		return n
	}
	return 0
}

func docID(doc map[string]any) string {
	name, _ := doc["name"].(string)
	parts := strings.Split(name, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func toSet(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			m[id] = true
		}
	}
	return m
}

func randomID(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to time-based non-negative index.
		now := uint64(time.Now().UnixNano())
		for i := range b {
			now = now*1103515245 + 12345
			b[i] = letters[now%uint64(len(letters))]
		}
		return string(b)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
