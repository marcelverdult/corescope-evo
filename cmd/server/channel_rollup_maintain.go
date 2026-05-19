// channel_rollup_maintain.go — channels rollup backfill + maintenance.
// Mirrors rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"encoding/json"
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

// runChannelRollupMaintenance finds every hour touched by new type-5
// transmissions since the watermark, then re-scans ALL rows for each touched
// hour (not just the new ones) in a single bulk query. This is idempotent and
// handles partial-hour additions correctly while avoiding N per-hour round-trips
// for large backfills.
func runChannelRollupMaintenance(rw *sql.DB) error {
	wm, err := chRollupWatermark(rw)
	if err != nil {
		return err
	}

	// Step 1: collect distinct hours touched by new transmissions + max id.
	var maxID int64
	touchedHours := map[string]bool{}
	{
		rows, err := rw.Query(`
			SELECT id, strftime('%Y-%m-%dT%H', first_seen)
			FROM transmissions WHERE payload_type = 5 AND id > ?
			ORDER BY id`, wm)
		if err != nil {
			return fmt.Errorf("channel maintenance touched-hours: %w", err)
		}
		for rows.Next() {
			var txID int64
			var hour string
			if err := rows.Scan(&txID, &hour); err != nil {
				rows.Close()
				return fmt.Errorf("channel maintenance scan hour: %w", err)
			}
			if txID > maxID {
				maxID = txID
			}
			touchedHours[hour] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("channel maintenance touched-hours err: %w", err)
		}
	}
	if len(touchedHours) == 0 {
		return nil
	}

	// Step 2: for all touched hours, read ALL type-5 rows in those hours in one
	// query, accumulate by (hour, channel_hash, observer_idx).
	// Build a placeholder list for the IN clause.
	hours := make([]string, 0, len(touchedHours))
	for h := range touchedHours {
		hours = append(hours, h)
	}
	inPlaceholders := rfIntPlaceholders(len(hours))
	args := make([]interface{}, len(hours))
	for i, h := range hours {
		args[i] = h
	}

	allRows, err := rw.Query(`
		SELECT t.id, strftime('%Y-%m-%dT%H', t.first_seen), t.first_seen, t.decoded_json,
		       o.observer_idx
		FROM transmissions t
		LEFT JOIN observations o ON o.transmission_id = t.id
		WHERE t.payload_type = 5
		  AND strftime('%Y-%m-%dT%H', t.first_seen) IN (`+inPlaceholders+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("channel maintenance full-hour scan: %w", err)
	}

	type hck struct {
		hour string
		hash string
		obs  int
	}
	type hourData struct {
		cells    map[hck]*chChanCell
		txByChan map[string]map[int]bool
	}
	byHour := map[string]*hourData{}

	for allRows.Next() {
		var txID int64
		var hour, firstSeen, decodedJSON string
		var obsN sql.NullInt64
		if err := allRows.Scan(&txID, &hour, &firstSeen, &decodedJSON, &obsN); err != nil {
			allRows.Close()
			return fmt.Errorf("channel maintenance row: %w", err)
		}
		obsIdx := -1
		if obsN.Valid {
			obsIdx = int(obsN.Int64)
		}
		var d chDecodedGrp
		if json.Unmarshal([]byte(decodedJSON), &d) != nil {
			continue
		}
		hash := chHashStr(d.ChannelHash)
		if hash == "" {
			hash = d.ChannelHash2
		}
		if hash == "" {
			hash = "?"
		}
		name := d.Channel
		if name == "" {
			name = "ch" + hash
		}
		encrypted := d.Text == "" && d.Sender == ""
		if name != "" && name != "ch"+hash && !channelNameMatchesHash(name, hash) {
			name = "ch" + hash
			encrypted = true
		}

		hd := byHour[hour]
		if hd == nil {
			hd = &hourData{
				cells:    map[hck]*chChanCell{},
				txByChan: map[string]map[int]bool{},
			}
			byHour[hour] = hd
		}
		key := hck{hour, hash, obsIdx}
		c := hd.cells[key]
		if c == nil {
			c = newChChanCell()
			c.name = name
			hd.cells[key] = c
		} else if isPlaceholderName(c.name) && !isPlaceholderName(name) {
			c.name = name
		}
		c.msgCount++
		c.lastActivity = firstSeen
		if !encrypted {
			c.decryptedCount++
		}
		if d.Sender != "" {
			c.senders[d.Sender]++
		}
		if d.Text != "" {
			n := len(d.Text)
			c.msglenSum += n
			c.msglenCount++
			if !c.haveMsglen || n < c.msglenMin {
				c.msglenMin = n
			}
			if !c.haveMsglen || n > c.msglenMax {
				c.msglenMax = n
			}
			c.haveMsglen = true
			c.msglenBins[rfBinIndex(n, chMsgLenBinMin, chMsgLenBinWidth, chMsgLenBinCount)]++
		}
		if hd.txByChan[hash] == nil {
			hd.txByChan[hash] = map[int]bool{}
		}
		hd.txByChan[hash][int(txID)] = true
	}
	allRows.Close()
	if err := allRows.Err(); err != nil {
		return fmt.Errorf("channel maintenance rows: %w", err)
	}

	// Step 3: write all touched hours in a single transaction.
	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("channel maintenance begin: %w", err)
	}
	defer tx.Rollback()
	for hour, hd := range byHour {
		for _, tbl := range []string{"channel_rollup", "channel_sender_rollup", "channel_rollup_tx"} {
			if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE hour=?`, hour); err != nil {
				return fmt.Errorf("channel maintenance delete %s %q: %w", tbl, hour, err)
			}
		}
		for key, c := range hd.cells {
			var mn, mx interface{}
			if c.haveMsglen {
				mn, mx = c.msglenMin, c.msglenMax
			}
			if _, err := tx.Exec(`INSERT INTO channel_rollup
				(hour,channel_hash,observer_idx,msg_count,decrypted_count,name,last_activity,
				 msglen_sum,msglen_count,msglen_min,msglen_max,msglen_bins)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				hour, key.hash, key.obs, c.msgCount, c.decryptedCount, c.name, c.lastActivity,
				c.msglenSum, c.msglenCount, mn, mx, rfPackBins(c.msglenBins)); err != nil {
				return fmt.Errorf("channel maintenance insert rollup %q: %w", hour, err)
			}
			for sender, cnt := range c.senders {
				if _, err := tx.Exec(`INSERT INTO channel_sender_rollup
					(hour,channel_hash,observer_idx,sender,msg_count) VALUES (?,?,?,?,?)`,
					hour, key.hash, key.obs, sender, cnt); err != nil {
					return fmt.Errorf("channel maintenance insert sender %q: %w", hour, err)
				}
			}
		}
		for hash, set := range hd.txByChan {
			if _, err := tx.Exec(`INSERT INTO channel_rollup_tx(hour,channel_hash,distinct_tx)
				VALUES (?,?,?)`, hour, hash, len(set)); err != nil {
				return fmt.Errorf("channel maintenance insert tx %q: %w", hour, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("channel maintenance commit: %w", err)
	}
	if maxID > wm {
		return chSetRollupWatermark(rw, maxID)
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
