# RF Analytics SQL Backend — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a SQL-backed implementation of RF analytics so the endpoint no longer iterates the in-memory `PacketStore`, behind a config flag, with parity tests against the existing in-memory implementation.

**Architecture:** A new `computeAnalyticsRFSQL` method on `*PacketStore` runs ~7 SQLite aggregation queries against `s.db` and assembles the same `map[string]interface{}` the current `computeAnalyticsRF` returns. Exact aggregates come from SQL; percentiles/histograms come from sorted single-column fetches; scatter from a strided sample. A `packetStore.analyticsSqlBackend` config flag selects old vs new on cache miss. The TTL cache wrapper is reused unchanged except for error propagation.

**Tech Stack:** Go, `database/sql` with the modernc.org/sqlite driver, existing CoreScope `DB`/`PacketStore` types.

Spec: `.specs/2026-05-19-rf-analytics-sql-design.md`

---

## Build commands — IMPORTANT

This project uses **per-directory Go modules**: `cmd/server/` has its own `go.mod` (module `github.com/corescope/server`). There is **no root `go.mod`**. Every `go` command in this plan must run from inside `cmd/server/`.

- Where a task says `go test ./cmd/server/ -run X -v` → actually run: `cd cmd/server && go test . -run X -v`
- Where a task says `go build ./...` → actually run: `cd cmd/server && go build ./...`
- Where a task says `go vet ./cmd/server/...` → actually run: `cd cmd/server && go vet ./...`
- `git` commands run from the worktree root; their paths (`cmd/server/...`) are relative to that root — unchanged.

## Background facts (verified)

- `observations(id, transmission_id, observer_idx, snr REAL, rssi REAL, timestamp INTEGER, raw_hex, …)` — `timestamp` is Unix epoch **seconds**.
- `transmissions(id, raw_hex, hash TEXT UNIQUE, first_seen TEXT, payload_type INTEGER, …)` — `first_seen` is RFC3339 text.
- `computeAnalyticsRF` (in `cmd/server/store_analytics.go:46-580`) has two branches:
  - **No region:** iterate transmissions, keep those with `window.Includes(tx.FirstSeen)`, count all their observations.
  - **Region:** iterate observations of region observers, keep those with `window.Includes(obs.Timestamp)`.
  The SQL version must replicate this: no-region filters on `transmissions.first_seen`; region filters on `observations.timestamp`.
- Result map keys: `totalPackets` (count of obs with non-null snr), `totalAllPackets` (all obs counted), `totalTransmissions` (distinct hashes), `snr`/`rssi` (`{min,max,avg,median,stddev}`), `snrValues`/`rssiValues` (20-bin histograms), `packetSizes` (25-bin histogram), `minPacketSize`/`maxPacketSize`/`avgPacketSize`, `packetsPerHour` (`[{hour,count}]`), `payloadTypes` (`[{type,name,count}]` desc by count), `snrByType` (`[{name,count,avg,min,max}]` desc by count), `signalOverTime` (`[{hour,count,avgSnr}]`), `scatterData` (`[{snr,rssi}]` ≤500), `timeSpanHours`.
- `median` in the old code = `vals[len/2]` on the sorted slice (not a true median for even counts) — replicate exactly.
- `hour` keys are `ts[:13]` of an RFC3339 string, i.e. `2026-05-18T23`.
- Test helpers exist: `setupTestDB(t) *DB`, `mustExec(t, db, q, args…)`, `loadStore(t, dbPath, maxMemMB) *PacketStore` (in `cmd/server/*_test.go`).
- `PacketStore` holds `db *DB` (set in `NewPacketStore`). `DB` has `conn *sql.DB`, `path string`.

---

## File structure

- **Modify** `cmd/server/config.go` — add `AnalyticsSQLBackend` to `PacketStoreConfig`.
- **Modify** `cmd/server/store.go` — add `analyticsSQLBackend bool` field to `PacketStore`, set in `NewPacketStore`.
- **Modify** `cmd/server/ensure_indexes.go` — add the composite index.
- **Create** `cmd/server/analytics_rf_sql.go` — the SQL RF computer + its query helpers.
- **Modify** `cmd/server/store_analytics.go` — extract histogram helpers to package level; change cache wrapper to branch on the flag and return an error.
- **Modify** `cmd/server/routes.go` — `handleAnalyticsRF` propagates the error as HTTP 500.
- **Create** `cmd/server/analytics_rf_sql_test.go` — parity tests.

---

## Task 1: Config flag

**Files:**
- Modify: `cmd/server/config.go` (`PacketStoreConfig`, around line 161-166)
- Modify: `cmd/server/store.go` (`PacketStore` struct ~line 285; `NewPacketStore` ~line 363)
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/server/analytics_rf_sql_test.go`:

```go
package main

import "testing"

