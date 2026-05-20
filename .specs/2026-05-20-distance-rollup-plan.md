# Distance Analytics Rollup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pre-aggregate distance analytics into SQL rollup tables so `/api/analytics/distance` serves any window over the full DB, flat-fast.

**Architecture:** Three rollup tables (`distance_hourly` per-type, `distance_pair_hourly` per node-pair best, `distance_paths` per-tx) + `distance_path_observers` mapping + meta watermark. Per-observer key with `-1` sentinel for global (deduped). Adds `TimeWindow` support. In-memory `computeAnalyticsDistance` stays as fallback + parity. Mirrors `node_rollup*.go` / `rf_rollup*.go`.

**Tech Stack:** Go, SQLite (`database/sql`), per-directory Go modules.

---

## Conventions for every task

- All Go commands run from `cmd/server/`: `cd cmd/server && go test ...`.
- After editing any `.go` file, run `gofmt -l <file>` from `cmd/server/`. Empty output = clean. Do NOT `gofmt -w` pre-existing files (some are not currently gofmt-clean; only your changes must be).
- `git` commands run from the worktree root `/Users/verdi/Projects/corescope/corescope-evo/.claude/worktrees/distance-rollup`.
- Commits are signed (1Password). If a commit fails with a signing/vault error, STOP and report BLOCKED — do not bypass signing.
- **Go imports**: Go rejects unused AND undeclared imports. Each task adds exactly the imports its new code uses — when a step's code first needs a package, the step says to add it.
- Test helpers already in the package: `setupTestDBFile(t)` → returns a `*DB` with `.path` and `.conn` (v3 schema); `mustExec(t, db, sql)`; `cachedRW(path)` → `*sql.DB`; `loadStore(t, path, retentionHours)` → `*PacketStore`.
- Helpers to reuse from earlier rollups: `rfBinIndex`, `rfPackBins`, `rfUnpackBins`, `rfAddBins`, `rfHistogramFromBins`, `rfEffectiveWindow`, `rfWindowHourBounds`, `rfIntPlaceholders`, `rfRegionObserverIdxs`. `haversineKm` exists in `cmd/server/store_distance.go`. `parsePathJSON` exists in `cmd/server/store.go:4217`.
- Fixture facts: `observations.timestamp` is epoch seconds (`1779098400 = 2026-05-18T10:00:00Z`); `transmissions.first_seen` is RFC3339 text; `nodes` table has `(public_key, name, role, lat REAL, lon REAL, …)`.

---

## Task 1: schema

**Files:**
- Create: `cmd/server/distance_rollup.go`
- Test: `cmd/server/distance_rollup_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/server/distance_rollup_test.go`:

```go
package main

import (
	"testing"
)

func TestEnsureDistanceRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatalf("ensureDistanceRollupTable: %v", err)
	}
	for _, tbl := range []string{
		"distance_hourly", "distance_pair_hourly", "distance_paths",
		"distance_path_observers", "distance_rollup_meta",
	} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

`cd cmd/server && go test -run TestEnsureDistanceRollupTable -v` → expect FAIL with `undefined: ensureDistanceRollupTable`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/distance_rollup.go`:

```go
// distance_rollup.go — distance analytics rollup: schema, hop helpers,
// single-hour recompute. See .specs/2026-05-20-distance-rollup-design.md.
// Mirrors rf_rollup.go / node_rollup.go.

package main

import (
	"fmt"
)

// Fixed-bin distance histogram: 0..300 km, 12 km width = 25 bins. Values
// outside the range clamp to the end bins. Matches the in-memory
// computeAnalyticsDistance 25-bin count, with fixed (not dynamic) edges.
const (
	distDistBinMin, distDistBinWidth, distDistBinCount = 0, 12, 25
	distSnrBinMin, distSnrBinWidth, distSnrBinCount    = -30, 1, 50
)

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
```

- [ ] **Step 4: Run test to verify it passes**

`cd cmd/server && go test -run TestEnsureDistanceRollupTable -v` → expect PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git commit -m "feat(distance-rollup): schema for distance rollup tables"
```

---

## Task 2: hop-chain helper

**Files:**
- Modify: `cmd/server/distance_rollup.go` (append types + `distanceHopChain`, `distanceLoadNodeMap`)
- Test: `cmd/server/distance_rollup_test.go` (append)

A small per-tx helper that turns one observation's `path_json` + `resolved_path` + sender pubkey + node map into the **chain of GPS-bearing nodes** for haversine pairing. Mirrors the in-memory `computeDistancesForTx` chain construction.

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/distance_rollup_test.go`:

```go
func TestDistanceHopChain(t *testing.T) {
	nodes := map[string]*distNode{
		"aa": {Name: "A", Role: "repeater", Lat: 52.0, Lon: 4.0, HasGPS: true},
		"bb": {Name: "B", Role: "repeater", Lat: 53.0, Lon: 5.0, HasGPS: true},
		"cc": {Name: "C", Role: "client", Lat: 54.0, Lon: 6.0, HasGPS: true},
		"dd": {Name: "D", Role: "client", HasGPS: false}, // no GPS, skipped
	}
	cases := []struct {
		name, path, resolved, sender string
		want                         []string // names in chain order
	}{
		{"sender + 2 hops", `["bb","cc"]`, ``, "aa", []string{"A", "B", "C"}},
		{"resolved overrides raw", `["AA","BB"]`, `["aa",null]`, "", []string{"A", "B"}},
		{"no-GPS hop skipped", `["dd","cc"]`, ``, "aa", []string{"A", "C"}},
		{"unknown sender ignored", `["aa","bb"]`, ``, "zz", []string{"A", "B"}},
		{"empty path", `[]`, ``, "aa", nil},
		{"single GPS node", `[]`, ``, "aa", nil}, // < 2 nodes → caller drops
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chain := distanceHopChain(c.path, c.resolved, c.sender, nodes)
			got := make([]string, len(chain))
			for i, n := range chain {
				got[i] = n.Name
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

`cd cmd/server && go test -run TestDistanceHopChain -v` → expect FAIL (`undefined: distanceHopChain` / `distNode`).

- [ ] **Step 3: Write minimal implementation**

First, update the `import` block of `cmd/server/distance_rollup.go` to add `database/sql`, `encoding/json`, `strings`:

```go
import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)
```

Then append to `cmd/server/distance_rollup.go`:

```go
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
```

(`parsePathJSON` already exists in `cmd/server/store.go:4217`.)

- [ ] **Step 4: Run test to verify it passes**

`cd cmd/server && go test -run TestDistanceHopChain -v` → expect PASS (6 subtests).

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git commit -m "feat(distance-rollup): hop-chain reconstruction + node map"
```

