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

// chChanCell accumulates one (channel_hash, observer_idx) cell for an hour.
type chChanCell struct {
	msgCount, decryptedCount int
	name                     string
	lastActivity             string
	msglenSum, msglenCount   int
	msglenMin, msglenMax     int
	haveMsglen               bool
	msglenBins               []int
	senders                  map[string]int
}

func newChChanCell() *chChanCell {
	return &chChanCell{
		msglenBins: make([]int, chMsgLenBinCount),
		senders:    map[string]int{},
	}
}

// chDecodedGrp mirrors the decoded_json shape computeAnalyticsChannels reads.
type chDecodedGrp struct {
	Channel      string      `json:"channel"`
	ChannelHash  interface{} `json:"channelHash"`
	ChannelHash2 string      `json:"channel_hash"`
	Text         string      `json:"text"`
	Sender       string      `json:"sender"`
}

func chHashStr(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// recomputeChannelRollupHour rebuilds the channel rollup for one hour bucket
// ("2026-05-18T10") from raw type-5 transmissions. Idempotent. The read runs
// outside the write transaction and uses the indexed first_seen range.
func recomputeChannelRollupHour(rw *sql.DB, hour string) error {
	ht, err := time.Parse("2006-01-02T15", hour)
	if err != nil {
		return fmt.Errorf("channel recompute parse hour %q: %w", hour, err)
	}
	lo := ht.UTC().Format("2006-01-02T15:04:05Z")
	hi := ht.UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")

	rows, err := rw.Query(`
		SELECT t.id, t.first_seen, t.decoded_json, o.observer_idx
		FROM transmissions t
		LEFT JOIN observations o ON o.transmission_id = t.id
		WHERE t.payload_type = 5 AND t.first_seen >= ? AND t.first_seen < ?`, lo, hi)
	if err != nil {
		return fmt.Errorf("channel recompute scan: %w", err)
	}
	type ck struct {
		hash string
		obs  int
	}
	cells := map[ck]*chChanCell{}
	txByChan := map[string]map[int]bool{}
	for rows.Next() {
		var txID int
		var firstSeen, decodedJSON string
		var obsN sql.NullInt64
		if err := rows.Scan(&txID, &firstSeen, &decodedJSON, &obsN); err != nil {
			rows.Close()
			return fmt.Errorf("channel recompute row: %w", err)
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
		key := ck{hash, obsIdx}
		c := cells[key]
		if c == nil {
			c = newChChanCell()
			c.name = name
			cells[key] = c
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
		if txByChan[hash] == nil {
			txByChan[hash] = map[int]bool{}
		}
		txByChan[hash][txID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("channel recompute rows: %w", err)
	}

	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("channel recompute begin: %w", err)
	}
	defer tx.Rollback()
	for _, t := range []string{"channel_rollup", "channel_sender_rollup", "channel_rollup_tx"} {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE hour=?`, hour); err != nil {
			return fmt.Errorf("channel recompute delete %s: %w", t, err)
		}
	}
	for key, c := range cells {
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
			return fmt.Errorf("channel recompute insert rollup: %w", err)
		}
		for sender, cnt := range c.senders {
			if _, err := tx.Exec(`INSERT INTO channel_sender_rollup
				(hour,channel_hash,observer_idx,sender,msg_count) VALUES (?,?,?,?,?)`,
				hour, key.hash, key.obs, sender, cnt); err != nil {
				return fmt.Errorf("channel recompute insert sender: %w", err)
			}
		}
	}
	for hash, set := range txByChan {
		if _, err := tx.Exec(`INSERT INTO channel_rollup_tx(hour,channel_hash,distinct_tx)
			VALUES (?,?,?)`, hour, hash, len(set)); err != nil {
			return fmt.Errorf("channel recompute insert tx: %w", err)
		}
	}
	return tx.Commit()
}
