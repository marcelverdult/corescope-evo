// channel_rollup.go — channels analytics rollup: schema, single-hour recompute.
// See .specs/2026-05-19-channels-rollup-design.md. Mirrors rf_rollup.go.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Message-length histogram bins: 0..512 bytes, 16-byte width -> 32 bins.
const (
	chMsgLenBinMin, chMsgLenBinWidth, chMsgLenBinCount = 0, 16, 32
)

// ensureChannelRollupTable creates the channels rollup tables. Idempotent.
func ensureChannelRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for channel_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS channel_rollup (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			msg_count INTEGER NOT NULL DEFAULT 0,
			decrypted_count INTEGER NOT NULL DEFAULT 0,
			name TEXT,
			last_activity TEXT,
			msglen_sum INTEGER NOT NULL DEFAULT 0,
			msglen_count INTEGER NOT NULL DEFAULT 0,
			msglen_min INTEGER, msglen_max INTEGER,
			msglen_bins BLOB,
			PRIMARY KEY (hour, channel_hash, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_rollup_hour ON channel_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS channel_sender_rollup (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			sender TEXT NOT NULL,
			msg_count INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, channel_hash, observer_idx, sender)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_sender_rollup_hour ON channel_sender_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS channel_rollup_tx (
			hour TEXT NOT NULL,
			channel_hash TEXT NOT NULL,
			distinct_tx INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, channel_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS channel_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("channel_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}

var _ = json.Unmarshal // used by recompute (Task 2)
var _ = sql.ErrNoRows
var _ = time.Now