---

## Task 3: single-hour recompute

**Files:**
- Modify: `cmd/server/distance_rollup.go` (append `recomputeDistanceRollupHour`)
- Test: `cmd/server/distance_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test**

First, update the `import` block of `cmd/server/distance_rollup_test.go` to add `time`:

```go
import (
	"testing"
	"time"
)
```

Then append to `cmd/server/distance_rollup_test.go`:

```go
func TestRecomputeDistanceRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// 3 GPS-bearing nodes: A (repeater) → B (repeater) → C (client).
	// Sender is A; path is [B, C]. Distances roughly:
	//   A(52,4) → B(53,5)  ≈ 127 km   R↔R
	//   B(53,5) → C(54,6)  ≈ 124 km   C↔R
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0),
		('cc','C','client',54.0,6.0)`)
	// Sender pubkey 'aa' encoded in decoded_json's pubKey field.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr) VALUES
		(1,7,1779098400,'["bb","cc"]',5.0)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeDistanceRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	// Global row (observer_idx=-1) should have 2 hops total.
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&n)
	if n != 2 {
		t.Fatalf("global hop count=%d want 2", n)
	}
	// Per-observer row (oi=7) should also have 2 hops.
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=7`,
		"2026-05-18T10").Scan(&n)
	if n != 2 {
		t.Fatalf("per-observer hop count=%d want 2", n)
	}
	// Per-type breakdown on the global row: 1 R↔R + 1 C↔R.
	var rr, cr int
	rw.QueryRow(`SELECT COALESCE(count,0) FROM distance_hourly WHERE hour=? AND observer_idx=-1 AND type='R↔R'`,
		"2026-05-18T10").Scan(&rr)
	rw.QueryRow(`SELECT COALESCE(count,0) FROM distance_hourly WHERE hour=? AND observer_idx=-1 AND type='C↔R'`,
		"2026-05-18T10").Scan(&cr)
	if rr != 1 || cr != 1 {
		t.Fatalf("R↔R=%d C↔R=%d want 1 and 1", rr, cr)
	}
	// distance_paths: one row with total_dist ≈ 127+124 = ~251 km.
	var total float64
	var hopCount int
	rw.QueryRow(`SELECT total_dist, hop_count FROM distance_paths WHERE tx_id=1`).Scan(&total, &hopCount)
	if hopCount != 2 {
		t.Fatalf("hop_count=%d want 2", hopCount)
	}
	if total < 240 || total > 260 {
		t.Fatalf("total_dist=%.2f want ~251 km", total)
	}
	// distance_path_observers: one row (tx_id=1, observer_idx=7).
	var oi int
	rw.QueryRow(`SELECT observer_idx FROM distance_path_observers WHERE tx_id=1`).Scan(&oi)
	if oi != 7 {
		t.Fatalf("path_observers oi=%d want 7", oi)
	}
	// Idempotent: second run yields same numbers.
	if err := recomputeDistanceRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE hour=? AND observer_idx=-1`,
		"2026-05-18T10").Scan(&n)
	if n != 2 {
		t.Fatalf("after rerun global=%d want 2", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

`cd cmd/server && go test -run TestRecomputeDistanceRollupHour -v` → expect FAIL with `undefined: recomputeDistanceRollupHour`.

- [ ] **Step 3: Write minimal implementation**

First, update the `import` block of `cmd/server/distance_rollup.go` to add `math` and `time`:

```go
import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)
```

Then append to `cmd/server/distance_rollup.go`:

```go
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
		firstSeen  string
		hash       string
		senderPk   string
		repPath    string
		repResolv  string
		havePath   bool
		observers  map[int]bool
		snrSeen    bool
		snrMax     float64
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
```

Notes on edge handling:
- The composite pair-cell key uses `|` as separator; `pair_key` itself contains a `|` (it is `pk_a|pk_b`), so `strings.SplitN(k, "|", 4)` recovers the four segments cleanly: `[pk_a, pk_b, type, oi]`.
- `haversineKm` is already in `cmd/server/store_distance.go`.
- Pair name resolution iterates `nodeByPk` to find the pubkey behind a `*distNode`. This is O(N) per hop but N is bounded by node count — acceptable. If pairs become a hotspot, swap to a reverse map keyed by `*distNode` pointer.
- The `_ = pairCells` line documents that `pairCells` exists only because Go would otherwise reject the unused-variable; in practice it stays empty and `pairCellByKey` does the real work.

- [ ] **Step 4: Run test to verify it passes**

`cd cmd/server && go test -run 'TestEnsureDistanceRollupTable|TestDistanceHopChain|TestRecomputeDistanceRollupHour' -v` → all PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup.go cmd/server/distance_rollup_test.go
git commit -m "feat(distance-rollup): single-hour recompute"
```

---

## Task 4: backfill + maintenance + watermark

