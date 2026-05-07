package autoreg

import (
	"database/sql"
	"time"
)

type SyncedStore struct {
	db *sql.DB
}

func NewSyncedStore(db *sql.DB) (*SyncedStore, error) {
	s := &SyncedStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SyncedStore) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS autoreg_synced (
		py_account_id INTEGER PRIMARY KEY,
		platform TEXT NOT NULL DEFAULT '',
		pool_account_id TEXT NOT NULL DEFAULT '',
		synced_at TEXT NOT NULL
	)`)
	return err
}

func (s *SyncedStore) IsSynced(pyID int) bool {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM autoreg_synced WHERE py_account_id = ?", pyID).Scan(&count)
	return err == nil && count > 0
}

func (s *SyncedStore) MarkSynced(pyID int, platform, poolAccountID string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO autoreg_synced (py_account_id, platform, pool_account_id, synced_at) VALUES (?, ?, ?, ?)`,
		pyID, platform, poolAccountID, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s *SyncedStore) ListSynced() ([]SyncedRecord, error) {
	rows, err := s.db.Query("SELECT py_account_id, platform, pool_account_id, synced_at FROM autoreg_synced ORDER BY synced_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []SyncedRecord
	for rows.Next() {
		var r SyncedRecord
		if err := rows.Scan(&r.PyAccountID, &r.Platform, &r.PoolAccountID, &r.SyncedAt); err != nil {
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

type SyncedRecord struct {
	PyAccountID   int
	Platform      string
	PoolAccountID string
	SyncedAt      string
}
