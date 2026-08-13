package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

type Settings struct {
	IntervalSeconds             int
	LookbackDays                int
	LookaheadDays               int
	DryRun                      bool
	GoogleCalendarID            string
	PreferGoogleOnTie           bool
	ImportUnmappedGoogleEvents  bool
	SkipCompletedTodos          bool
	DataDir                     string
	GoogleCredentials           string
	GoogleToken                 string
	TodomateSession             string
	SQLiteDB                    string
	TodomateAPIKey              string
	TodomateProjectID           string
	IncludeGoalIDs              []string
	ExcludeGoalIDs              []string
	DefaultGoalID               string
	Root                        string
}

type fileConfig struct {
	Sync struct {
		IntervalSeconds            int    `toml:"interval_seconds"`
		LookbackDays               int    `toml:"lookback_days"`
		LookaheadDays              int    `toml:"lookahead_days"`
		DryRun                     bool   `toml:"dry_run"`
		GoogleCalendarID           string `toml:"google_calendar_id"`
		PreferGoogleOnTie          bool   `toml:"prefer_google_on_tie"`
		ImportUnmappedGoogleEvents bool   `toml:"import_unmapped_google_events"`
		SkipCompletedTodos         bool   `toml:"skip_completed_todos"`
	} `toml:"sync"`
	Paths struct {
		DataDir           string `toml:"data_dir"`
		GoogleCredentials string `toml:"google_credentials"`
		GoogleToken       string `toml:"google_token"`
		TodomateSession   string `toml:"todomate_session"`
		SQLiteDB          string `toml:"sqlite_db"`
	} `toml:"paths"`
	Todomate struct {
		APIKey         string   `toml:"api_key"`
		ProjectID      string   `toml:"project_id"`
		IncludeGoalIDs []string `toml:"include_goal_ids"`
		ExcludeGoalIDs []string `toml:"exclude_goal_ids"`
		DefaultGoalID  string   `toml:"default_goal_id"`
	} `toml:"todomate"`
}

func FindRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for i := 0; i < 6; i++ {
		if fileExists(filepath.Join(dir, "config.local.toml")) ||
			fileExists(filepath.Join(dir, "config.example.toml")) ||
			fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}

func Load(root string) (*Settings, error) {
	if root == "" {
		root = FindRoot()
	}
	s := defaults(root)
	candidates := []string{
		filepath.Join(root, "config.local.toml"),
		filepath.Join(root, "config.toml"),
		filepath.Join(root, "config.example.toml"),
	}
	var raw fileConfig
	loaded := false
	for _, p := range candidates {
		if !fileExists(p) {
			continue
		}
		if _, err := toml.DecodeFile(p, &raw); err != nil {
			return nil, err
		}
		loaded = true
		break
	}
	if loaded {
		applyFile(s, raw, root)
	}
	_ = os.MkdirAll(s.DataDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(s.SQLiteDB), 0o755)
	return s, nil
}

func defaults(root string) *Settings {
	return &Settings{
		IntervalSeconds:            60,
		LookbackDays:               14,
		LookaheadDays:              60,
		DryRun:                     true,
		GoogleCalendarID:           "primary",
		PreferGoogleOnTie:          true,
		ImportUnmappedGoogleEvents: false,
		SkipCompletedTodos:         true,
		DataDir:                    filepath.Join(root, "data"),
		GoogleCredentials:          filepath.Join(root, "credentials.json"),
		GoogleToken:                filepath.Join(root, "token.json"),
		TodomateSession:            filepath.Join(root, "todomate_session.json"),
		SQLiteDB:                   filepath.Join(root, "data", "sync.sqlite3"),
		TodomateAPIKey:             "",
		TodomateProjectID:          "mate-914f3",
		Root:                       root,
	}
}

func applyFile(s *Settings, raw fileConfig, root string) {
	if raw.Sync.IntervalSeconds > 0 {
		s.IntervalSeconds = raw.Sync.IntervalSeconds
	}
	if raw.Sync.LookbackDays > 0 {
		s.LookbackDays = raw.Sync.LookbackDays
	}
	if raw.Sync.LookaheadDays > 0 {
		s.LookaheadDays = raw.Sync.LookaheadDays
	}
	s.DryRun = raw.Sync.DryRun
	if raw.Sync.GoogleCalendarID != "" {
		s.GoogleCalendarID = raw.Sync.GoogleCalendarID
	}
	s.PreferGoogleOnTie = raw.Sync.PreferGoogleOnTie
	s.ImportUnmappedGoogleEvents = raw.Sync.ImportUnmappedGoogleEvents
	s.SkipCompletedTodos = raw.Sync.SkipCompletedTodos

	s.DataDir = resolve(root, raw.Paths.DataDir, s.DataDir)
	s.GoogleCredentials = resolve(root, raw.Paths.GoogleCredentials, s.GoogleCredentials)
	s.GoogleToken = resolve(root, raw.Paths.GoogleToken, s.GoogleToken)
	s.TodomateSession = resolve(root, raw.Paths.TodomateSession, s.TodomateSession)
	s.SQLiteDB = resolve(root, raw.Paths.SQLiteDB, s.SQLiteDB)

	if raw.Todomate.APIKey != "" {
		s.TodomateAPIKey = raw.Todomate.APIKey
	}
	if raw.Todomate.ProjectID != "" {
		s.TodomateProjectID = raw.Todomate.ProjectID
	}
	s.IncludeGoalIDs = raw.Todomate.IncludeGoalIDs
	s.ExcludeGoalIDs = raw.Todomate.ExcludeGoalIDs
	s.DefaultGoalID = raw.Todomate.DefaultGoalID
}

func resolve(root, value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(root, value)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func FormatBool(v bool) string {
	return strconv.FormatBool(v)
}