**Files:**
- Create: `cmd/server/distance_rollup_maintain.go`
- Test: `cmd/server/distance_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/distance_rollup_test.go`:

```go
func TestDistanceRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0)`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,1779098400,'["bb"]',5.0)`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if distanceRollupReady(rw) {
		t.Fatal("rollup should not be ready before first run")
	}
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	if !distanceRollupReady(rw) {
		t.Fatal("rollup should be ready after first run")
	}
	var n int
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE observer_idx=-1`).Scan(&n)
	if n != 1 {
		t.Fatalf("after run 1 hop count=%d want 1", n)
	}
	// Second transmission in the same hour with a new path.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (2,'bb','h2','2026-05-18T10:20:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (2,1,1779099600,'["bb"]',6.0)`)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(SUM(count),0) FROM distance_hourly WHERE observer_idx=-1`).Scan(&n)
	if n != 2 {
		t.Fatalf("after run 2 hop count=%d want 2", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

`cd cmd/server && go test -run TestDistanceRollupMaintenance -v` → expect FAIL with `undefined: distanceRollupReady` / `runDistanceRollupMaintenance`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/distance_rollup_maintain.go`:

```go
// distance_rollup_maintain.go — distance rollup backfill + incremental
// maintenance. Mirrors node_rollup_maintain.go / rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const distanceRollupWatermarkKey = "distance_rollup_last_obs_id"

func distanceRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM distance_rollup_meta WHERE key=?`,
		distanceRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read distance watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func distanceSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO distance_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		distanceRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set distance watermark: %w", err)
	}
	return nil
}

var distanceRollupMaintMu sync.Mutex

func runDistanceRollupMaintenanceGuarded(rw *sql.DB) {
	if !distanceRollupMaintMu.TryLock() {
		log.Printf("[distance-rollup] maintenance skipped — run already in progress")
		return
	}
	defer distanceRollupMaintMu.Unlock()
	if err := runDistanceRollupMaintenance(rw); err != nil {
		log.Printf("[distance-rollup] maintenance: %v", err)
	}
}

// runDistanceRollupMaintenance recomputes every hour bucket whose
// transmissions were touched by observations newer than the watermark, then
// advances it. Watermark on observations.id — a new observation on an old
// transmission can change its observer coverage or its representative path.
func runDistanceRollupMaintenance(rw *sql.DB) error {
	wm, err := distanceRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE o.id > ?`, wm)
	if err != nil {
		return fmt.Errorf("distance maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("distance maintenance scan hour: %w", err)
		}
		if h.Valid && h.String != "" {
			hours = append(hours, h.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("distance maintenance touched-hours err: %w", err)
	}
	for _, h := range hours {
		if err := recomputeDistanceRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("distance maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return distanceSetRollupWatermark(rw, maxID.Int64)
	}
	if !maxID.Valid {
		return distanceSetRollupWatermark(rw, 0)
	}
	return nil
}

func backfillDistanceRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[distance-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[distance-rollup] backfill open rw: %v", err)
		return
	}
	distanceRollupMaintMu.Lock()
	defer distanceRollupMaintMu.Unlock()
	start := time.Now()
	if err := runDistanceRollupMaintenance(rw); err != nil {
		log.Printf("[distance-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[distance-rollup] backfill complete in %s", time.Since(start))
}

func distanceRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM distance_rollup_meta WHERE key=?`,
		distanceRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
```

- [ ] **Step 4: Run test to verify it passes**

`cd cmd/server && go test -run TestDistanceRollupMaintenance -v` → expect PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/distance_rollup_maintain.go cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup_maintain.go cmd/server/distance_rollup_test.go
git commit -m "feat(distance-rollup): backfill, watermark, maintenance"
```

---

## Task 5: read path

**Files:**
- Create: `cmd/server/distance_rollup_read.go`
- Test: `cmd/server/distance_rollup_test.go` (append)

`computeAnalyticsDistanceFromRollup` builds the full response map (same shape as `computeAnalyticsDistance`) from the four rollup tables.

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/distance_rollup_test.go`:

```go
func TestComputeAnalyticsDistanceFromRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0)`)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1,'{"pubKey":"aa"}')`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,1779098400,'["bb"]',5.0)`)
	rw, _ := cachedRW(db.path)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}
	win := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}
	res, err := computeAnalyticsDistanceFromRollup(db, "", win)
	if err != nil {
		t.Fatalf("computeAnalyticsDistanceFromRollup: %v", err)
	}
	for _, k := range []string{"summary", "topHops", "topPaths", "catStats", "distHistogram", "distOverTime"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	sum := res["summary"].(map[string]interface{})
	if sum["totalHops"].(int) != 1 {
		t.Errorf("totalHops=%v want 1", sum["totalHops"])
	}
	if sum["totalPaths"].(int) != 1 {
		t.Errorf("totalPaths=%v want 1", sum["totalPaths"])
	}
	tops := res["topHops"].([]map[string]interface{})
	if len(tops) != 1 {
		t.Errorf("topHops len=%d want 1", len(tops))
	}
	paths := res["topPaths"].([]map[string]interface{})
	if len(paths) != 1 {
		t.Errorf("topPaths len=%d want 1", len(paths))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

`cd cmd/server && go test -run TestComputeAnalyticsDistanceFromRollup -v` → expect FAIL with `undefined: computeAnalyticsDistanceFromRollup`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/distance_rollup_read.go`:

```go
// distance_rollup_read.go — distance analytics result assembly from the
// distance rollup tables.

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// computeAnalyticsDistanceFromRollup builds the distance analytics result
// map from the four distance rollup tables. region "" = global; window zero
// → default 24h via rfEffectiveWindow.
func computeAnalyticsDistanceFromRollup(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	eff := rfEffectiveWindow(window)
	sinceHour, untilHour := rfWindowHourBounds(eff)
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	// Observer-set clause for distance_hourly / distance_pair_hourly:
	// region "" → observer_idx = -1 (global deduped); region != "" → IN(region idxs).
	var oiClause string
	var oiArgs []interface{}
	if region == "" {
		oiClause = "observer_idx = ?"
		oiArgs = []interface{}{-1}
	} else {
		oiClause = "observer_idx IN (" + rfIntPlaceholders(len(idxs)) + ")"
		oiArgs = make([]interface{}, len(idxs))
		for i, v := range idxs {
			oiArgs[i] = v
		}
	}
	baseArgs := []interface{}{sinceHour, untilHour}
	allArgs := append(append([]interface{}{}, baseArgs...), oiArgs...)

	// Per-type aggregates + global histogram counts.
	type catAgg struct {
		count             int
		distSum           float64
		distMin, distMax  float64
		haveDist          bool
		distBins          []int
	}
	byType := map[string]*catAgg{
		"R↔R": {distBins: make([]int, distDistBinCount)},
		"C↔R": {distBins: make([]int, distDistBinCount)},
		"C↔C": {distBins: make([]int, distDistBinCount)},
	}
	hourlyRows, err := db.conn.Query(
		`SELECT type, count, dist_sum, dist_min, dist_max, dist_bins
		 FROM distance_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance hourly read: %w", err)
	}
	for hourlyRows.Next() {
		var typ string
		var cnt int
		var ds float64
		var dmn, dmx sql.NullFloat64
		var bins []byte
		if err := hourlyRows.Scan(&typ, &cnt, &ds, &dmn, &dmx, &bins); err != nil {
			hourlyRows.Close()
			return nil, fmt.Errorf("distance hourly scan: %w", err)
		}
		a := byType[typ]
		if a == nil {
			continue
		}
		a.count += cnt
		a.distSum += ds
		if dmn.Valid {
			if !a.haveDist || dmn.Float64 < a.distMin {
				a.distMin = dmn.Float64
			}
			a.haveDist = true
		}
		if dmx.Valid {
			if dmx.Float64 > a.distMax {
				a.distMax = dmx.Float64
			}
		}
		rfAddBins(a.distBins, rfUnpackBins(bins, distDistBinCount))
	}
	hourlyRows.Close()
	if err := hourlyRows.Err(); err != nil {
		return nil, fmt.Errorf("distance hourly rows: %w", err)
	}

	// Build catStats.
	catStats := map[string]interface{}{}
	totalCount := 0
	totalSum := 0.0
	globalMin, globalMax, haveGlobal := 0.0, 0.0, false
	globalBins := make([]int, distDistBinCount)
	for _, typ := range []string{"R↔R", "C↔R", "C↔C"} {
		a := byType[typ]
		if a.count == 0 {
			catStats[typ] = map[string]interface{}{"count": 0, "avg": 0, "median": 0, "min": 0, "max": 0}
			continue
		}
		avg := a.distSum / float64(a.count)
		med := distMedianFromBins(a.distBins)
		catStats[typ] = map[string]interface{}{
			"count":  a.count,
			"avg":    math.Round(avg*100) / 100,
			"median": math.Round(med*100) / 100,
			"min":    math.Round(a.distMin*100) / 100,
			"max":    math.Round(a.distMax*100) / 100,
		}
		totalCount += a.count
		totalSum += a.distSum
		if !haveGlobal || a.distMin < globalMin {
			globalMin = a.distMin
		}
		if !haveGlobal || a.distMax > globalMax {
			globalMax = a.distMax
		}
		haveGlobal = true
		rfAddBins(globalBins, a.distBins)
	}

	// summary.totalPaths from distance_paths (+ optional region join).
	var totalPaths int
	if region == "" {
		if err := db.conn.QueryRow(
			`SELECT COUNT(*) FROM distance_paths WHERE hour >= ? AND hour <= ?`,
			sinceHour, untilHour).Scan(&totalPaths); err != nil {
			return nil, fmt.Errorf("distance totalPaths: %w", err)
		}
	} else {
		pathRegionArgs := append([]interface{}{sinceHour, untilHour}, oiArgs...)
		if err := db.conn.QueryRow(
			`SELECT COUNT(DISTINCT p.tx_id)
			 FROM distance_paths p
			 JOIN distance_path_observers o ON o.tx_id = p.tx_id
			 WHERE p.hour >= ? AND p.hour <= ?
			   AND o.observer_idx IN (`+rfIntPlaceholders(len(idxs))+`)`,
			pathRegionArgs...).Scan(&totalPaths); err != nil {
			return nil, fmt.Errorf("distance totalPaths region: %w", err)
		}
	}

	summary := map[string]interface{}{
		"totalHops":  totalCount,
		"totalPaths": totalPaths,
		"avgDist":    0.0,
		"maxDist":    0.0,
	}
	if totalCount > 0 {
		summary["avgDist"] = math.Round((totalSum/float64(totalCount))*100) / 100
		summary["maxDist"] = math.Round(globalMax*100) / 100
	}

	// distHistogram (fixed 25 bins).
	var distHistogram interface{} = []interface{}{}
	if totalCount > 0 {
		distHistogram = rfHistogramFromBins(globalBins, distDistBinMin, distDistBinWidth)
		// rfHistogramFromBins returns {bins:[{x,w,count}], min, max} keyed; for
		// distance the in-memory shape uses the raw dist min/max — overlay them.
		if m, ok := distHistogram.(map[string]interface{}); ok {
			m["min"] = math.Round(globalMin*100) / 100
			m["max"] = math.Round(globalMax*100) / 100
			distHistogram = m
		}
	}

	// distOverTime: GROUP BY hour, sum count + dist_sum.
	timeRows, err := db.conn.Query(
		`SELECT hour, SUM(count), SUM(dist_sum)
		 FROM distance_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause+`
		 GROUP BY hour ORDER BY hour`, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance over_time: %w", err)
	}
	distOverTime := make([]map[string]interface{}, 0)
	for timeRows.Next() {
		var hr string
		var cnt int
		var sum float64
		if err := timeRows.Scan(&hr, &cnt, &sum); err != nil {
			timeRows.Close()
			return nil, fmt.Errorf("distance over_time scan: %w", err)
		}
		if cnt == 0 {
			continue
		}
		distOverTime = append(distOverTime, map[string]interface{}{
			"hour":  hr,
			"avg":   math.Round((sum/float64(cnt))*100) / 100,
			"count": cnt,
		})
	}
	timeRows.Close()

	// topHops: per (pair_key, type) over window, take row with max best_dist.
	pairRows, err := db.conn.Query(
		`SELECT pair_key, type, count, best_dist, best_from_name, best_from_pk,
		        best_to_name, best_to_pk, snr_max, snr_bins
		 FROM distance_pair_hourly
		 WHERE hour >= ? AND hour <= ? AND `+oiClause, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("distance pair read: %w", err)
	}
	type pairAgg struct {
		count       int
		bestDist    float64
		fromName    string
		fromPk      string
		toName      string
		toPk        string
		typ         string
		snrMax      float64
		haveSnr     bool
		snrBins     []int
	}
	pairAggs := map[string]*pairAgg{}
	for pairRows.Next() {
		var pk, typ string
		var cnt int
		var bd float64
		var fn, fpk, tn, tpk sql.NullString
		var snr sql.NullFloat64
		var sbins []byte
		if err := pairRows.Scan(&pk, &typ, &cnt, &bd, &fn, &fpk, &tn, &tpk, &snr, &sbins); err != nil {
			pairRows.Close()
			return nil, fmt.Errorf("distance pair scan: %w", err)
		}
		k := pk + "|" + typ
		a := pairAggs[k]
		if a == nil {
			a = &pairAgg{typ: typ, snrBins: make([]int, distSnrBinCount)}
			pairAggs[k] = a
		}
		a.count += cnt
		if bd > a.bestDist {
			a.bestDist = bd
			a.fromName = fn.String
			a.fromPk = fpk.String
			a.toName = tn.String
			a.toPk = tpk.String
		}
		if snr.Valid {
			if !a.haveSnr || snr.Float64 > a.snrMax {
				a.snrMax = snr.Float64
				a.haveSnr = true
			}
		}
		rfAddBins(a.snrBins, rfUnpackBins(sbins, distSnrBinCount))
	}
	pairRows.Close()
	pairs := make([]*pairAgg, 0, len(pairAggs))
	for _, a := range pairAggs {
		pairs = append(pairs, a)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].bestDist > pairs[j].bestDist })
	if len(pairs) > 20 {
		pairs = pairs[:20]
	}
	topHops := make([]map[string]interface{}, 0, len(pairs))
	for _, a := range pairs {
		row := map[string]interface{}{
			"fromName": a.fromName, "fromPk": a.fromPk,
			"toName": a.toName, "toPk": a.toPk,
			"dist":     a.bestDist,
			"type":     a.typ,
			"obsCount": a.count,
		}
		if a.haveSnr {
			row["bestSnr"] = a.snrMax
			row["medianSnr"] = distMedianSnrFromBins(a.snrBins)
		}
		topHops = append(topHops, row)
	}

	// topPaths: ORDER BY total_dist DESC LIMIT 20.
	var pathRows *sql.Rows
	if region == "" {
		pathRows, err = db.conn.Query(
			`SELECT tx_id, total_dist, hop_count, hash, timestamp, hops_json
			 FROM distance_paths
			 WHERE hour >= ? AND hour <= ?
			 ORDER BY total_dist DESC LIMIT 20`, sinceHour, untilHour)
	} else {
		pathArgs := append([]interface{}{sinceHour, untilHour}, oiArgs...)
		pathRows, err = db.conn.Query(
			`SELECT p.tx_id, p.total_dist, p.hop_count, p.hash, p.timestamp, p.hops_json
			 FROM distance_paths p
			 JOIN distance_path_observers o ON o.tx_id = p.tx_id
			 WHERE p.hour >= ? AND p.hour <= ?
			   AND o.observer_idx IN (`+rfIntPlaceholders(len(idxs))+`)
			 GROUP BY p.tx_id
			 ORDER BY p.total_dist DESC LIMIT 20`, pathArgs...)
	}
	if err != nil {
		return nil, fmt.Errorf("distance paths read: %w", err)
	}
	topPaths := make([]map[string]interface{}, 0)
	for pathRows.Next() {
		var txID, hc int
		var td float64
		var hsh, ts, hops sql.NullString
		if err := pathRows.Scan(&txID, &td, &hc, &hsh, &ts, &hops); err != nil {
			pathRows.Close()
			return nil, fmt.Errorf("distance paths scan: %w", err)
		}
		var hopList []map[string]interface{}
		if hops.String != "" {
			_ = json.Unmarshal([]byte(hops.String), &hopList)
		}
		topPaths = append(topPaths, map[string]interface{}{
			"hash":      hsh.String,
			"totalDist": td,
			"hopCount":  hc,
			"timestamp": ts.String,
			"hops":      hopList,
		})
	}
	pathRows.Close()

	return map[string]interface{}{
		"summary":       summary,
		"topHops":       topHops,
		"topPaths":      topPaths,
		"catStats":      catStats,
		"distHistogram": distHistogram,
		"distOverTime":  distOverTime,
	}, nil
}

