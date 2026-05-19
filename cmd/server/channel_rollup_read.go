// channel_rollup_read.go — channels analytics result assembly from the rollup.

package main

import (
	"database/sql"
	"fmt"
	"sort"
)

// computeChannelsFromRollup builds the channels analytics result map from the
// channel_rollup tables. region "" = global; window zero -> default 24h.
func computeChannelsFromRollup(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	eff := rfEffectiveWindow(window)
	sinceHour, untilHour := rfWindowHourBounds(eff)
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	where := "hour >= ? AND hour <= ?"
	args := []interface{}{sinceHour, untilHour}
	if region != "" {
		where += " AND observer_idx IN (" + rfIntPlaceholders(len(idxs)) + ")"
		for _, v := range idxs {
			args = append(args, v)
		}
	}

	type chAgg struct {
		hash, name               string
		msgCount, decryptedCount int
		lastActivity             string
	}
	chRows, err := db.conn.Query(`
		SELECT channel_hash, SUM(msg_count), SUM(decrypted_count),
		       MAX(last_activity), MAX(name)
		FROM channel_rollup WHERE `+where+` GROUP BY channel_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel rollup query: %w", err)
	}
	byHash := map[string]*chAgg{}
	for chRows.Next() {
		a := &chAgg{}
		var name, la sql.NullString
		if err := chRows.Scan(&a.hash, &a.msgCount, &a.decryptedCount, &la, &name); err != nil {
			chRows.Close()
			return nil, fmt.Errorf("channel rollup scan: %w", err)
		}
		a.name = name.String
		a.lastActivity = la.String
		byHash[a.hash] = a
	}
	chRows.Close()

	senderByHash := map[string]int{}
	sndRows, err := db.conn.Query(`
		SELECT channel_hash, COUNT(DISTINCT sender)
		FROM channel_sender_rollup WHERE `+where+` GROUP BY channel_hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel sender count query: %w", err)
	}
	for sndRows.Next() {
		var h string
		var n int
		if err := sndRows.Scan(&h, &n); err != nil {
			sndRows.Close()
			return nil, fmt.Errorf("channel sender count scan: %w", err)
		}
		senderByHash[h] = n
	}
	sndRows.Close()

	topRows, err := db.conn.Query(`
		SELECT sender, SUM(msg_count) AS c
		FROM channel_sender_rollup WHERE `+where+`
		GROUP BY sender ORDER BY c DESC LIMIT 15`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel top-sender query: %w", err)
	}
	topSenders := make([]map[string]interface{}, 0, 15)
	for topRows.Next() {
		var name string
		var c int
		if err := topRows.Scan(&name, &c); err != nil {
			topRows.Close()
			return nil, fmt.Errorf("channel top-sender scan: %w", err)
		}
		topSenders = append(topSenders, map[string]interface{}{"name": name, "count": c})
	}
	topRows.Close()

	exactTx := map[string]int{}
	if region == "" {
		txRows, err := db.conn.Query(`
			SELECT channel_hash, SUM(distinct_tx) FROM channel_rollup_tx
			WHERE hour >= ? AND hour <= ? GROUP BY channel_hash`, sinceHour, untilHour)
		if err != nil {
			return nil, fmt.Errorf("channel rollup_tx query: %w", err)
		}
		for txRows.Next() {
			var h string
			var n int
			if err := txRows.Scan(&h, &n); err != nil {
				txRows.Close()
				return nil, fmt.Errorf("channel rollup_tx scan: %w", err)
			}
			exactTx[h] = n
		}
		txRows.Close()
	}

	tlRows, err := db.conn.Query(`
		SELECT hour, MAX(name), SUM(msg_count)
		FROM channel_rollup WHERE `+where+` GROUP BY hour, channel_hash ORDER BY hour`, args...)
	if err != nil {
		return nil, fmt.Errorf("channel timeline query: %w", err)
	}
	channelTimeline := make([]map[string]interface{}, 0)
	for tlRows.Next() {
		var hr string
		var nm sql.NullString
		var c int
		if err := tlRows.Scan(&hr, &nm, &c); err != nil {
			tlRows.Close()
			return nil, fmt.Errorf("channel timeline scan: %w", err)
		}
		channelTimeline = append(channelTimeline, map[string]interface{}{
			"hour": hr, "channel": nm.String, "count": c,
		})
	}
	tlRows.Close()

	msglenBins := make([]int, chMsgLenBinCount)
	blobRows, err := db.conn.Query(`SELECT msglen_bins FROM channel_rollup WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("channel msglen query: %w", err)
	}
	for blobRows.Next() {
		var b []byte
		if err := blobRows.Scan(&b); err != nil {
			blobRows.Close()
			return nil, fmt.Errorf("channel msglen scan: %w", err)
		}
		rfAddBins(msglenBins, rfUnpackBins(b, chMsgLenBinCount))
	}
	blobRows.Close()

	channels := make([]map[string]interface{}, 0, len(byHash))
	decryptable := 0
	for hash, a := range byHash {
		enc := a.decryptedCount == 0
		if !enc {
			decryptable++
		}
		msgs := a.msgCount
		if region == "" {
			if ex, ok := exactTx[hash]; ok {
				msgs = ex
			}
		}
		name := a.name
		if name == "" {
			name = "ch" + hash
		}
		channels = append(channels, map[string]interface{}{
			"hash": hash, "name": name, "messages": msgs,
			"senders": senderByHash[hash], "lastActivity": a.lastActivity,
			"encrypted": enc,
		})
	}
	sort.Slice(channels, func(i, j int) bool {
		return channels[i]["messages"].(int) > channels[j]["messages"].(int)
	})

	return map[string]interface{}{
		"activeChannels":  len(channels),
		"decryptable":     decryptable,
		"channels":        channels,
		"topSenders":      topSenders,
		"channelTimeline": channelTimeline,
		"msgLengths":      rfHistogramFromBins(msglenBins, chMsgLenBinMin, chMsgLenBinWidth),
	}, nil
}
