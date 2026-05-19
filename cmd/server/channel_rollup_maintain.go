// channel_rollup_maintain.go — channels rollup backfill + maintenance.
// Mirrors rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const chRollupWatermarkKey = "channel_rollup_last_tx_id"

var chRollupMaintMu sync.Mutex

func chRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM channel_rollup_meta WHERE key=?`,
		chRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("channel watermark read: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func chSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO channel_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		chRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("channel watermark set: %w", err)
	}
	return nil
}

// runChannelRollupMaintenance recomputes every hour with type-5 transmissions
// newer than the watermark, then advances the watermark.
func runChannelRollupMaintenance(rw *sql.DB) error {
	wm, err := chRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', first_seen)
		FROM transmissions WHERE payload_type = 5 AND id > ?`, wm)
	if err != nil {
		return fmt.Errorf("channel maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("channel maintenance scan hour: %w", err)
		}
		hours = append(hours, h)
	}
	rows.Close()
	for _, h := range hours {
		if err := recomputeChannelRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM transmissions WHERE payload_type=5`).Scan(&maxID); err != nil {
		return fmt.Errorf("channel maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return chSetRollupWatermark(rw, maxID.Int64)
	}
	return nil
}

func runChannelRollupMaintenanceGuarded(rw *sql.DB) {
	if !chRollupMaintMu.TryLock() {
		log.Printf("[channel-rollup] maintenance skipped — run in progress")
		return
	}
	defer chRollupMaintMu.Unlock()
	if err := runChannelRollupMaintenance(rw); err != nil {
		log.Printf("[channel-rollup] maintenance: %v", err)
	}
}

func backfillChannelRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[channel-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[channel-rollup] backfill open rw: %v", err)
		return
	}
	chRollupMaintMu.Lock()
	defer chRollupMaintMu.Unlock()
	start := time.Now()
	if err := runChannelRollupMaintenance(rw); err != nil {
		log.Printf("[channel-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[channel-rollup] backfill complete in %s", time.Since(start))
}

func chRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM channel_rollup_meta WHERE key=?`,
		chRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