// distMedianFromBins interpolates the median distance from a 25-bin
// histogram of [0,300) km using 12 km bins.
func distMedianFromBins(bins []int) float64 {
	total := 0
	for _, c := range bins {
		total += c
	}
	if total == 0 {
		return 0
	}
	half := total / 2
	cum := 0
	for i, c := range bins {
		cum += c
		if cum >= half {
			return float64(distDistBinMin) + (float64(i)+0.5)*float64(distDistBinWidth)
		}
	}
	return 0
}

// distMedianSnrFromBins interpolates the median SNR from a 50-bin histogram
// of [-30,20) dB using 1 dB bins.
func distMedianSnrFromBins(bins []int) float64 {
	total := 0
	for _, c := range bins {
		total += c
	}
	if total == 0 {
		return 0
	}
	half := total / 2
	cum := 0
	for i, c := range bins {
		cum += c
		if cum >= half {
			return float64(distSnrBinMin) + (float64(i)+0.5)*float64(distSnrBinWidth)
		}
	}
	return 0
}
```

(`rfEffectiveWindow`, `rfWindowHourBounds`, `rfRegionObserverIdxs`, `rfIntPlaceholders`, `rfAddBins`, `rfUnpackBins`, `rfHistogramFromBins` all exist in earlier rollup files.)

- [ ] **Step 4: Run test to verify it passes**

`cd cmd/server && go test -run TestComputeAnalyticsDistanceFromRollup -v` → expect PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/distance_rollup_read.go cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup_read.go cmd/server/distance_rollup_test.go
git commit -m "feat(distance-rollup): rollup read path (summary/topHops/topPaths/catStats/histogram/over-time)"
```

