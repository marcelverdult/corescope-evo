// distance_rollup.go — distance analytics rollup: schema, hop helpers,
// single-hour recompute. See .specs/2026-05-20-distance-rollup-design.md.
// Mirrors rf_rollup.go / node_rollup.go.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Fixed-bin distance histogram: 0..300 km, 12 km width = 25 bins. Values
// outside the range clamp to the end bins. Matches the in-memory
// computeAnalyticsDistance 25-bin count, with fixed (not dynamic) edges.
const (
	distDistBinMin, distDistBinWidth, distDistBinCount = 0, 12, 25
	distSnrBinMin, distSnrBinWidth, distSnrBinCount    = -30, 1, 50
)

// distNode is the per-pubkey lookup the recompute uses to resolve hops to
// GPS-bearing nodes.
type distNode struct {
	Name   string
	Role   string
	Lat    float64
	Lon    float64
	HasGPS bool
}

// distanceLoadNodeMap fetches all nodes with role + GPS, keyed by pubkey.
// The recompute calls this once per hour, not per tx.
func distanceLoadNodeMap(db *sql.DB) (map[string]*distNode, error) {
	rows, err := db.Query(`SELECT public_key, name, role, lat, lon FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("distance load nodes: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*distNode, 1024)
	for rows.Next() {
		var pk string
		var name, role sql.NullString
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&pk, &name, &role, &lat, &lon); err != nil {
			return nil, fmt.Errorf("distance load nodes scan: %w", err)
		}
		n := &distNode{Name: name.String, Role: role.String}
		if lat.Valid && lon.Valid {
			n.Lat = lat.Float64
			n.Lon = lon.Float64
			n.HasGPS = true
		}
		out[pk] = n
	}
	return out, rows.Err()
}

// distanceHopChain reconstructs the chain of GPS-bearing nodes for one
// observation: sender (if GPS-known) followed by every positional hop that
// resolves to a GPS-known node. Returns the chain in path order. A chain of
// fewer than 2 nodes yields no haversine pairs; callers must check.
func distanceHopChain(pathJSON, resolvedPath, senderPk string, nodeByPk map[string]*distNode) []*distNode {
	raw := parsePathJSON(pathJSON)
	if len(raw) == 0 {
		return nil
	}
	var resolved []*string
	if resolvedPath != "" {
		_ = json.Unmarshal([]byte(resolvedPath), &resolved)
	}
	chain := make([]*distNode, 0, len(raw)+1)
	if s, ok := nodeByPk[senderPk]; ok && s != nil && s.HasGPS {
		chain = append(chain, s)
	}
	for i, rawHop := range raw {
		pk := strings.ToLower(rawHop)
		if i < len(resolved) && resolved[i] != nil && *resolved[i] != "" {
			pk = strings.ToLower(*resolved[i])
		}
		if n, ok := nodeByPk[pk]; ok && n != nil && n.HasGPS {
			chain = append(chain, n)
		}
	}
	return chain
}

// distanceClassify returns "R↔R" / "C↔R" / "C↔C" from two roles. The rule
// is: role contains "repeater" (case-insensitive) → R, else C.
func distanceClassify(roleA, roleB string) string {
	aRep := strings.Contains(strings.ToLower(roleA), "repeater")
	bRep := strings.Contains(strings.ToLower(roleB), "repeater")
	switch {
	case aRep && bRep:
		return "R↔R"
	case !aRep && !bRep:
		return "C↔C"
	default:
		return "C↔R"
	}
}

// distancePairKey returns the unordered-pair key "<min>|<max>".
func distancePairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// ensureDistanceRollupTable creates the distance rollup tables. Idempotent.
func ensureDistanceRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for distance_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS distance_hourly (
			hour TEXT NOT NULL,
			type TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			dist_sum REAL NOT NULL DEFAULT 0,
			dist_min REAL,
			dist_max REAL,
			dist_bins BLOB,
			PRIMARY KEY (hour, type, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_hourly_hour ON distance_hourly(hour)`,
		`CREATE TABLE IF NOT EXISTS distance_pair_hourly (
			hour TEXT NOT NULL,
			pair_key TEXT NOT NULL,
			type TEXT NOT NULL,
			observer_idx INTEGER NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			best_dist REAL NOT NULL DEFAULT 0,
			best_from_name TEXT,
			best_from_pk TEXT,
			best_to_name TEXT,
			best_to_pk TEXT,
			best_hash TEXT,
			best_timestamp TEXT,
			snr_max REAL,
			snr_bins BLOB,
			PRIMARY KEY (hour, pair_key, type, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_pair_hourly_hour ON distance_pair_hourly(hour)`,
		`CREATE TABLE IF NOT EXISTS distance_paths (
			hour TEXT NOT NULL,
			tx_id INTEGER PRIMARY KEY,
			total_dist REAL NOT NULL DEFAULT 0,
			hop_count INTEGER NOT NULL DEFAULT 0,
			hash TEXT,
			timestamp TEXT,
			hops_json TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_paths_hour_dist ON distance_paths(hour, total_dist DESC)`,
		`CREATE TABLE IF NOT EXISTS distance_path_observers (
			tx_id INTEGER NOT NULL,
			observer_idx INTEGER NOT NULL,
			PRIMARY KEY (tx_id, observer_idx)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_distance_path_observers_obs ON distance_path_observers(observer_idx)`,
		`CREATE TABLE IF NOT EXISTS distance_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("distance_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}

// distHopCell accumulates one (type, observer_idx) cell while scanning a
// single hour.
type distHopCell struct {
	count            int
	distSum          float64
	distMin, distMax float64
	haveDist         bool
	distBins         []int
}

func newDistHopCell() *distHopCell {
	return &distHopCell{distBins: make([]int, distDistBinCount)}
}

// distPairCell accumulates one (pair_key, type, observer_idx) cell.
type distPairCell struct {
	count         int
	bestDist      float64
	bestFromName  string
	bestFromPk    string
	bestToName    string
	bestToPk      string
	bestHash      string
	bestTimestamp string
	snrMax        float64
	haveSnr       bool
	snrBins       []int
}

func newDistPairCell() *distPairCell {
	return &distPairCell{snrBins: make([]int, distSnrBinCount)}
}

// distPathRow is one prospective distance_paths row built during recompute.
type distPathRow struct {
	totalDist float64
	hopCount  int
	hash      string
	timestamp string
	hopsJSON  string
	observers map[int]bool
}

// recomputeDistanceRollupHour rebuilds all four distance rollup tables for
// one hour bucket ("2026-05-18T10") from raw transmissions + observations +
// the nodes GPS map. Idempotent: deletes then re-inserts. The raw read runs
// OUTSIDE the write transaction and filters on the indexed first_seen
// RFC3339 range.
func recomputeDistanceRollupHour(rw *sql.DB, hour string) error {
	ht, err := time.Parse("2006-01-02T15", hour)
	if err != nil {
		return fmt.Errorf("distance recompute parse hour %q: %w", hour, err)
	}
	lo := ht.UTC().Format("2006-01-02T15:04:05Z")
	hi := ht.UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")

	nodeByPk, err := distanceLoadNodeMap(rw)
	if err != nil {
		return err
	}

	rows, err := rw.Query(`
		SELECT t.id, t.first_seen, t.hash, t.decoded_json,
		       o.id, o.observer_idx, o.path_json, o.resolved_path, o.snr
		FROM transmissions t
		JOIN observations o ON o.transmission_id = t.id
		WHERE t.first_seen >= ? AND t.first_seen < ?
		ORDER BY t.id, o.id`, lo, hi)
	if err != nil {
		return fmt.Errorf("distance recompute scan: %w", err)
	}

	// Per-tx accumulators (grouped by tx.id via ORDER BY).
	type txState struct {
		firstSeen string
		hash      string
		senderPk  string
		repPath   string
		repResolv string
		havePath  bool
		observers map[int]bool
		snrSeen   bool
		snrMax    float64
	}
	txs := map[int]*txState{}
	for rows.Next() {
		var txID int
		var firstSeen, hash string
		var decoded sql.NullString
		var obsID sql.NullInt64
		var obsIdx sql.NullInt64
		var pathJSON, resolvedPath sql.NullString
		var snr sql.NullFloat64
		if err := rows.Scan(&txID, &firstSeen, &hash, &decoded,
			&obsID, &obsIdx, &pathJSON, &resolvedPath, &snr); err != nil {
			rows.Close()
			return fmt.Errorf("distance recompute row: %w", err)
		}
		st := txs[txID]
		if st == nil {
			st = &txState{
				firstSeen: firstSeen,
				hash:      hash,
				observers: map[int]bool{},
			}
			if decoded.Valid {
				var d map[string]interface{}
				if json.Unmarshal([]byte(decoded.String), &d) == nil {
					if pk, ok := d["pubKey"].(string); ok {
						st.senderPk = strings.ToLower(pk)
					}
				}
			}
			txs[txID] = st
		}
		if obsIdx.Valid {
			st.observers[int(obsIdx.Int64)] = true
		}
		if !st.havePath && pathJSON.Valid && pathJSON.String != "" && pathJSON.String != "[]" {
			st.repPath = pathJSON.String
			if resolvedPath.Valid {
				st.repResolv = resolvedPath.String
			}
			st.havePath = true
		}
		if snr.Valid {
			if !st.snrSeen || snr.Float64 > st.snrMax {
				st.snrMax = snr.Float64
				st.snrSeen = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("distance recompute rows: %w", err)
	}

	// Aggregate.
	hourCells := map[[2]int]*distHopCell{}      // (typeIdx, observer_idx) — typeIdx via distTypeIndex
	pairCellByKey := map[string]*distPairCell{} // composite key "pk_a|pk_b|type|oi"
	pathRows := map[int]*distPathRow{}

	addHop := func(typ string, oi int, dist float64) {
		key := [2]int{distTypeIndex(typ), oi}
		c := hourCells[key]
		if c == nil {
			c = newDistHopCell()
			hourCells[key] = c
		}
		c.count++
		c.distSum += dist
		if !c.haveDist || dist < c.distMin {
			c.distMin = dist
		}
		if !c.haveDist || dist > c.distMax {
			c.distMax = dist
		}
		c.haveDist = true
		c.distBins[rfBinIndex(int(dist), distDistBinMin, distDistBinWidth, distDistBinCount)]++
	}

	addPair := func(pairKey, typ string, oi int, dist float64,
		fromName, fromPk, toName, toPk, hash, ts string, snr float64, haveSnr bool) {
		k := fmt.Sprintf("%s|%s|%d", pairKey, typ, oi)
		c := pairCellByKey[k]
		if c == nil {
			c = newDistPairCell()
			pairCellByKey[k] = c
		}
		c.count++
		if dist > c.bestDist {
			c.bestDist = dist
			c.bestFromName = fromName
			c.bestFromPk = fromPk
			c.bestToName = toName
			c.bestToPk = toPk
			c.bestHash = hash
			c.bestTimestamp = ts
		}
		if haveSnr {
			if !c.haveSnr || snr > c.snrMax {
				c.snrMax = snr
				c.haveSnr = true
			}
			c.snrBins[rfBinIndex(int(snr), distSnrBinMin, distSnrBinWidth, distSnrBinCount)]++
		}
	}

	for txID, st := range txs {
		if !st.havePath {
			continue
		}
		chain := distanceHopChain(st.repPath, st.repResolv, st.senderPk, nodeByPk)
		if len(chain) < 2 {
			continue
		}
		type hopDetail struct {
			FromName string  `json:"fromName"`
			FromPk   string  `json:"fromPk"`
			ToName   string  `json:"toName"`
			ToPk     string  `json:"toPk"`
			Dist     float64 `json:"dist"`
		}
		var hops []hopDetail
		totalDist := 0.0
		for i := 0; i < len(chain)-1; i++ {
			a, b := chain[i], chain[i+1]
			dist := haversineKm(a.Lat, a.Lon, b.Lat, b.Lon)
			if dist > 300 {
				continue
			}
			roundedDist := math.Round(dist*100) / 100
			typ := distanceClassify(a.Role, b.Role)
			fromPk, toPk := "", ""
			for pk, nd := range nodeByPk {
				if nd == a && fromPk == "" {
					fromPk = pk
				}
				if nd == b && toPk == "" {
					toPk = pk
				}
				if fromPk != "" && toPk != "" {
					break
				}
			}
			pairKey := distancePairKey(fromPk, toPk)

			// Global (-1) cell and per-observer cells.
			addHop(typ, -1, roundedDist)
			addPair(pairKey, typ, -1, roundedDist, a.Name, fromPk, b.Name, toPk,
				st.hash, st.firstSeen, st.snrMax, st.snrSeen)
			for oi := range st.observers {
				addHop(typ, oi, roundedDist)
				addPair(pairKey, typ, oi, roundedDist, a.Name, fromPk, b.Name, toPk,
					st.hash, st.firstSeen, st.snrMax, st.snrSeen)
			}

			hops = append(hops, hopDetail{
				FromName: a.Name, FromPk: fromPk,
				ToName: b.Name, ToPk: toPk,
				Dist: roundedDist,
			})
			totalDist += dist
		}
		if len(hops) == 0 {
			continue
		}
		hopsJSON, _ := json.Marshal(hops)
		pathRows[txID] = &distPathRow{
			totalDist: math.Round(totalDist*100) / 100,
			hopCount:  len(hops),
			hash:      st.hash,
			timestamp: st.firstSeen,
			hopsJSON:  string(hopsJSON),
			observers: st.observers,
		}
	}

	// Write transaction.
	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("distance recompute begin: %w", err)
	}
	defer tx.Rollback()
	// Delete any existing rows for this hour.
	if _, err := tx.Exec(`DELETE FROM distance_hourly WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("distance recompute delete hourly: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM distance_pair_hourly WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("distance recompute delete pair_hourly: %w", err)
	}
	// Delete distance_path_observers BEFORE distance_paths so we can resolve tx_ids.
	if _, err := tx.Exec(`DELETE FROM distance_path_observers
		WHERE tx_id IN (SELECT tx_id FROM distance_paths WHERE hour=?)`, hour); err != nil {
		return fmt.Errorf("distance recompute delete path_observers: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM distance_paths WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("distance recompute delete paths: %w", err)
	}
	for key, c := range hourCells {
		typ := distTypeName(key[0])
		var mn, mx interface{}
		if c.haveDist {
			mn, mx = c.distMin, c.distMax
		}
		if _, err := tx.Exec(`INSERT INTO distance_hourly
			(hour,type,observer_idx,count,dist_sum,dist_min,dist_max,dist_bins)
			VALUES (?,?,?,?,?,?,?,?)`,
			hour, typ, key[1], c.count, c.distSum, mn, mx, rfPackBins(c.distBins)); err != nil {
			return fmt.Errorf("distance recompute insert hourly: %w", err)
		}
	}
	for k, c := range pairCellByKey {
		// k = pairKey|type|oi — parse last two segments by splitting on the LAST two pipes.
		parts := strings.SplitN(k, "|", 4)
		if len(parts) != 4 {
			return fmt.Errorf("distance recompute bad pair key %q", k)
		}
		pairKey := parts[0] + "|" + parts[1]
		typ := parts[2]
		var oi int
		fmt.Sscan(parts[3], &oi)
		var snrMax interface{}
		if c.haveSnr {
			snrMax = c.snrMax
		}
		if _, err := tx.Exec(`INSERT INTO distance_pair_hourly
			(hour,pair_key,type,observer_idx,count,best_dist,best_from_name,best_from_pk,
			 best_to_name,best_to_pk,best_hash,best_timestamp,snr_max,snr_bins)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			hour, pairKey, typ, oi, c.count, c.bestDist,
			c.bestFromName, c.bestFromPk, c.bestToName, c.bestToPk,
			c.bestHash, c.bestTimestamp, snrMax, rfPackBins(c.snrBins)); err != nil {
			return fmt.Errorf("distance recompute insert pair_hourly: %w", err)
		}
	}
	for txID, p := range pathRows {
		if _, err := tx.Exec(`INSERT INTO distance_paths
			(hour,tx_id,total_dist,hop_count,hash,timestamp,hops_json)
			VALUES (?,?,?,?,?,?,?)`,
			hour, txID, p.totalDist, p.hopCount, p.hash, p.timestamp, p.hopsJSON); err != nil {
			return fmt.Errorf("distance recompute insert paths: %w", err)
		}
		for oi := range p.observers {
			if _, err := tx.Exec(`INSERT INTO distance_path_observers(tx_id,observer_idx)
				VALUES (?,?)`, txID, oi); err != nil {
				return fmt.Errorf("distance recompute insert path_observers: %w", err)
			}
		}
	}
	return tx.Commit()
}

// distTypeIndex maps type strings to dense ints for map keys.
func distTypeIndex(t string) int {
	switch t {
	case "R↔R":
		return 0
	case "C↔R":
		return 1
	case "C↔C":
		return 2
	}
	return 3
}

func distTypeName(i int) string {
	switch i {
	case 0:
		return "R↔R"
	case 1:
		return "C↔R"
	case 2:
		return "C↔C"
	}
	return "?"
}