func TestAnalyticsSQLBackendFlagWiring(t *testing.T) {
	db := setupTestDB(t)
	cfg := &PacketStoreConfig{AnalyticsSQLBackend: true}
	ps := NewPacketStore(db, cfg)
	if !ps.analyticsSQLBackend {
		t.Fatal("expected analyticsSQLBackend true when config sets it")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestAnalyticsSQLBackendFlagWiring -v`
Expected: FAIL — `AnalyticsSQLBackend` and `analyticsSQLBackend` undefined (compile error).

- [ ] **Step 3: Add the config field**

In `cmd/server/config.go`, inside `type PacketStoreConfig struct`, add after `HotStartupHours`:

```go
	AnalyticsSQLBackend           bool    `json:"analyticsSqlBackend"`           // route RF analytics through the SQL backend
```

- [ ] **Step 4: Add the PacketStore field and wire it**

In `cmd/server/store.go`, add to the `PacketStore` struct (near `retentionHours`):

```go
	analyticsSQLBackend bool // RF analytics uses the SQL backend (config packetStore.analyticsSqlBackend)
```

In `NewPacketStore`, where other `cfg` fields are copied (near `ps.retentionHours = cfg.RetentionHours`), add:

```go
		ps.analyticsSQLBackend = cfg.AnalyticsSQLBackend
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestAnalyticsSQLBackendFlagWiring -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/server/config.go cmd/server/store.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): add packetStore.analyticsSqlBackend config flag"
```

---

## Task 2: Composite index

**Files:**
- Modify: `cmd/server/ensure_indexes.go` (the `stmts` slice, ~line 21-32)
- Test: `cmd/server/ensure_indexes_test.go` (create if absent) or `analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

Add to `cmd/server/analytics_rf_sql_test.go`:

```go
func TestEnsureServerIndexesCreatesTSSnrRssi(t *testing.T) {
	db := setupTestDB(t)
	if err := ensureServerIndexes(db.path); err != nil {
		t.Fatalf("ensureServerIndexes: %v", err)
	}
	var name string
	err := db.conn.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_observations_ts_snr_rssi'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("composite index not created: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestEnsureServerIndexesCreatesTSSnrRssi -v`
Expected: FAIL — index not found, `sql: no rows in result set`.

- [ ] **Step 3: Add the index statement**

In `cmd/server/ensure_indexes.go`, add to the `stmts` slice:

```go
		`CREATE INDEX IF NOT EXISTS idx_observations_ts_snr_rssi ON observations(timestamp, snr, rssi)`,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestEnsureServerIndexesCreatesTSSnrRssi -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/ensure_indexes.go cmd/server/analytics_rf_sql_test.go
git commit -m "perf(analytics): add (timestamp,snr,rssi) covering index"
```

---

## Task 3: Window and region SQL filter helpers

Build the `WHERE`-clause fragments + args for the two filtering modes. No-region filters `transmissions.first_seen` (RFC3339 string); region filters `observations.timestamp` (epoch seconds) and `observations.observer_idx`.

**Files:**
- Create: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFWindowEpochBounds(t *testing.T) {
	w := TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-18T12:00:00Z"}
	lo, hi, ok := rfWindowEpochBounds(w)
	if !ok {
		t.Fatal("expected ok for bounded window")
	}
	if lo != 1779408000 || hi != 1779451200 {
		t.Fatalf("epoch bounds wrong: lo=%d hi=%d", lo, hi)
	}
	if _, _, ok := rfWindowEpochBounds(TimeWindow{}); ok {
		t.Fatal("zero window must report ok=false")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFWindowEpochBounds -v`
Expected: FAIL — `rfWindowEpochBounds` undefined.

- [ ] **Step 3: Create the file with the helper**

Create `cmd/server/analytics_rf_sql.go`:

```go
// analytics_rf_sql.go — SQL-backed RF analytics (spec 2026-05-19-rf-analytics-sql).

package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// rfWindowEpochBounds converts a TimeWindow to inclusive Unix-second bounds for
// the observations.timestamp column. ok=false means the window is unbounded on
// at least... it returns ok=false only when BOTH ends are empty (zero window).
// Unset ends are reported as MinInt64/MaxInt64 sentinels.
func rfWindowEpochBounds(w TimeWindow) (lo, hi int64, ok bool) {
	if w.IsZero() {
		return 0, 0, false
	}
	lo = -62135596800 // year 1, effectively unbounded low
	hi = 253402300799 // year 9999, effectively unbounded high
	if w.Since != "" {
		if t, err := time.Parse(time.RFC3339, w.Since); err == nil {
			lo = t.Unix()
		}
	}
	if w.Until != "" {
		if t, err := time.Parse(time.RFC3339, w.Until); err == nil {
			hi = t.Unix()
		}
	}
	return lo, hi, true
}

// rfObsWindowClause returns a WHERE fragment + args filtering observations by
// epoch-second timestamp. Empty when the window is zero.
func rfObsWindowClause(w TimeWindow) (string, []interface{}) {
	lo, hi, ok := rfWindowEpochBounds(w)
	if !ok {
		return "", nil
	}
	return "o.timestamp >= ? AND o.timestamp <= ?", []interface{}{lo, hi}
}

// rfTxWindowClause returns a WHERE fragment + args filtering transmissions by
// the RFC3339 first_seen string. Empty when the window is zero.
func rfTxWindowClause(w TimeWindow) (string, []interface{}) {
	if w.IsZero() {
		return "", nil
	}
	var parts []string
	var args []interface{}
	if w.Since != "" {
		parts = append(parts, "t.first_seen >= ?")
		args = append(args, w.Since)
	}
	if w.Until != "" {
		parts = append(parts, "t.first_seen <= ?")
		args = append(args, w.Until)
	}
	return strings.Join(parts, " AND "), args
}

// rfObserverIdxClause returns a WHERE fragment + args restricting to a set of
// observer_idx values. Empty when idxs is empty.
func rfObserverIdxClause(idxs []int) (string, []interface{}) {
	if len(idxs) == 0 {
		return "", nil
	}
	ph := make([]string, len(idxs))
	args := make([]interface{}, len(idxs))
	for i, v := range idxs {
		ph[i] = "?"
		args[i] = v
	}
	return fmt.Sprintf("o.observer_idx IN (%s)", strings.Join(ph, ",")), args
}

// rfWhere joins non-empty clauses with AND and concatenates their args.
func rfWhere(clauses []string, argSets [][]interface{}) (string, []interface{}) {
	var parts []string
	var args []interface{}
	for i, c := range clauses {
		if c == "" {
			continue
		}
		parts = append(parts, c)
		args = append(args, argSets[i]...)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

var _ = sql.ErrNoRows // keep database/sql imported for later tasks
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestRFWindowEpochBounds -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): RF SQL window/region filter helpers"
```

---

## Task 4: Region → observer_idx resolver

The region path needs `observer_idx` integers. Add a helper that maps a region to its `observer_idx` set using the `observers` table, independent of in-memory packet data.

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFRegionObserverIdxs(t *testing.T) {
	db := setupTestDB(t)
	// setupTestDB seeds observers; pick whatever region helper resolves.
	idxs, err := rfRegionObserverIdxs(db, "")
	if err != nil {
		t.Fatalf("rfRegionObserverIdxs empty region: %v", err)
	}
	if idxs != nil {
		t.Fatalf("empty region must return nil idx set, got %v", idxs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFRegionObserverIdxs -v`
Expected: FAIL — `rfRegionObserverIdxs` undefined.

- [ ] **Step 3: Implement the resolver**

Add to `cmd/server/analytics_rf_sql.go`:

```go
// rfRegionObserverIdxs returns the observer_idx values whose observers belong
// to the given region. An empty region returns (nil, nil) — caller treats nil
// as "no region filter". The region→observer mapping mirrors the in-memory
// resolveRegionObservers: observers whose region column equals the region.
func rfRegionObserverIdxs(db *DB, region string) ([]int, error) {
	if region == "" {
		return nil, nil
	}
	rows, err := db.conn.Query(
		`SELECT observer_idx FROM observers WHERE region = ? AND observer_idx IS NOT NULL`,
		region,
	)
	if err != nil {
		return nil, fmt.Errorf("rfRegionObserverIdxs query: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, fmt.Errorf("rfRegionObserverIdxs scan: %w", err)
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}
```

> **Note for implementer:** confirm the `observers` table has a `region` column and an `observer_idx` column with `PRAGMA table_info(observers)`. If region is stored differently (e.g. a join table or a prefix of the observer name), adjust this query to match `resolveRegionObservers` in `store.go` — the SQL result set must equal the in-memory one. The parity test in Task 10 with a region argument will catch a mismatch.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestRFRegionObserverIdxs -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): RF SQL region→observer_idx resolver"
```

---

## Task 5: Core aggregates query

One query over `observations` (joined to `transmissions` for the window basis) returning the counts and SNR/RSSI sums needed for stats.

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFCoreAggregates(t *testing.T) {
	db := setupTestDB(t)
	// Two transmissions in May 2026, three observations.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccddee','h2','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(1,1,7.0,-90.0,1779444000),(2,0,NULL,-70.0,1779447600)`)

	agg, err := rfCoreAggregates(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfCoreAggregates: %v", err)
	}
	if agg.NObs != 3 {
		t.Fatalf("NObs=%d want 3", agg.NObs)
	}
	if agg.NSnr != 2 {
		t.Fatalf("NSnr=%d want 2", agg.NSnr)
	}
	if agg.SnrSum != 12.0 || agg.SnrMin != 5.0 || agg.SnrMax != 7.0 {
		t.Fatalf("snr agg wrong: %+v", agg)
	}
	if agg.NRssi != 3 {
		t.Fatalf("NRssi=%d want 3", agg.NRssi)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFCoreAggregates -v`
Expected: FAIL — `rfCoreAggregates` / `rfAggregates` undefined.

- [ ] **Step 3: Implement the query**

Add to `cmd/server/analytics_rf_sql.go`:

```go
// rfAggregates holds the scalar aggregate results for RF analytics.
type rfAggregates struct {
	NObs    int64
	NSnr    int64
	SnrSum  float64
	SnrSumSq float64
	SnrMin  float64
	SnrMax  float64
	NRssi   int64
	RssiSum float64
	RssiSumSq float64
	RssiMin float64
	RssiMax float64
}

// rfCoreAggregates runs the single-scan aggregate query. region="" means no
// region filter (window applies to transmissions.first_seen); a non-empty
// region filters observations by observer_idx and by observations.timestamp.
func rfCoreAggregates(db *DB, region string, window TimeWindow) (rfAggregates, error) {
	var agg rfAggregates
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return agg, err
	}

	var where string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args = rfWhere([]string{txW}, [][]interface{}{txArgs})
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, args = rfWhere([]string{obsW, idxW}, [][]interface{}{obsArgs, idxArgs})
	}

	q := `SELECT
		COUNT(*),
		COUNT(o.snr), COALESCE(SUM(o.snr),0), COALESCE(SUM(o.snr*o.snr),0),
		COALESCE(MIN(o.snr),0), COALESCE(MAX(o.snr),0),
		COUNT(o.rssi), COALESCE(SUM(o.rssi),0), COALESCE(SUM(o.rssi*o.rssi),0),
		COALESCE(MIN(o.rssi),0), COALESCE(MAX(o.rssi),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where

	row := db.conn.QueryRow(q, args...)
	if err := row.Scan(
		&agg.NObs,
		&agg.NSnr, &agg.SnrSum, &agg.SnrSumSq, &agg.SnrMin, &agg.SnrMax,
		&agg.NRssi, &agg.RssiSum, &agg.RssiSumSq, &agg.RssiMin, &agg.RssiMax,
	); err != nil {
		return agg, fmt.Errorf("rfCoreAggregates scan: %w", err)
	}
	return agg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestRFCoreAggregates -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): RF SQL core aggregates query"
```

---

## Task 6: Extract histogram + stats helpers to package level

The old `computeAnalyticsRF` defines `buildHistogramF64`, `buildHistogramInt`, `stddevF64` as local closures. Extract them to package-level functions so the SQL path reuses the **identical** logic — this guarantees histogram parity for free.

**Files:**
- Modify: `cmd/server/store_analytics.go` (`computeAnalyticsRF` body, lines ~286-506)
- Create: `cmd/server/analytics_stats.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFHistogramHelperPackageLevel(t *testing.T) {
	h := rfBuildHistogramF64([]float64{1, 2, 3, 4}, 2)
	bins, ok := h["bins"].([]map[string]interface{})
	if !ok || len(bins) != 2 {
		t.Fatalf("expected 2 bins, got %#v", h["bins"])
	}
	if h["min"].(float64) != 1 || h["max"].(float64) != 4 {
		t.Fatalf("min/max wrong: %#v", h)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFHistogramHelperPackageLevel -v`
Expected: FAIL — `rfBuildHistogramF64` undefined.

- [ ] **Step 3: Create package-level helpers**

Create `cmd/server/analytics_stats.go` — copy the closure bodies verbatim from `computeAnalyticsRF`:

```go
// analytics_stats.go — shared stat/histogram helpers used by both the
// in-memory and SQL analytics implementations. Logic is identical so the two
// paths produce byte-equal histograms.

package main

import "math"

func rfStddevF64(arr []float64, avg float64) float64 {
	if len(arr) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range arr {
		d := v - avg
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(arr)))
}

func rfBuildHistogramF64(values []float64, bins int) map[string]interface{} {
	if len(values) == 0 {
		return map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0}
	}
	mn, mx := values[0], values[0]
	for _, v := range values {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	rng := mx - mn
	if rng == 0 {
		rng = 1
	}
	binWidth := rng / float64(bins)
	counts := make([]int, bins)
	for _, v := range values {
		idx := int((v - mn) / binWidth)
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	binArr := make([]map[string]interface{}, bins)
	for i, c := range counts {
		binArr[i] = map[string]interface{}{"x": mn + float64(i)*binWidth, "w": binWidth, "count": c}
	}
	return map[string]interface{}{"bins": binArr, "min": mn, "max": mx}
}

func rfBuildHistogramInt(values []int, bins int) map[string]interface{} {
	if len(values) == 0 {
		return map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0}
	}
	mnI, mxI := values[0], values[0]
	for _, v := range values {
		if v < mnI {
			mnI = v
		}
		if v > mxI {
			mxI = v
		}
	}
	mn, mx := float64(mnI), float64(mxI)
	rng := mx - mn
	if rng == 0 {
		rng = 1
	}
	binWidth := rng / float64(bins)
	counts := make([]int, bins)
	for _, v := range values {
		idx := int((float64(v) - mn) / binWidth)
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	binArr := make([]map[string]interface{}, bins)
	for i, c := range counts {
		binArr[i] = map[string]interface{}{"x": mn + float64(i)*binWidth, "w": binWidth, "count": c}
	}
	return map[string]interface{}{"bins": binArr, "min": mn, "max": mx}
}
```

- [ ] **Step 4: Replace the closures in `computeAnalyticsRF` with calls to the package helpers**

In `cmd/server/store_analytics.go`, inside `computeAnalyticsRF`:
- Delete the local `stddevF64`, `buildHistogramF64`, `buildHistogramInt` closure definitions (lines ~286-296 and ~459-506).
- Replace usages: `stddevF64(` → `rfStddevF64(`, `buildHistogramF64(` → `rfBuildHistogramF64(`, `buildHistogramInt(` → `rfBuildHistogramInt(`.
- Keep `minF64`, `maxF64`, `minInt`, `maxInt` closures as-is (still used locally).

- [ ] **Step 5: Run tests to verify nothing regressed**

Run: `go test ./cmd/server/ -run 'TestRFHistogramHelperPackageLevel|AnalyticsRF' -v`
Expected: PASS — new test passes, existing RF analytics tests still pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/server/analytics_stats.go cmd/server/store_analytics.go cmd/server/analytics_rf_sql_test.go
git commit -m "refactor(analytics): extract histogram/stddev helpers to package level"
```

---

## Task 7: Sorted-column fetches (snr, rssi, packet sizes)

Fetch the sorted `snr` and `rssi` columns and the per-transmission packet sizes. These feed medians, histograms, and packet-size stats — reusing the Task 6 helpers for exact parity.

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFSortedColumns(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,7.0,-80.0,1779444000),(1,1,5.0,-90.0,1779444000)`)

	snr, err := rfSortedColumn(db, "snr", "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfSortedColumn snr: %v", err)
	}
	if len(snr) != 2 || snr[0] != 5.0 || snr[1] != 7.0 {
		t.Fatalf("snr column not sorted ascending: %v", snr)
	}

	sizes, err := rfPacketSizes(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfPacketSizes: %v", err)
	}
	if len(sizes) != 1 || sizes[0] != 2 { // 'aabb' = 4 hex chars / 2 = 2 bytes
		t.Fatalf("packet sizes wrong: %v", sizes)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFSortedColumns -v`
Expected: FAIL — `rfSortedColumn` / `rfPacketSizes` undefined.

- [ ] **Step 3: Implement the fetches**

Add to `cmd/server/analytics_rf_sql.go`:

```go
// rfSortedColumn fetches one non-null REAL column ("snr" or "rssi") from
// observations, ascending. Used for medians and histograms.
func rfSortedColumn(db *DB, col, region string, window TimeWindow) ([]float64, error) {
	if col != "snr" && col != "rssi" {
		return nil, fmt.Errorf("rfSortedColumn: unsupported column %q", col)
	}
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	var where string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args = rfWhere([]string{txW, "o." + col + " IS NOT NULL"},
			[][]interface{}{txArgs, nil})
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, args = rfWhere([]string{obsW, idxW, "o." + col + " IS NOT NULL"},
			[][]interface{}{obsArgs, idxArgs, nil})
	}
	q := `SELECT o.` + col + ` FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id ` + where +
		` ORDER BY o.` + col + ` ASC`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfSortedColumn query: %w", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("rfSortedColumn scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// rfPacketSizes returns one byte-size per transmission (len(raw_hex)/2),
// matching the old "unique by hash" set since hash is UNIQUE.
func rfPacketSizes(db *DB, region string, window TimeWindow) ([]int, error) {
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return nil, err
	}
	var q string
	var args []interface{}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, wArgs := rfWhere([]string{txW, "t.raw_hex != ''"},
			[][]interface{}{txArgs, nil})
		q = `SELECT length(t.raw_hex)/2 FROM transmissions t ` + where
		args = wArgs
	} else {
		obsW, obsArgs := rfObsWindowClause(window)
		idxW, idxArgs := rfObserverIdxClause(idxs)
		where, wArgs := rfWhere([]string{obsW, idxW, "t.raw_hex != ''"},
			[][]interface{}{obsArgs, idxArgs, nil})
		q = `SELECT DISTINCT t.id, length(t.raw_hex)/2 FROM transmissions t
			JOIN observations o ON o.transmission_id = t.id ` + where
		args = wArgs
	}
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfPacketSizes query: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		if region == "" {
			var sz int
			if err := rows.Scan(&sz); err != nil {
				return nil, fmt.Errorf("rfPacketSizes scan: %w", err)
			}
			out = append(out, sz)
		} else {
			var id, sz int
			if err := rows.Scan(&id, &sz); err != nil {
				return nil, fmt.Errorf("rfPacketSizes scan: %w", err)
			}
			out = append(out, sz)
		}
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestRFSortedColumns -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): RF SQL sorted-column and packet-size fetches"
```

---

## Task 8: Group-by queries (payload types, snr-by-type, hourly, signal-over-time)

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRFGroupByQueries(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccdd','h2','2026-05-18T10:30:00Z',1),
		       (3,'eeff','h3','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(2,0,7.0,-81.0,1779445800),(3,0,9.0,-82.0,1779447600)`)

	types, err := rfTypeDistribution(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfTypeDistribution: %v", err)
	}
	if types[1] != 2 || types[2] != 1 {
		t.Fatalf("type distribution wrong: %v", types)
	}

	hourly, err := rfHourlyBuckets(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("rfHourlyBuckets: %v", err)
	}
	if hourly["2026-05-18T10"].count != 2 || hourly["2026-05-18T11"].count != 1 {
		t.Fatalf("hourly buckets wrong: %v", hourly)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestRFGroupByQueries -v`
Expected: FAIL — `rfTypeDistribution` / `rfHourlyBuckets` undefined.

- [ ] **Step 3: Implement the group-by queries**

Add to `cmd/server/analytics_rf_sql.go`. These reuse a shared `rfObsWhere` builder:

```go
// rfObsWhere builds the WHERE clause for observation-scoped queries, in both
// region modes. Returns clause (incl. "WHERE") and args.
func rfObsWhere(db *DB, region string, window TimeWindow, extra string) (string, []interface{}, error) {
	idxs, err := rfRegionObserverIdxs(db, region)
	if err != nil {
		return "", nil, err
	}
	if region == "" {
		txW, txArgs := rfTxWindowClause(window)
		where, args := rfWhere([]string{txW, extra}, [][]interface{}{txArgs, nil})
		return where, args, nil
	}
	obsW, obsArgs := rfObsWindowClause(window)
	idxW, idxArgs := rfObserverIdxClause(idxs)
	where, args := rfWhere([]string{obsW, idxW, extra}, [][]interface{}{obsArgs, idxArgs, nil})
	return where, args, nil
}

// rfTypeDistribution returns payload_type -> distinct-transmission count.
func rfTypeDistribution(db *DB, region string, window TimeWindow) (map[int]int, error) {
	where, args, err := rfObsWhere(db, region, window, "t.payload_type IS NOT NULL")
	if err != nil {
		return nil, err
	}
	q := `SELECT t.payload_type, COUNT(DISTINCT t.id)
		FROM transmissions t JOIN observations o ON o.transmission_id = t.id
		` + where + ` GROUP BY t.payload_type`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfTypeDistribution query: %w", err)
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var pt, c int
		if err := rows.Scan(&pt, &c); err != nil {
			return nil, fmt.Errorf("rfTypeDistribution scan: %w", err)
		}
		out[pt] = c
	}
	return out, rows.Err()
}