---

## Task 6: extend in-memory compute to accept TimeWindow

**Files:**
- Modify: `cmd/server/store_distance.go:405-676` (extend `GetAnalyticsDistance` + `computeAnalyticsDistance`)

The cache wrapper in Task 7 calls the in-memory compute with a `TimeWindow` as fallback. Today the in-memory ignores any window. This task threads the window through: rename the existing method to take a window, filter `hopsSnap`/`pathsSnap` by `window.Includes(timestamp)`, and keep a thin no-arg variant for backwards compatibility.

- [ ] **Step 1: Add the windowed method**

In `cmd/server/store_distance.go`, change `GetAnalyticsDistance(region string)` to `GetAnalyticsDistance(region string) map[string]interface{}` BUT make it call a new `GetAnalyticsDistanceWithWindow(region, TimeWindow{})`. Replace lines 405–422 with:

```go
func (s *PacketStore) GetAnalyticsDistance(region string) map[string]interface{} {
	return s.GetAnalyticsDistanceWithWindow(region, TimeWindow{})
}

func (s *PacketStore) GetAnalyticsDistanceWithWindow(region string, window TimeWindow) map[string]interface{} {
	cacheKey := region
	if !window.IsZero() {
		cacheKey = region + "|" + window.CacheKey()
	}
	s.cacheMu.Lock()
	if cached, ok := s.distCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		s.cacheHits++
		s.cacheMu.Unlock()
		return cached.data
	}
	s.cacheMisses++
	s.cacheMu.Unlock()

	// SQL rollup path. Falls through to the in-memory compute on flag-off /
	// not-ready / read error.
	var result map[string]interface{}
	if s.analyticsSQLBackend && s.db != nil && distanceRollupReady(s.db.conn) {
		if r, err := computeAnalyticsDistanceFromRollup(s.db, region, window); err == nil {
			result = r
		} else {
			log.Printf("[distance-rollup] read failed, falling back to in-memory: %v", err)
		}
	}
	if result == nil {
		result = s.computeAnalyticsDistance(region, window)
	}

	s.cacheMu.Lock()
	s.distCache[cacheKey] = &cachedResult{data: result, expiresAt: time.Now().Add(s.rfCacheTTL)}
	s.cacheMu.Unlock()
	return result
}
```

