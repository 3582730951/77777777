package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupConfig controls automatic SQLite backups.
type BackupConfig struct {
	Dir      string        // directory for backup files
	Interval time.Duration // how often to run
	Keep     int           // number of backups to retain
}

// RunBackupLoop runs periodic VACUUM INTO backups in a background goroutine.
// Cancel the context to stop.
func (s *Store) RunBackupLoop(ctx context.Context, cfg BackupConfig) {
	if cfg.Dir == "" {
		return
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 3
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		slog.Error("backup dir creation failed", "err", err)
		return
	}
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Backup(cfg.Dir); err != nil {
					slog.Error("backup failed", "err", err)
				} else {
					pruneBackups(cfg.Dir, cfg.Keep)
				}
			}
		}
	}()
}

// Backup creates a consistent SQLite backup using VACUUM INTO.
func (s *Store) Backup(dir string) error {
	name := fmt.Sprintf("pool-%s.db", time.Now().UTC().Format("20060102-150405"))
	dest := filepath.Join(dir, name)
	safe := strings.ReplaceAll(dest, "'", "''")
	_, err := s.db.Exec(fmt.Sprintf(`VACUUM INTO '%s'`, safe))
	if err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	slog.Info("backup created", "path", dest)
	return nil
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".db" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) <= keep {
		return
	}
	for _, f := range files[:len(files)-keep] {
		os.Remove(f)
		slog.Info("pruned old backup", "path", f)
	}
}