// rfTypeStat aggregates snr per payload type.
type rfTypeStat struct {
	Count          int
	Sum, Min, Max  float64
}

// rfSnrByType returns payload_type -> snr stats.
func rfSnrByType(db *DB, region string, window TimeWindow) (map[int]rfTypeStat, error) {
	where, args, err := rfObsWhere(db, region, window, "o.snr IS NOT NULL")
	if err != nil {
		return nil, err
	}
	q := `SELECT t.payload_type, COUNT(o.snr), COALESCE(SUM(o.snr),0),
		COALESCE(MIN(o.snr),0), COALESCE(MAX(o.snr),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where + ` GROUP BY t.payload_type`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfSnrByType query: %w", err)
	}
	defer rows.Close()
	out := map[int]rfTypeStat{}
	for rows.Next() {
		var pt, c int
		var st rfTypeStat
		if err := rows.Scan(&pt, &c, &st.Sum, &st.Min, &st.Max); err != nil {
			return nil, fmt.Errorf("rfSnrByType scan: %w", err)
		}
		st.Count = c
		out[pt] = st
	}
	return out, rows.Err()
}

// rfHourBucket holds per-hour counts and snr.
type rfHourBucket struct {
	count    int
	snrCount int
	snrSum   float64
}

// rfHourlyBuckets returns hour ("2026-05-18T10") -> bucket. count is distinct
// transmissions per hour; snrSum/snrCount feed signal-over-time.
func rfHourlyBuckets(db *DB, region string, window TimeWindow) (map[string]rfHourBucket, error) {
	where, args, err := rfObsWhere(db, region, window, "")
	if err != nil {
		return nil, err
	}
	q := `SELECT strftime('%Y-%m-%dT%H', o.timestamp, 'unixepoch') AS hr,
		COUNT(DISTINCT o.transmission_id),
		COUNT(o.snr), COALESCE(SUM(o.snr),0)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		` + where + ` GROUP BY hr`
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rfHourlyBuckets query: %w", err)
	}
	defer rows.Close()
	out := map[string]rfHourBucket{}
	for rows.Next() {
		var hr string
		var b rfHourBucket
		if err := rows.Scan(&hr, &b.count, &b.snrCount, &b.snrSum); err != nil {
			return nil, fmt.Errorf("rfHourlyBuckets scan: %w", err)
		}
		out[hr] = b
	}
	return out, rows.Err()
}
```

> **Parity note for implementer:** the old code keys hours off the observation timestamp string `ts[:13]`. `strftime('%Y-%m-%dT%H', timestamp,'unixepoch')` produces the same `YYYY-MM-DDTHH` string. The old "packets per hour" dedups by `hash+hour`; `COUNT(DISTINCT o.transmission_id)` per hour is the SQL equivalent (transmission_id ↔ hash 1:1). The old `signalOverTime` count is the number of SNR observations per hour, not distinct — that is `COUNT(o.snr)`, captured here as `snrCount`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestRFGroupByQueries -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): RF SQL group-by queries (types, snr-by-type, hourly)"
```

---

## Task 9: Scatter sample + assemble `computeAnalyticsRFSQL`

Scatter stays as `[{snr,rssi}]` points (≤500) — the response shape must not change (spec non-goal). Use a `ROW_NUMBER()` stride to match the old "every Nth point" downsampling. Then assemble the full result map.

**Files:**
- Modify: `cmd/server/analytics_rf_sql.go`
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestComputeAnalyticsRFSQLShape(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1),(2,'ccdd','h2','2026-05-18T11:00:00Z',2)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000),(1,1,7.0,-90.0,1779444000),(2,0,9.0,-70.0,1779447600)`)

	res, err := computeAnalyticsRFSQL(db, "", TimeWindow{})
	if err != nil {
		t.Fatalf("computeAnalyticsRFSQL: %v", err)
	}
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"snr", "rssi", "snrValues", "rssiValues", "packetSizes", "packetsPerHour",
		"payloadTypes", "snrByType", "signalOverTime", "scatterData", "timeSpanHours"} {
		if _, ok := res[k]; !ok {
			t.Errorf("missing result key %q", k)
		}
	}
	if res["totalAllPackets"].(int) != 3 {
		t.Errorf("totalAllPackets=%v want 3", res["totalAllPackets"])
	}
	if res["totalPackets"].(int) != 3 {
		t.Errorf("totalPackets=%v want 3", res["totalPackets"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestComputeAnalyticsRFSQLShape -v`
Expected: FAIL — `computeAnalyticsRFSQL` undefined.

- [ ] **Step 3: Implement scatter + assembly**

Add to `cmd/server/analytics_rf_sql.go`. `payloadTypeNames` (`ptNames` in the old code) is the existing package var.

```go
// rfScatterPoint mirrors the old scatter point shape.
type rfScatterPoint struct {
	SNR  float64 `json:"snr"`
	RSSI float64 `json:"rssi"`
}

// rfScatterSample returns at most 500 (snr,rssi) points, strided to match the
// old "every Nth of all points in id order" downsampling.
func rfScatterSample(db *DB, region string, window TimeWindow) ([]rfScatterPoint, error) {
	where, args, err := rfObsWhere(db, region, window,
		"o.snr IS NOT NULL AND o.rssi IS NOT NULL")
	if err != nil {
		return nil, err
	}
	// Count first to compute the stride.
	var total int
	cq := `SELECT COUNT(*) FROM observations o
		JOIN transmissions t ON t.id = o.transmission_id ` + where
	if err := db.conn.QueryRow(cq, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("rfScatterSample count: %w", err)
	}
	if total == 0 {
		return []rfScatterPoint{}, nil
	}
	stride := 1
	if total > 500 {
		stride = total / 500
	}
	q := `SELECT snr, rssi FROM (
		SELECT o.snr AS snr, o.rssi AS rssi,
		       ROW_NUMBER() OVER (ORDER BY o.id) - 1 AS rn
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id ` + where +
		`) WHERE rn % ? = 0`
	qArgs := append(append([]interface{}{}, args...), stride)
	rows, err := db.conn.Query(q, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("rfScatterSample query: %w", err)
	}
	defer rows.Close()
	out := make([]rfScatterPoint, 0, 500)
	for rows.Next() {
		var p rfScatterPoint
		if err := rows.Scan(&p.SNR, &p.RSSI); err != nil {
			return nil, fmt.Errorf("rfScatterSample scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// rfTotalTransmissions returns the distinct-transmission count for the window/region.
func rfTotalTransmissions(db *DB, region string, window TimeWindow) (int, error) {
	where, args, err := rfObsWhere(db, region, window, "")
	if err != nil {
		return 0, err
	}
	q := `SELECT COUNT(DISTINCT t.id) FROM transmissions t
		JOIN observations o ON o.transmission_id = t.id ` + where
	var n int
	if err := db.conn.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("rfTotalTransmissions: %w", err)
	}
	return n, nil
}

// computeAnalyticsRFSQL is the SQL-backed equivalent of computeAnalyticsRF.
func computeAnalyticsRFSQL(db *DB, region string, window TimeWindow) (map[string]interface{}, error) {
	agg, err := rfCoreAggregates(db, region, window)
	if err != nil {
		return nil, err
	}
	snrVals, err := rfSortedColumn(db, "snr", region, window)
	if err != nil {
		return nil, err
	}
	rssiVals, err := rfSortedColumn(db, "rssi", region, window)
	if err != nil {
		return nil, err
	}
	sizes, err := rfPacketSizes(db, region, window)
	if err != nil {
		return nil, err
	}
	typeDist, err := rfTypeDistribution(db, region, window)
	if err != nil {
		return nil, err
	}
	snrByType, err := rfSnrByType(db, region, window)
	if err != nil {
		return nil, err
	}
	hourly, err := rfHourlyBuckets(db, region, window)
	if err != nil {
		return nil, err
	}
	scatter, err := rfScatterSample(db, region, window)
	if err != nil {
		return nil, err
	}
	totalTx, err := rfTotalTransmissions(db, region, window)
	if err != nil {
		return nil, err
	}

	ptNames := payloadTypeNames

	// SNR/RSSI stats — median is vals[len/2] on the sorted slice (old behavior).
	snrStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if len(snrVals) > 0 {
		avg := agg.SnrSum / float64(agg.NSnr)
		snrStats = map[string]interface{}{
			"min": snrVals[0], "max": snrVals[len(snrVals)-1],
			"avg": avg, "median": snrVals[len(snrVals)/2],
			"stddev": rfStddevF64(snrVals, avg),
		}
	}
	rssiStats := map[string]interface{}{"min": 0.0, "max": 0.0, "avg": 0.0, "median": 0.0, "stddev": 0.0}
	if len(rssiVals) > 0 {
		avg := agg.RssiSum / float64(agg.NRssi)
		rssiStats = map[string]interface{}{
			"min": rssiVals[0], "max": rssiVals[len(rssiVals)-1],
			"avg": avg, "median": rssiVals[len(rssiVals)/2],
			"stddev": rfStddevF64(rssiVals, avg),
		}
	}

	// Packets per hour — sorted by hour key.
	type hourCount struct {
		Hour  string `json:"hour"`
		Count int    `json:"count"`
	}
	hourKeys := make([]string, 0, len(hourly))
	for k := range hourly {
		hourKeys = append(hourKeys, k)
	}
	sortStrings(hourKeys)
	packetsPerHour := make([]hourCount, len(hourKeys))
	signalOverTime := make([]map[string]interface{}, 0, len(hourKeys))
	for i, k := range hourKeys {
		b := hourly[k]
		packetsPerHour[i] = hourCount{Hour: k, Count: b.count}
		if b.snrCount > 0 {
			signalOverTime = append(signalOverTime, map[string]interface{}{
				"hour": k, "count": b.snrCount, "avgSnr": b.snrSum / float64(b.snrCount),
			})
		}
	}

	// Payload types — desc by count.
	type ptEntry struct {
		Type  int    `json:"type"`
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	payloadTypes := make([]ptEntry, 0, len(typeDist))
	for pt, c := range typeDist {
		name := ptNames[pt]
		if name == "" {
			name = fmt.Sprintf("UNK(%d)", pt)
		}
		payloadTypes = append(payloadTypes, ptEntry{Type: pt, Name: name, Count: c})
	}
	sort.Slice(payloadTypes, func(i, j int) bool { return payloadTypes[i].Count > payloadTypes[j].Count })

	// SNR by type — desc by count.
	type snrTypeEntry struct {
		Name  string  `json:"name"`
		Count int     `json:"count"`
		Avg   float64 `json:"avg"`
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
	}
	snrByTypeArr := make([]snrTypeEntry, 0, len(snrByType))
	for pt, st := range snrByType {
		name := ptNames[pt]
		if name == "" {
			name = fmt.Sprintf("UNK(%d)", pt)
		}
		avg := 0.0
		if st.Count > 0 {
			avg = st.Sum / float64(st.Count)
		}
		snrByTypeArr = append(snrByTypeArr, snrTypeEntry{
			Name: name, Count: st.Count, Avg: avg, Min: st.Min, Max: st.Max,
		})
	}
	sort.Slice(snrByTypeArr, func(i, j int) bool { return snrByTypeArr[i].Count > snrByTypeArr[j].Count })

	// Packet-size stats.
	avgPkt, minPkt, maxPkt := 0, 0, 0
	if len(sizes) > 0 {
		sum := 0
		minPkt, maxPkt = sizes[0], sizes[0]
		for _, v := range sizes {
			sum += v
			if v < minPkt {
				minPkt = v
			}
			if v > maxPkt {
				maxPkt = v
			}
		}
		avgPkt = sum / len(sizes)
	}

	// Time span from the min/max observation timestamp. rfAggregates carries
	// MinTS/MaxTS — see Task 9 Step 4, which extends rfCoreAggregates.
	timeSpanHours := rfTimeSpanHours(agg.MinTS, agg.MaxTS)

	return map[string]interface{}{
		"totalPackets":       int(agg.NSnr),
		"totalAllPackets":    int(agg.NObs),
		"totalTransmissions": totalTx,
		"snr":                snrStats,
		"rssi":               rssiStats,
		"snrValues":          rfBuildHistogramF64(snrVals, 20),
		"rssiValues":         rfBuildHistogramF64(rssiVals, 20),
		"packetSizes":        rfBuildHistogramInt(sizes, 25),
		"minPacketSize":      minPkt,
		"maxPacketSize":      maxPkt,
		"avgPacketSize":      avgPkt,
		"packetsPerHour":     packetsPerHour,
		"payloadTypes":       payloadTypes,
		"snrByType":          snrByTypeArr,
		"signalOverTime":     signalOverTime,
		"scatterData":        scatter,
		"timeSpanHours":      timeSpanHours,
	}, nil
}
```

- [ ] **Step 4: Add the supporting helpers**

`rfCoreAggregates` must also return min/max timestamp. Extend `rfAggregates` with `MinTS, MaxTS int64`, add `COALESCE(MIN(o.timestamp),0), COALESCE(MAX(o.timestamp),0)` to the query and the `Scan`. Add to `cmd/server/analytics_rf_sql.go`:

```go
import "sort" // add to the existing import block

func sortStrings(s []string) { sort.Strings(s) }

func rfTimeSpanHours(minTS, maxTS int64) float64 {
	if minTS == 0 || maxTS == 0 || minTS == maxTS {
		return 0
	}
	return float64(maxTS-minTS) / 3600.0
}
```

The count-desc sorts are inlined with `sort.Slice` directly in `computeAnalyticsRFSQL` (Step 3 already shows this) — no helper needed, matching how `computeAnalyticsRF` does it.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/server/ -run TestComputeAnalyticsRFSQLShape -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/server/analytics_rf_sql.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): assemble computeAnalyticsRFSQL"
```

---

## Task 10: Wire into the cache wrapper and handler

`GetAnalyticsRFWithWindow` branches on the flag and returns an error; `GetAnalyticsRF` and `handleAnalyticsRF` are updated to match.

**Files:**
- Modify: `cmd/server/store_analytics.go` (`GetAnalyticsRF` ~17, `GetAnalyticsRFWithWindow` ~23-44)
- Modify: `cmd/server/routes.go` (`handleAnalyticsRF` ~1755)
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestGetAnalyticsRFWithWindowUsesSQLBackend(t *testing.T) {
	db := setupTestDB(t)
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aabb','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp)
		VALUES (1,0,5.0,-80.0,1779444000)`)
	ps := NewPacketStore(db, &PacketStoreConfig{AnalyticsSQLBackend: true})
	res, err := ps.GetAnalyticsRFWithWindow("", TimeWindow{})
	if err != nil {
		t.Fatalf("GetAnalyticsRFWithWindow: %v", err)
	}
	if res["totalAllPackets"].(int) != 1 {
		t.Fatalf("totalAllPackets=%v want 1", res["totalAllPackets"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/server/ -run TestGetAnalyticsRFWithWindowUsesSQLBackend -v`
Expected: FAIL — `GetAnalyticsRFWithWindow` returns one value, not `(map, error)`.

- [ ] **Step 3: Update the cache wrapper**

In `cmd/server/store_analytics.go`, change `GetAnalyticsRF` and `GetAnalyticsRFWithWindow`:

```go
func (s *PacketStore) GetAnalyticsRF(region string) (map[string]interface{}, error) {
	return s.GetAnalyticsRFWithWindow(region, TimeWindow{})
}

func (s *PacketStore) GetAnalyticsRFWithWindow(region string, window TimeWindow) (map[string]interface{}, error) {
	cacheKey := region
	if !window.IsZero() {
		cacheKey = region + "|" + window.CacheKey()
	}
	s.cacheMu.Lock()
	if cached, ok := s.rfCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		s.cacheHits++
		s.cacheMu.Unlock()
		return cached.data, nil
	}
	s.cacheMisses++
	s.cacheMu.Unlock()

	var result map[string]interface{}
	if s.analyticsSQLBackend {
		r, err := computeAnalyticsRFSQL(s.db, region, window)
		if err != nil {
			return nil, err
		}
		result = r
	} else {
		result = s.computeAnalyticsRF(region, window)
	}

	s.cacheMu.Lock()
	s.rfCache[cacheKey] = &cachedResult{data: result, expiresAt: time.Now().Add(s.rfCacheTTL)}
	s.cacheMu.Unlock()
	return result, nil
}
```

- [ ] **Step 4: Update the handler and any other callers**

In `cmd/server/routes.go`, `handleAnalyticsRF`:

```go
func (s *Server) handleAnalyticsRF(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	window := ParseTimeWindow(r)
	if s.store == nil {
		writeError(w, 503, "Packet store unavailable")
		return
	}
	res, err := s.store.GetAnalyticsRFWithWindow(region, window)
	if err != nil {
		log.Printf("[analytics] RF SQL backend error: %v", err)
		writeError(w, 500, "analytics query failed")
		return
	}
	writeJSON(w, res)
}
```

Then run `grep -rn "GetAnalyticsRF\b\|GetAnalyticsRFWithWindow" cmd/server/ --include=*.go` and update every other caller (including `_test.go` files) to handle the new `(map, error)` return. Existing tests that did `res := store.GetAnalyticsRF(...)` become `res, err := ...; if err != nil { t.Fatal(err) }`.

- [ ] **Step 5: Run the build and the test**

Run: `go build ./... && go test ./cmd/server/ -run 'AnalyticsRF|TestGetAnalyticsRFWithWindowUsesSQLBackend' -v`
Expected: build succeeds, tests PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/server/store_analytics.go cmd/server/routes.go cmd/server/analytics_rf_sql_test.go
git commit -m "feat(analytics): route RF analytics through SQL backend when flag set"
```

---

## Task 11: Full parity test

Compare the SQL backend against the in-memory implementation across the matrix: no-region/region × zero-window/24h-window × empty dataset.

**Files:**
- Test: `cmd/server/analytics_rf_sql_test.go`

- [ ] **Step 1: Write the parity test**

```go
func seedRFParityData(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aabbcc','h1','2026-05-18T10:00:00Z',1),
		(2,'ddee','h2','2026-05-18T10:30:00Z',1),
		(3,'ff00112233','h3','2026-05-18T23:00:00Z',2),
		(4,'4455','h4','2026-05-17T09:00:00Z',3)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,snr,rssi,timestamp) VALUES
		(1,0,5.0,-80.0,1779444000),(1,1,7.5,-88.0,1779444000),
		(2,0,6.0,-82.0,1779445800),(2,1,NULL,-95.0,1779445800),
		(3,0,9.0,-70.0,1779490800),(3,2,9.0,NULL,1779490800),
		(4,1,3.0,-101.0,1779354000)`)
}

func assertRFParity(t *testing.T, label string, region string, window TimeWindow, dbPath string) {
	t.Helper()
	memStore := loadStore(t, dbPath, 0)
	old := memStore.computeAnalyticsRF(region, window)
	sqlRes, err := computeAnalyticsRFSQL(memStore.db, region, window)
	if err != nil {
		t.Fatalf("[%s] SQL backend error: %v", label, err)
	}
	// Exact aggregates.
	for _, k := range []string{"totalPackets", "totalAllPackets", "totalTransmissions",
		"minPacketSize", "maxPacketSize", "avgPacketSize"} {
		if fmt.Sprint(old[k]) != fmt.Sprint(sqlRes[k]) {
			t.Errorf("[%s] %s: old=%v sql=%v", label, k, old[k], sqlRes[k])
		}
	}
	// SNR/RSSI stat maps.
	for _, grp := range []string{"snr", "rssi"} {
		om := old[grp].(map[string]interface{})
		sm := sqlRes[grp].(map[string]interface{})
		for _, stat := range []string{"min", "max", "avg", "median", "stddev"} {
			of := toF64(om[stat])
			sf := toF64(sm[stat])
			if math.Abs(of-sf) > 1e-9 {
				t.Errorf("[%s] %s.%s: old=%v sql=%v", label, grp, stat, of, sf)
			}
		}
	}
	// Histograms: compare bin counts.
	for _, k := range []string{"snrValues", "rssiValues", "packetSizes"} {
		assertHistogramEqual(t, label, k, old[k], sqlRes[k])
	}
}

func toF64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func assertHistogramEqual(t *testing.T, label, key string, oldH, sqlH interface{}) {
	t.Helper()
	om := oldH.(map[string]interface{})
	sm := sqlH.(map[string]interface{})
	ob, _ := om["bins"].([]map[string]interface{})
	sb, _ := sm["bins"].([]map[string]interface{})
	if len(ob) != len(sb) {
		t.Errorf("[%s] %s: bin count old=%d sql=%d", label, key, len(ob), len(sb))
		return
	}
	for i := range ob {
		if fmt.Sprint(ob[i]["count"]) != fmt.Sprint(sb[i]["count"]) {
			t.Errorf("[%s] %s bin %d: old count=%v sql=%v", label, key, i,
				ob[i]["count"], sb[i]["count"])
		}
	}
}

func TestRFAnalyticsParity(t *testing.T) {
	db := setupTestDB(t)
	seedRFParityData(t, db)
	dbPath := db.path

	cases := []struct {
		label  string
		region string
		window TimeWindow
	}{
		{"no-region/zero-window", "", TimeWindow{}},
		{"no-region/24h", "", TimeWindow{Since: "2026-05-18T00:00:00Z", Until: "2026-05-19T00:00:00Z"}},
	}
	for _, c := range cases {
		assertRFParity(t, c.label, c.region, c.window, dbPath)
	}
}

func TestRFAnalyticsParityEmpty(t *testing.T) {
	db := setupTestDB(t)
	assertRFParity(t, "empty-dataset", "", TimeWindow{}, db.path)
}
```

> **Region case:** add a `region` case to `TestRFAnalyticsParity` once you have confirmed (Task 4) which region value the seeded `observers` belong to. Seed observers with a known region in `seedRFParityData` and add `{"region/zero-window", "<thatRegion>", TimeWindow{}}`. The region path must reach parity too.

- [ ] **Step 2: Run the parity test**

Run: `go test ./cmd/server/ -run 'TestRFAnalyticsParity' -v`
Expected: PASS. If a stat diverges, the failure names the exact key — fix the SQL query or assembly until parity holds.

- [ ] **Step 3: Commit**

```bash
git add cmd/server/analytics_rf_sql_test.go
git commit -m "test(analytics): RF SQL vs in-memory parity tests"
```

---

## Task 12: Full build, test suite, and final commit

- [ ] **Step 1: Build everything**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 2: Run the full server test suite**

Run: `go test ./cmd/server/...`
Expected: PASS. Any failure here is a caller missed in Task 10 Step 4 — fix and re-run.

- [ ] **Step 3: Run go vet**

Run: `go vet ./cmd/server/...`
Expected: no warnings.

- [ ] **Step 4: Commit any remaining fixes**

```bash
git add -A cmd/server/
git commit -m "chore(analytics): finalize RF SQL backend — build + tests green"
```

---

## Rollout (post-merge, manual — not a code task)

1. Deploy. `packetStore.analyticsSqlBackend` defaults `false` — no behavior change.
2. On `analyzer.kiekr.app`, set `"analyticsSqlBackend": true` in the `packetStore` block of `config.json`, restart, eyeball the RF analytics page.
3. Once confirmed, change the default to `true` in a follow-up, delete `computeAnalyticsRF` and the flag.

---

## Self-review notes

- **Spec coverage:** architecture (Tasks 1,10), all 6 query types (Tasks 5,7,8,9), composite index (Task 2), error handling → 500 (Task 10), parity matrix (Task 11), rollout (documented). Covered.
- **Scatter:** spec section 2 said "2D histogram"; spec non-goals said "no response-shape change." The histogram would change `scatterData` from points to bins → frontend change. Resolved here: `scatterData` stays `[{snr,rssi}]` points via a strided `ROW_NUMBER()` sample. This is the spec-consistent choice (honors the non-goal); the design doc's section 2 wording should be treated as superseded by this task.
- **Open item for implementer:** the `observers` region column (Task 4) — confirm against `resolveRegionObservers` in `store.go`. The region parity case (Task 11) is the guard.