- [ ] **Step 2: Extend computeAnalyticsDistance to accept window**

Change the signature of `computeAnalyticsDistance` from `(region string)` to `(region string, window TimeWindow)`. After the existing `filteredHops` / `filteredPaths` build (around lines 491-505 in store_distance.go), add a window filter pass:

Replace:

```go
	// Filter precomputed hop records (copy to avoid mutating precomputed data during sort)
	filteredHops := make([]distHopRecord, 0, len(hopsSnap))
	for i := range hopsSnap {
		if matchSet == nil || matchSet[hopsSnap[i].tx] {
			filteredHops = append(filteredHops, hopsSnap[i])
		}
	}

	// Filter precomputed path records
	filteredPaths := make([]distPathRecord, 0, len(pathsSnap))
	for i := range pathsSnap {
		if matchSet == nil || matchSet[pathsSnap[i].tx] {
			filteredPaths = append(filteredPaths, pathsSnap[i])
		}
	}
```

with:

```go
	// Filter precomputed hop records (copy to avoid mutating precomputed data during sort)
	filteredHops := make([]distHopRecord, 0, len(hopsSnap))
	for i := range hopsSnap {
		if matchSet != nil && !matchSet[hopsSnap[i].tx] {
			continue
		}
		if !window.IsZero() && !window.Includes(hopsSnap[i].Timestamp) {
			continue
		}
		filteredHops = append(filteredHops, hopsSnap[i])
	}

	// Filter precomputed path records
	filteredPaths := make([]distPathRecord, 0, len(pathsSnap))
	for i := range pathsSnap {
		if matchSet != nil && !matchSet[pathsSnap[i].tx] {
			continue
		}
		if !window.IsZero() && !window.Includes(pathsSnap[i].Timestamp) {
			continue
		}
		filteredPaths = append(filteredPaths, pathsSnap[i])
	}
```

