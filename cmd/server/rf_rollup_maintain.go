// rf_rollup_maintain.go — RF rollup backfill + incremental maintenance.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

const rfRollupWatermarkKey = "rf_rollup_last_obs_id"

func rfRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM rf_rollup_meta WHERE key=?`,
		rfRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func rfSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO rf_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		rfRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return err
}

// runRFRollupMaintenance recomputes every hour bucket that has observations
// newer than the watermark, then advances the watermark.
func runRFRollupMaintenance(rw *sql.DB) error {
	wm, err := rfRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', timestamp, 'unixepoch')
		FROM observations WHERE id > ?`, wm)
	if err != nil {
		return fmt.Errorf("maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("maintenance scan hour: %w", err)
		}
		hours = append(hours, h)
	}
	rows.Close()
	for _, h := range hours {
		if err := recomputeRFRollupHour(rw, h); err != nil {
			return err
		}
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return rfSetRollupWatermark(rw, maxID.Int64)
	}
	return nil
}

// backfillRFRollupAsync runs the first full rollup build in the background.
// Backfill is just maintenance from watermark 0 (all hours touched).
func backfillRFRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[rf-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[rf-rollup] backfill open rw: %v", err)
		return
	}
	start := time.Now()
	if err := runRFRollupMaintenance(rw); err != nil {
		log.Printf("[rf-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[rf-rollup] backfill complete in %s", time.Since(start))
}

// rfRollupReady reports whether the rollup has been populated at least once.
func rfRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM rf_rollup_meta WHERE key=?`,
		rfRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
