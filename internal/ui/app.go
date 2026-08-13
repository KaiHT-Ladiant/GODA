package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/local/google-to-domate/internal/config"
	"github.com/local/google-to-domate/internal/googleauth"
	"github.com/local/google-to-domate/internal/googlecal"
	"github.com/local/google-to-domate/internal/store"
	"github.com/local/google-to-domate/internal/syncer"
	"github.com/local/google-to-domate/internal/todomate"
)

//go:embed web/*
var webFS embed.FS

type Server struct {
	settings *config.Settings
	mu       sync.Mutex
	logs     []string
	busy     bool
	polling  bool
	stopPoll chan struct{}
}

type statusResp struct {
	GoogleLoggedIn   bool   `json:"googleLoggedIn"`
	TodomateLoggedIn bool   `json:"todomateLoggedIn"`
	TodomateEmail    string `json:"todomateEmail"`
	DryRun           bool   `json:"dryRun"`
	IntervalSeconds  int    `json:"intervalSeconds"`
	Busy             bool   `json:"busy"`
	Polling          bool   `json:"polling"`
	Logs             string `json:"logs"`
}

func Run(settings *config.Settings) error {
	s := &Server{settings: settings}
	mux := http.NewServeMux()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/google/login", s.handleGoogleLogin)
	mux.HandleFunc("/api/todomate/login", s.handleTodomateLogin)
	mux.HandleFunc("/api/sync/once", s.handleSyncOnce)
	mux.HandleFunc("/api/poll/start", s.handlePollStart)
	mux.HandleFunc("/api/poll/stop", s.handlePollStop)
	mux.HandleFunc("/api/settings", s.handleSettings)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	s.appendLog("GODA GUI server: %s", url)
	_ = openBrowser(url)

	srv := &http.Server{Handler: mux}
	fmt.Println("GODA GUI:", url)
	return srv.Serve(ln)
}

func (s *Server) appendLog(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, line)
	if len(s.logs) > 500 {
		s.logs = s.logs[len(s.logs)-400:]
	}
}

func (s *Server) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.logs, "\n")
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	email := ""
	if sess, err := todomate.LoadSession(s.settings.TodomateSession); err == nil {
		email = sess.Email
	}
	s.mu.Lock()
	busy, polling := s.busy, s.polling
	s.mu.Unlock()
	writeJSON(w, statusResp{
		GoogleLoggedIn:   googleauth.HasValidToken(s.settings.GoogleToken),
		TodomateLoggedIn: todomate.HasSession(s.settings.TodomateSession),
		TodomateEmail:    email,
		DryRun:           s.settings.DryRun,
		IntervalSeconds:  s.settings.IntervalSeconds,
		Busy:             busy,
		Polling:          polling,
		Logs:             s.logText(),
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DryRun          *bool `json:"dryRun"`
		IntervalSeconds *int  `json:"intervalSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.DryRun != nil {
		s.settings.DryRun = *body.DryRun
	}
	if body.IntervalSeconds != nil && *body.IntervalSeconds >= 10 {
		s.settings.IntervalSeconds = *body.IntervalSeconds
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	go func() {
		s.setBusy(true)
		defer s.setBusy(false)
		s.appendLog("Google 로그인 시작 (브라우저 확인)…")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := googleauth.Login(ctx, s.settings.GoogleCredentials, s.settings.GoogleToken); err != nil {
			s.appendLog("Google 로그인 실패: %v", err)
			return
		}
		s.appendLog("Google 로그인 완료")
	}()
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

func (s *Server) handleTodomateLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	go func() {
		s.setBusy(true)
		defer s.setBusy(false)
		s.appendLog("Todomate 로그인 중…")
		sess, err := todomate.SignIn(s.settings.TodomateAPIKey, strings.TrimSpace(body.Email), body.Password)
		if err != nil {
			s.appendLog("Todomate 로그인 실패: %v", err)
			return
		}
		if err := todomate.SaveSession(s.settings.TodomateSession, sess); err != nil {
			s.appendLog("세션 저장 실패: %v", err)
			return
		}
		s.appendLog("Todomate 로그인 완료")
	}()
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

func (s *Server) handleSyncOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	go func() {
		s.setBusy(true)
		defer s.setBusy(false)
		s.appendLog("동기화 1회 시작…")
		if err := s.runSync(); err != nil {
			s.appendLog("동기화 실패: %v", err)
			return
		}
		s.appendLog("동기화 1회 완료")
	}()
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

func (s *Server) handlePollStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	s.mu.Lock()
	if s.polling {
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "already": true})
		return
	}
	s.polling = true
	s.stopPoll = make(chan struct{})
	stop := s.stopPoll
	interval := s.settings.IntervalSeconds
	if interval < 10 {
		interval = 10
	}
	s.mu.Unlock()

	s.appendLog("폴링 시작 (매 %d초, dry_run=%v)", interval, s.settings.DryRun)
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		if err := s.runSync(); err != nil {
			s.appendLog("동기화 실패: %v", err)
		}
		for {
			select {
			case <-stop:
				s.appendLog("폴링 중지됨")
				return
			case <-ticker.C:
				if err := s.runSync(); err != nil {
					s.appendLog("동기화 실패: %v", err)
				}
			}
		}
	}()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePollStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", 405)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.polling {
		close(s.stopPoll)
		s.polling = false
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) setBusy(v bool) {
	s.mu.Lock()
	s.busy = v
	s.mu.Unlock()
}

func (s *Server) runSync() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	httpClient, err := googleauth.GetClient(ctx, s.settings.GoogleCredentials, s.settings.GoogleToken)
	if err != nil {
		return fmt.Errorf("Google: %w", err)
	}
	session, err := todomate.EnsureSession(s.settings.TodomateAPIKey, s.settings.TodomateSession, "", "")
	if err != nil {
		return fmt.Errorf("Todomate: %w", err)
	}
	st, err := store.Open(s.settings.SQLiteDB)
	if err != nil {
		return err
	}
	defer st.Close()
	engine := &syncer.Engine{
		Settings: s.settings,
		Google:   googlecal.New(httpClient, s.settings.GoogleCalendarID),
		Todo:     todomate.NewClient(s.settings.TodomateAPIKey, s.settings.TodomateProjectID, session),
		Store:    st,
		Logf:     s.appendLog,
	}
	return engine.RunOnce(ctx)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