- [ ] **Step 3: Build**

`cd cmd/server && go build ./...` → expect build OK.

- [ ] **Step 4: Run existing distance tests**

`cd cmd/server && go test -run 'Distance|distance' -v 2>&1 | tail -30` → all PASS. Existing in-memory tests still pass because `TimeWindow{}` is zero and the window filter is a no-op for that case.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/store_distance.go
git add cmd/server/store_distance.go
git commit -m "feat(distance-rollup): GetAnalyticsDistanceWithWindow wrapper + in-memory window filter"
```

---

## Task 7: route handler accepts window

**Files:**
- Modify: `cmd/server/routes.go:1835-1842` (handleAnalyticsDistance)

- [ ] **Step 1: Read the current handler**

Current `handleAnalyticsDistance`:

```go
func (s *Server) handleAnalyticsDistance(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if s.store == nil {
		writeError(w, 503, "Packet store unavailable")
		return
	}
	writeJSON(w, s.store.GetAnalyticsDistance(region))
}
```

- [ ] **Step 2: Replace with window-aware version**

Replace lines 1835–1842 with:

```go
func (s *Server) handleAnalyticsDistance(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if s.store == nil {
		writeError(w, 503, "Packet store unavailable")
		return
	}
	window := ParseTimeWindow(r)
	writeJSON(w, s.store.GetAnalyticsDistanceWithWindow(region, window))
}
```

(`ParseTimeWindow` is in `cmd/server/time_window.go` and is already used by the RF / channels handlers.)

- [ ] **Step 3: Build + targeted route tests**

`cd cmd/server && go build ./... && go test -run 'Distance|distance' -v 2>&1 | tail -20` → build OK, tests PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -l cmd/server/routes.go
git add cmd/server/routes.go
git commit -m "feat(distance-rollup): handler accepts ?window= and ?from/to"
```

---

## Task 8: wire startup in main.go

**Files:**
- Modify: `cmd/server/main.go` (~line 249 ensure-table; ~line 663 backfill + ticker)

- [ ] **Step 1: Add the ensure-table call**

After the existing `ensureNodeRollupTable` block (around line 249), add:

```go
	if err := ensureDistanceRollupTable(dbPath); err != nil {
		log.Fatalf("ensureDistanceRollupTable: %v", err)
	}
```

- [ ] **Step 2: Add backfill + ticker**

Inside the `if cfg.PacketStore != nil && cfg.PacketStore.AnalyticsSQLBackend {` block, AFTER the node-rollup ticker goroutine, BEFORE the closing `}`, add:

```go
		go backfillDistanceRollupAsync(dbPath)
		distanceRollupTicker := time.NewTicker(5 * time.Minute)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[distance-rollup] maintenance panic recovered: %v", r)
				}
			}()
			for range distanceRollupTicker.C {
				rw, err := cachedRW(dbPath)
				if err != nil {
					log.Printf("[distance-rollup] maintenance open rw: %v", err)
					continue
				}
				runDistanceRollupMaintenanceGuarded(rw)
			}
		}()
```

- [ ] **Step 3: Build**

`cd cmd/server && go build ./...` → expect build OK.

- [ ] **Step 4: Run the full suite**

`cd cmd/server && go test ./... 2>&1 | tail -10` → expect `ok`, no regressions.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/main.go
git add cmd/server/main.go
git commit -m "feat(distance-rollup): wire schema, backfill, maintenance into startup"
```

---

## Task 9: parity test (rollup vs in-memory)

**Files:**
- Test: `cmd/server/distance_rollup_test.go` (append)

Verifies summary numbers match in-memory on a controlled fixture (one tx, one path, two GPS-bearing hops).

- [ ] **Step 1: Write the test**

Append (no new imports needed yet — `testing` and `time` are already in scope from Task 3):

```go
func TestDistanceRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES
		('aa','A','repeater',52.0,4.0),
		('bb','B','repeater',53.0,5.0),
		('cc','C','client',54.0,6.0)`)
	// One tx in the current hour, two hops in path.
	now := time.Now().UTC()
	fs := now.Format("2006-01-02T15:04:05Z")
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
		VALUES (1,'aa','h1',?,1,'{"pubKey":"aa"}')`, fs)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
		VALUES (1,1,?,'["bb","cc"]',5.0)`, now.Unix())
	rw, _ := cachedRW(db.path)
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	// In-memory reference.
	ps := loadStore(t, db.path, 0)
	memRes := ps.GetAnalyticsDistanceWithWindow("",
		TimeWindow{Since: now.Add(-1 * time.Hour).Format(time.RFC3339),
			Until: now.Add(1 * time.Hour).Format(time.RFC3339)})

	// Rollup path.
	ps.analyticsSQLBackend = true
	// Drop the cached in-memory result so the flag path takes over.
	ps.cacheMu.Lock()
	ps.distCache = map[string]*cachedResult{}
	ps.cacheMu.Unlock()
	rollupRes := ps.GetAnalyticsDistanceWithWindow("",
		TimeWindow{Since: now.Add(-1 * time.Hour).Format(time.RFC3339),
			Until: now.Add(1 * time.Hour).Format(time.RFC3339)})

	memTotal := memRes["summary"].(map[string]interface{})["totalHops"].(int)
	rollupTotal := rollupRes["summary"].(map[string]interface{})["totalHops"].(int)
	if memTotal != rollupTotal {
		t.Fatalf("totalHops rollup=%d in-memory=%d", rollupTotal, memTotal)
	}
	if rollupTotal != 2 {
		t.Fatalf("totalHops=%d want 2", rollupTotal)
	}
	memPaths := memRes["summary"].(map[string]interface{})["totalPaths"].(int)
	rollupPaths := rollupRes["summary"].(map[string]interface{})["totalPaths"].(int)
	if memPaths != rollupPaths {
		t.Fatalf("totalPaths rollup=%d in-memory=%d", rollupPaths, memPaths)
	}
	if rollupPaths != 1 {
		t.Fatalf("totalPaths=%d want 1", rollupPaths)
	}
}
```

