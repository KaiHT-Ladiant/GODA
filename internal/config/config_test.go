package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDryRunDefaultsToTrueWhenOmitted(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.DryRun || s.HardDelete || s.MaxDeletionsPerRun != 3 {
		t.Fatalf("defaults dry=%v hard=%v max=%d", s.DryRun, s.HardDelete, s.MaxDeletionsPerRun)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(`
[sync]
interval_seconds = 30
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.DryRun {
		t.Fatal("omitted dry_run overrode the safe default")
	}
	if s.IntervalSeconds != 30 {
		t.Fatalf("interval = %d", s.IntervalSeconds)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(`
[sync]
dry_run = false
max_deletions_per_run = 0
hard_delete = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.DryRun || !s.HardDelete || s.MaxDeletionsPerRun != 0 {
		t.Fatalf("explicit dry=%v hard=%v max=%d", s.DryRun, s.HardDelete, s.MaxDeletionsPerRun)
	}
}
