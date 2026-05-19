// node_rollup_read.go — node-health relay/usefulness assembly from the
// node_rollup tables, plus the cache/flag wrapper used by handleNodes.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// nodeRelayResult is the rollup-derived relay info + usefulness for one node.
type nodeRelayResult struct {
	Relay      RepeaterRelayInfo
	Usefulness float64
}

// computeNodeRelayFromRollup returns, per pubkey, relay activity + usefulness
// read from node_rollup. Usefulness is over a trailing 7-day window.
func computeNodeRelayFromRollup(db *DB, pubkeys []string, relayWindowHours float64) (map[string]nodeRelayResult, error) {
	out := make(map[string]nodeRelayResult, len(pubkeys))
	if len(pubkeys) == 0 {
		return out, nil
	}
	now := time.Now().UTC()
	hour7d := now.Add(-7 * 24 * time.Hour).Format("2006-01-02T15")
	hour24h := now.Add(-24 * time.Hour).Format("2006-01-02T15")
	hour1h := now.Add(-1 * time.Hour).Format("2006-01-02T15")

	type keyPair struct{ self, prefix string }
	pairs := make(map[string]keyPair, len(pubkeys))
	keySet := map[string]bool{}
	for _, pk := range pubkeys {
		lk := strings.ToLower(pk)
		if lk == "" {
			continue
		}
		kp := keyPair{self: lk}
		if len(lk) >= 2 {
			kp.prefix = lk[:2]
		}
		pairs[pk] = kp
		keySet[lk] = true
		if kp.prefix != "" && kp.prefix != lk {
			keySet[kp.prefix] = true
		}
	}
	if len(keySet) == 0 {
		return out, nil
	}
	keys := make([]interface{}, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}

	rows, err := db.conn.Query(`
		SELECT hop_key, hour, relay_count, last_relayed FROM node_rollup
		WHERE hour >= ? AND hop_key IN (`+rfIntPlaceholders(len(keys))+`)`,
		append([]interface{}{hour7d}, keys...)...)
	if err != nil {
		return nil, fmt.Errorf("node rollup read: %w", err)
	}
	type hopAgg struct {
		c1h, c24h, c7d int
		lastRelayed    string
	}
	byKey := map[string]*hopAgg{}
	for rows.Next() {
		var hk, hr, lr sql.NullString
		var rc int
		if err := rows.Scan(&hk, &hr, &rc, &lr); err != nil {
			rows.Close()
			return nil, fmt.Errorf("node rollup read scan: %w", err)
		}
		a := byKey[hk.String]
		if a == nil {
			a = &hopAgg{}
			byKey[hk.String] = a
		}
		a.c7d += rc
		if hr.String >= hour24h {
			a.c24h += rc
		}
		if hr.String >= hour1h {
			a.c1h += rc
		}
		if lr.String > a.lastRelayed {
			a.lastRelayed = lr.String
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("node rollup read rows: %w", err)
	}

	var denom int
	if err := db.conn.QueryRow(
		`SELECT COALESCE(SUM(n_nonadvert),0) FROM node_rollup_total WHERE hour >= ?`,
		hour7d).Scan(&denom); err != nil {
		return nil, fmt.Errorf("node rollup denom: %w", err)
	}

	for pk, kp := range pairs {
		var c1h, c24h, c7d int
		var lastRelayed string
		for _, k := range []string{kp.self, kp.prefix} {
			if k == "" {
				continue
			}
			a := byKey[k]
			if a == nil {
				continue
			}
			c1h += a.c1h
			c24h += a.c24h
			c7d += a.c7d
			if a.lastRelayed > lastRelayed {
				lastRelayed = a.lastRelayed
			}
		}
		info := RepeaterRelayInfo{
			WindowHours:   relayWindowHours,
			RelayCount1h:  c1h,
			RelayCount24h: c24h,
			LastRelayed:   lastRelayed,
		}
		if lastRelayed != "" && relayWindowHours > 0 {
			if ts, ok := parseRelayTS(lastRelayed); ok {
				if ts.After(now.Add(-time.Duration(relayWindowHours * float64(time.Hour)))) {
					info.RelayActive = true
				}
			}
		}
		use := 0.0
		if denom > 0 {
			use = float64(c7d) / float64(denom)
			if use > 1 {
				use = 1
			}
			if use < 0 {
				use = 0
			}
		}
		out[pk] = nodeRelayResult{Relay: info, Usefulness: use}
	}
	return out, nil
}

// GetBulkNodeRelay returns relay activity + usefulness for many pubkeys at
// once. Uses the node_rollup table when the flag is on and the rollup is
// ready; otherwise falls back to the per-node in-memory computation.
func (s *PacketStore) GetBulkNodeRelay(pubkeys []string, relayWindowHours float64) map[string]nodeRelayResult {
	if s.analyticsSQLBackend && s.db != nil && nodeRollupReady(s.db.conn) {
		if r, err := computeNodeRelayFromRollup(s.db, pubkeys, relayWindowHours); err == nil {
			return r
		} else {
			log.Printf("[node-rollup] read failed, falling back to in-memory: %v", err)
		}
	}
	out := make(map[string]nodeRelayResult, len(pubkeys))
	for _, pk := range pubkeys {
		out[pk] = nodeRelayResult{
			Relay:      s.GetRepeaterRelayInfo(pk, relayWindowHours),
			Usefulness: s.GetRepeaterUsefulnessScore(pk),
		}
	}
	return out
}