- [ ] **Step 2: Run the test**

`cd cmd/server && go test -run TestDistanceRollupParity -v` → expect PASS. If totals differ, fix the source of divergence before continuing.

- [ ] **Step 3: Commit**

```bash
gofmt -l cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup_test.go
git commit -m "test(distance-rollup): rollup vs in-memory parity (totalHops + totalPaths)"
```

---

## Task 10: performance benchmark

**Files:**
- Test: `cmd/server/distance_rollup_test.go` (append)

- [ ] **Step 1: Write the perf test**

First, update the `import` block of `cmd/server/distance_rollup_test.go` to add `fmt`:

```go
import (
	"fmt"
	"testing"
	"time"
)
```

Then append:

```go
func TestDistanceRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureDistanceRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	rw, _ := cachedRW(db.path)
	// 20 GPS-bearing nodes around the Netherlands.
	for i := 0; i < 20; i++ {
		pk := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", i)
		lat := 51.5 + float64(i%5)*0.4
		lon := 4.0 + float64(i/5)*0.4
		role := "client"
		if i%3 == 0 {
			role = "repeater"
		}
		mustExec(t, db, `INSERT INTO nodes(public_key,name,role,lat,lon) VALUES (?,?,?,?,?)`,
			pk, fmt.Sprintf("N%d", i), role, lat, lon)
	}
	// ~30k transmissions spread over 7 days; each path has 2 hops cycling
	// among the 20 nodes.
	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	tx, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30000; i++ {
		ts := base.Add(time.Duration(i) * 18 * time.Second)
		fs := ts.Format("2006-01-02T15:04:05Z")
		sender := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", i%20)
		hop1 := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", (i+7)%20)
		hop2 := fmt.Sprintf("%02x00000000000000000000000000000000000000000000000000000000000000", (i+11)%20)
		if _, err := tx.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type,decoded_json)
			VALUES (?,?,?,?,1,?)`, i, "aa", fmt.Sprintf("h%d", i), fs, `{"pubKey":"`+sender+`"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json,snr)
			VALUES (?,1,?,?,5.0)`, i, ts.Unix(), `["`+hop1+`","`+hop2+`"]`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runDistanceRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	win := TimeWindow{
		Since: base.Format(time.RFC3339),
		Until: time.Now().UTC().Format(time.RFC3339),
	}
	start := time.Now()
	res, err := computeAnalyticsDistanceFromRollup(db, "", win)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res["summary"]; !ok {
		t.Fatal("missing summary")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("rollup read took %s, want < 500ms", elapsed)
	}
	t.Logf("distance rollup read over 30k tx: %s", elapsed)
}
```

- [ ] **Step 2: Run the perf test**

`cd cmd/server && go test -run TestDistanceRollupPerf -v` → expect PASS; logged time well under 500ms.

If the threshold fires legitimately (e.g. > 800ms consistently with correct behaviour), do NOT raise the threshold — report DONE_WITH_CONCERNS with the measured numbers and let the controller decide.

- [ ] **Step 3: Commit**

```bash
gofmt -l cmd/server/distance_rollup_test.go
git add cmd/server/distance_rollup_test.go
git commit -m "test(distance-rollup): bulk-read performance benchmark"
```

---

## Task 11: final full-suite verification

- [ ] **Step 1: Full package test suite**

`cd cmd/server && go test ./... 2>&1 | tail -10` → expect `ok github.com/corescope/server`. No regressions.

- [ ] **Step 2: gofmt audit of every touched file**

`cd cmd/server && gofmt -l distance_rollup.go distance_rollup_maintain.go distance_rollup_read.go distance_rollup_test.go store_distance.go routes.go main.go`

The first four (new files) must produce empty output. `store_distance.go`, `routes.go`, `main.go` may show pre-existing gofmt diffs that predate this branch — confirm by `gofmt -d <file>` and `git blame` if they appear. New code added by this plan must be gofmt-clean.

- [ ] **Step 3: confirm no stray changes**

`git status --short` (from worktree root) → only intended new/modified files; nothing uncommitted.

---

## Self-review notes (spec coverage)

- Schema (5 tables + indexes, fixed bins) → Task 1.
- Positional resolved_path ?? path_json + GPS chain + role classify + pair key → Task 2.
- Single-hour recompute: indexed first_seen range, raw read outside write tx, per-tx grouping, per-observer + global -1 attribution, idempotent delete-then-insert, short write tx → Task 3.
- Watermark on observations.id, touched-hours, 50ms yield, guard mutex, backfill, ready check → Task 4.
- Read path (summary, catStats, distHistogram, distOverTime, totalPaths, topHops, topPaths) → Task 5.
- In-memory TimeWindow extension + cache wrapper with flag/ready branching → Task 6.
- Handler picks up ?window= → Task 7.
- main.go wiring, flag-gated → Task 8.
- Parity + perf gate → Tasks 9–10.
- Full-suite regression check → Task 11.

**Deploy (after merge to main):** Coolify CLI, app uuid `yngsizj96krk25x05a08u8ib`, `coolify deploy uuid <uuid> --force`. The flag is already on → deploy auto-runs the distance-rollup backfill. Watch `coolify app logs yngsizj96krk25x05a08u8ib` for `[distance-rollup] backfill complete` and no SQLITE_BUSY storm beyond the steady-state pattern other rollups show. Verify `/api/analytics/distance` returns the expected JSON shape over a real region.
