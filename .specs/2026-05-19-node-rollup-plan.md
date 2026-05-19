# Node-Health Rollup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pre-aggregate the per-node relay-activity + usefulness enrichment that makes `/api/nodes` ~13s, serving it from a `node_rollup` table.

**Architecture:** An hourly path-hop rollup (`node_rollup`, key `(hour, hop_key)`) counts distinct non-advert transmissions per hop. A background backfill + 5-min maintenance ticker keep it fresh. A bulk read path replaces the 50× per-node in-memory scans in `handleNodes` with one query. Gated by the existing `analyticsSqlBackend` flag; the in-memory functions stay as fallback + parity reference. Mirrors `rf_rollup*.go` / `channel_rollup*.go`.

**Tech Stack:** Go, SQLite (`database/sql`), per-directory Go modules.

---

## Conventions for every task

- All Go commands run from `cmd/server/`: `cd cmd/server && go test ...`.
- After editing any `.go` file, run `gofmt -l <file>` from `cmd/server/` — expect empty output. Do **not** `gofmt -w` pre-existing files.
- `git` commands run from the worktree root.
- Commits are signed (1Password). If a commit fails with a signing/vault error, stop and ask the user to unlock — do not bypass signing.
- Test helpers already in the package: `setupTestDBFile(t)` → returns a `*DB` with `.path` and `.conn` (v3 schema: `transmissions`, `observations`, `observers`); `mustExec(t, db, sql)`; `cachedRW(path)` → `*sql.DB`; `loadStore(t, path, retentionHours)` → `*PacketStore`.
- **Go imports:** Go rejects both *unused* and *undeclared* imports. Each task adds exactly the imports its new code uses — when a step's code first needs a package, that step's instructions say to add it to the file's `import` block. Never pre-import a package before the task that uses it.
- Fixture facts: `observations.timestamp` is epoch **seconds** (`1779098400` = `2026-05-18T10:00:00Z`); `transmissions.first_seen` is RFC3339 text; `transmissions.payload_type` 4 = ADVERT.

---

## Task 1: `node_rollup` schema

**Files:**
- Create: `cmd/server/node_rollup.go`
- Test: `cmd/server/node_rollup_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/server/node_rollup_test.go`:

```go
package main

import (
	"testing"
)

func TestEnsureNodeRollupTable(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatalf("ensureNodeRollupTable: %v", err)
	}
	for _, tbl := range []string{"node_rollup", "node_rollup_total", "node_rollup_meta"} {
		var n string
		if err := db.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil {
			t.Fatalf("table %s missing: %v", tbl, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/server && go test -run TestEnsureNodeRollupTable -v`
Expected: FAIL — `undefined: ensureNodeRollupTable`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/node_rollup.go`:

```go
// node_rollup.go — node-health rollup: schema, hop-key extraction,
// single-hour recompute. See .specs/2026-05-19-node-rollup-design.md.
// Mirrors rf_rollup.go / channel_rollup.go.

package main

import (
	"fmt"
)

// ensureNodeRollupTable creates the node-health rollup tables. Idempotent.
func ensureNodeRollupTable(dbPath string) error {
	rw, err := cachedRW(dbPath)
	if err != nil {
		return fmt.Errorf("open rw for node_rollup: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS node_rollup (
			hour TEXT NOT NULL,
			hop_key TEXT NOT NULL,
			relay_count INTEGER NOT NULL DEFAULT 0,
			last_relayed TEXT,
			PRIMARY KEY (hour, hop_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_node_rollup_hop ON node_rollup(hop_key)`,
		`CREATE INDEX IF NOT EXISTS idx_node_rollup_hour ON node_rollup(hour)`,
		`CREATE TABLE IF NOT EXISTS node_rollup_total (
			hour TEXT PRIMARY KEY,
			n_nonadvert INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS node_rollup_meta (
			key TEXT PRIMARY KEY, value TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := rw.Exec(s); err != nil {
			return fmt.Errorf("node_rollup ddl %q: %w", s, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cmd/server && go test -run TestEnsureNodeRollupTable -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git add cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git commit -m "feat(node-rollup): schema for node_rollup tables"
```

---

## Task 2: hop-key extraction helper

**Files:**
- Modify: `cmd/server/node_rollup.go` (append `nodeHopKeys`)
- Test: `cmd/server/node_rollup_test.go` (append)

`nodeHopKeys` turns one observation's raw `path_json` + `resolved_path` into the set of lowercased hop keys: the resolved full pubkey where a hop resolved, else the raw wire hop. `resolved_path` is a JSON array of nullable strings (`[]*string`); `path_json` a JSON array of raw hop strings. Resolution is positional.

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/node_rollup_test.go`:

```go
func TestNodeHopKeys(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		resolved string
		want     []string
	}{
		{"raw only", `["ab","cd"]`, ``, []string{"ab", "cd"}},
		{"resolved overrides", `["ab","cd"]`, `["ABCDEF",null]`, []string{"abcdef", "cd"}},
		{"null resolved falls back", `["ab"]`, `[null]`, []string{"ab"}},
		{"short resolved falls back", `["ab","cd"]`, `["ABCDEF"]`, []string{"abcdef", "cd"}},
		{"dedup within observation", `["ab","ab"]`, ``, []string{"ab"}},
		{"empty path", `[]`, ``, nil},
		{"empty resolved string", `["ab"]`, ``, []string{"ab"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nodeHopKeys(c.path, c.resolved)
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

Run: `cd cmd/server && go test -run TestNodeHopKeys -v`
Expected: FAIL — `undefined: nodeHopKeys`.

- [ ] **Step 3: Write minimal implementation**

First, update the `import` block of `cmd/server/node_rollup.go` to add `encoding/json` and `strings`:

```go
import (
	"encoding/json"
	"fmt"
	"strings"
)
```

Then append to `cmd/server/node_rollup.go`:

```go
// nodeHopKeys returns the lowercased hop keys for one observation: the
// resolved full pubkey where a hop resolved, else the raw wire hop. Each
// distinct key appears once. Resolution is positional — resolved_path[i]
// corresponds to path_json[i]; a null or missing entry falls back to the
// raw hop.
func nodeHopKeys(pathJSON, resolvedPath string) []string {
	raw := parsePathJSON(pathJSON)
	if len(raw) == 0 {
		return nil
	}
	var resolved []*string
	if resolvedPath != "" {
		_ = json.Unmarshal([]byte(resolvedPath), &resolved)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for i, rawHop := range raw {
		key := rawHop
		if i < len(resolved) && resolved[i] != nil && *resolved[i] != "" {
			key = *resolved[i]
		}
		key = strings.ToLower(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}
```

(`parsePathJSON` already exists in `store.go:4217`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cmd/server && go test -run TestNodeHopKeys -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git add cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git commit -m "feat(node-rollup): positional hop-key extraction"
```

---

## Task 3: single-hour recompute

**Files:**
- Modify: `cmd/server/node_rollup.go` (append `recomputeNodeRollupHour`)
- Test: `cmd/server/node_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/node_rollup_test.go`:

```go
func TestRecomputeNodeRollupHour(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Two non-advert transmissions in 2026-05-18T10, one advert (ignored).
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type) VALUES
		(1,'aa','h1','2026-05-18T10:00:00Z',1),
		(2,'bb','h2','2026-05-18T10:30:00Z',2),
		(3,'cc','h3','2026-05-18T10:45:00Z',4)`)
	// tx1 path ab,cd ; tx2 path ab ; tx3 (advert) path ab.
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json) VALUES
		(1,1,1779098400,'["ab","cd"]'),
		(2,1,1779100200,'["ab"]'),
		(3,1,1779100500,'["ab"]')`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recomputeNodeRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	// hop "ab": tx1 + tx2 = 2 (advert tx3 excluded). hop "cd": tx1 = 1.
	var ab, cd int
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&ab)
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='cd'`, "2026-05-18T10").Scan(&cd)
	if ab != 2 || cd != 1 {
		t.Fatalf("ab=%d cd=%d want 2 and 1", ab, cd)
	}
	var lastAb string
	rw.QueryRow(`SELECT last_relayed FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&lastAb)
	if lastAb != "2026-05-18T10:30:00Z" {
		t.Fatalf("last_relayed ab=%q want 2026-05-18T10:30:00Z", lastAb)
	}
	// n_nonadvert: tx1 + tx2 = 2.
	var total int
	rw.QueryRow(`SELECT n_nonadvert FROM node_rollup_total WHERE hour=?`, "2026-05-18T10").Scan(&total)
	if total != 2 {
		t.Fatalf("n_nonadvert=%d want 2", total)
	}
	// Idempotent: a second run yields the same numbers.
	if err := recomputeNodeRollupHour(rw, "2026-05-18T10"); err != nil {
		t.Fatalf("recompute 2: %v", err)
	}
	rw.QueryRow(`SELECT relay_count FROM node_rollup WHERE hour=? AND hop_key='ab'`, "2026-05-18T10").Scan(&ab)
	if ab != 2 {
		t.Fatalf("after rerun ab=%d want 2", ab)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/server && go test -run TestRecomputeNodeRollupHour -v`
Expected: FAIL — `undefined: recomputeNodeRollupHour`.

- [ ] **Step 3: Write minimal implementation**

First, update the `import` block of `cmd/server/node_rollup.go` to add `database/sql` and `time`:

```go
import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)
```

Then append to `cmd/server/node_rollup.go`:

```go
// recomputeNodeRollupHour rebuilds node_rollup + node_rollup_total for one
// hour bucket ("2026-05-18T10") from raw non-advert transmissions.
// Idempotent: deletes then re-inserts. The raw read runs OUTSIDE the write
// transaction and filters on the indexed first_seen RFC3339 range.
func recomputeNodeRollupHour(rw *sql.DB, hour string) error {
	ht, err := time.Parse("2006-01-02T15", hour)
	if err != nil {
		return fmt.Errorf("node recompute parse hour %q: %w", hour, err)
	}
	lo := ht.UTC().Format("2006-01-02T15:04:05Z")
	hi := ht.UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")

	rows, err := rw.Query(`
		SELECT t.id, t.first_seen, o.path_json, o.resolved_path
		FROM transmissions t JOIN observations o ON o.transmission_id = t.id
		WHERE t.first_seen >= ? AND t.first_seen < ?
		  AND (t.payload_type IS NULL OR t.payload_type != 4)`, lo, hi)
	if err != nil {
		return fmt.Errorf("node recompute scan: %w", err)
	}
	txByHop := map[string]map[int]bool{}
	lastByHop := map[string]string{}
	for rows.Next() {
		var txID int
		var firstSeen string
		var pathJSON, resolvedPath sql.NullString
		if err := rows.Scan(&txID, &firstSeen, &pathJSON, &resolvedPath); err != nil {
			rows.Close()
			return fmt.Errorf("node recompute row: %w", err)
		}
		for _, key := range nodeHopKeys(pathJSON.String, resolvedPath.String) {
			if txByHop[key] == nil {
				txByHop[key] = map[int]bool{}
			}
			txByHop[key][txID] = true
			if firstSeen > lastByHop[key] {
				lastByHop[key] = firstSeen
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("node recompute rows: %w", err)
	}

	var nNonAdvert int
	if err := rw.QueryRow(`
		SELECT COUNT(*) FROM transmissions
		WHERE first_seen >= ? AND first_seen < ?
		  AND (payload_type IS NULL OR payload_type != 4)`, lo, hi).Scan(&nNonAdvert); err != nil {
		return fmt.Errorf("node recompute count: %w", err)
	}

	tx, err := rw.Begin()
	if err != nil {
		return fmt.Errorf("node recompute begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_rollup WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("node recompute delete rollup: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM node_rollup_total WHERE hour=?`, hour); err != nil {
		return fmt.Errorf("node recompute delete total: %w", err)
	}
	for key, set := range txByHop {
		if _, err := tx.Exec(`INSERT INTO node_rollup(hour,hop_key,relay_count,last_relayed)
			VALUES (?,?,?,?)`, hour, key, len(set), lastByHop[key]); err != nil {
			return fmt.Errorf("node recompute insert rollup: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO node_rollup_total(hour,n_nonadvert) VALUES (?,?)`,
		hour, nNonAdvert); err != nil {
		return fmt.Errorf("node recompute insert total: %w", err)
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cmd/server && go test -run 'TestEnsureNodeRollupTable|TestNodeHopKeys|TestRecomputeNodeRollupHour' -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git add cmd/server/node_rollup.go cmd/server/node_rollup_test.go
git commit -m "feat(node-rollup): single-hour recompute"
```

---

## Task 4: backfill + watermark + maintenance

**Files:**
- Create: `cmd/server/node_rollup_maintain.go`
- Test: `cmd/server/node_rollup_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `cmd/server/node_rollup_test.go`:

```go
func TestNodeRollupMaintenance(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (1,'aa','h1','2026-05-18T10:00:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
		VALUES (1,1,1779098400,'["ab"]')`)
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if !(!nodeRollupReady(rw)) {
		t.Fatal("rollup should not be ready before first run")
	}
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 1: %v", err)
	}
	if !nodeRollupReady(rw) {
		t.Fatal("rollup should be ready after first run")
	}
	var ab int
	rw.QueryRow(`SELECT COALESCE(relay_count,0) FROM node_rollup WHERE hop_key='ab'`).Scan(&ab)
	if ab != 1 {
		t.Fatalf("after run 1 relay_count=%d want 1", ab)
	}
	// New observation on a new transmission in the same hour.
	mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
		VALUES (2,'bb','h2','2026-05-18T10:20:00Z',1)`)
	mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
		VALUES (2,1,1779099600,'["ab"]')`)
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatalf("maintenance 2: %v", err)
	}
	rw.QueryRow(`SELECT COALESCE(relay_count,0) FROM node_rollup WHERE hop_key='ab'`).Scan(&ab)
	if ab != 2 {
		t.Fatalf("after run 2 relay_count=%d want 2", ab)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/server && go test -run TestNodeRollupMaintenance -v`
Expected: FAIL — `undefined: nodeRollupReady` / `runNodeRollupMaintenance`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/node_rollup_maintain.go`:

```go
// node_rollup_maintain.go — node-health rollup backfill + incremental
// maintenance. Mirrors rf_rollup_maintain.go.

package main

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

const nodeRollupWatermarkKey = "node_rollup_last_obs_id"

func nodeRollupWatermark(rw *sql.DB) (int64, error) {
	var v string
	err := rw.QueryRow(`SELECT value FROM node_rollup_meta WHERE key=?`,
		nodeRollupWatermarkKey).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read node watermark: %w", err)
	}
	var id int64
	fmt.Sscan(v, &id)
	return id, nil
}

func nodeSetRollupWatermark(rw *sql.DB, id int64) error {
	_, err := rw.Exec(`INSERT INTO node_rollup_meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		nodeRollupWatermarkKey, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set node watermark: %w", err)
	}
	return nil
}

// nodeRollupMaintMu serializes backfill vs the periodic maintenance job.
var nodeRollupMaintMu sync.Mutex

func runNodeRollupMaintenanceGuarded(rw *sql.DB) {
	if !nodeRollupMaintMu.TryLock() {
		log.Printf("[node-rollup] maintenance skipped — run already in progress")
		return
	}
	defer nodeRollupMaintMu.Unlock()
	if err := runNodeRollupMaintenance(rw); err != nil {
		log.Printf("[node-rollup] maintenance: %v", err)
	}
}

// runNodeRollupMaintenance recomputes every hour bucket whose transmissions
// were touched by observations newer than the watermark, then advances it.
// The watermark tracks observations.id so a new observation on an OLD
// transmission still re-rolls that transmission's hour.
func runNodeRollupMaintenance(rw *sql.DB) error {
	wm, err := nodeRollupWatermark(rw)
	if err != nil {
		return err
	}
	rows, err := rw.Query(`
		SELECT DISTINCT strftime('%Y-%m-%dT%H', t.first_seen)
		FROM observations o JOIN transmissions t ON t.id = o.transmission_id
		WHERE o.id > ?`, wm)
	if err != nil {
		return fmt.Errorf("node maintenance touched-hours: %w", err)
	}
	var hours []string
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return fmt.Errorf("node maintenance scan hour: %w", err)
		}
		if h.Valid && h.String != "" {
			hours = append(hours, h.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("node maintenance touched-hours err: %w", err)
	}
	for _, h := range hours {
		if err := recomputeNodeRollupHour(rw, h); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	var maxID sql.NullInt64
	if err := rw.QueryRow(`SELECT MAX(id) FROM observations`).Scan(&maxID); err != nil {
		return fmt.Errorf("node maintenance max id: %w", err)
	}
	if maxID.Valid && maxID.Int64 > wm {
		return nodeSetRollupWatermark(rw, maxID.Int64)
	}
	// Still record the watermark key on an empty DB so nodeRollupReady flips.
	if !maxID.Valid {
		return nodeSetRollupWatermark(rw, 0)
	}
	return nil
}

// backfillNodeRollupAsync runs the first full rollup build in the background.
// Backfill is maintenance from watermark 0 (all hours touched).
func backfillNodeRollupAsync(dbPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[node-rollup] backfill panic recovered: %v", r)
		}
	}()
	rw, err := cachedRW(dbPath)
	if err != nil {
		log.Printf("[node-rollup] backfill open rw: %v", err)
		return
	}
	nodeRollupMaintMu.Lock()
	defer nodeRollupMaintMu.Unlock()
	start := time.Now()
	if err := runNodeRollupMaintenance(rw); err != nil {
		log.Printf("[node-rollup] backfill failed: %v", err)
		return
	}
	log.Printf("[node-rollup] backfill complete in %s", time.Since(start))
}

// nodeRollupReady reports whether the rollup has been populated at least once.
func nodeRollupReady(rw *sql.DB) bool {
	var n int
	if err := rw.QueryRow(`SELECT COUNT(*) FROM node_rollup_meta WHERE key=?`,
		nodeRollupWatermarkKey).Scan(&n); err != nil {
		return false
	}
	return n > 0
}
```

Note: unlike `rf_rollup_maintain.go`, this writes the watermark key even when `observations` is empty (`maxID` invalid), so `nodeRollupReady` flips to true after a backfill on a fresh DB. (The RF rollup never hits an empty DB in practice.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cmd/server && go test -run TestNodeRollupMaintenance -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/node_rollup_maintain.go cmd/server/node_rollup_test.go
git add cmd/server/node_rollup_maintain.go cmd/server/node_rollup_test.go
git commit -m "feat(node-rollup): backfill, watermark, maintenance"
```

---

## Task 5: rollup read path

**Files:**
- Create: `cmd/server/node_rollup_read.go`
- Test: `cmd/server/node_rollup_test.go` (append)

`computeNodeRelayFromRollup` takes the page's pubkeys and returns, per pubkey, a `RepeaterRelayInfo` + usefulness score, all from `node_rollup` in one query.

- [ ] **Step 1: Write the failing test**

First, update the `import` block of `cmd/server/node_rollup_test.go` to add `time`:

```go
import (
	"testing"
	"time"
)
```

Then append to `cmd/server/node_rollup_test.go`:

```go
func TestComputeNodeRelayFromRollup(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Current hour so the 24h/7d windows include it.
	hour := time.Now().UTC().Format("2006-01-02T15")
	fs := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	// node "ab00...": appears as hop "ab" in 3 non-advert tx this hour.
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT INTO node_rollup(hour,hop_key,relay_count,last_relayed)
		VALUES (?,?,?,?)`, hour, "ab", 3, fs); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(`INSERT INTO node_rollup_total(hour,n_nonadvert) VALUES (?,?)`,
		hour, 12); err != nil {
		t.Fatal(err)
	}
	pk := "ab00000000000000000000000000000000000000000000000000000000000000"
	res, err := computeNodeRelayFromRollup(db, []string{pk}, 24)
	if err != nil {
		t.Fatalf("computeNodeRelayFromRollup: %v", err)
	}
	r, ok := res[pk]
	if !ok {
		t.Fatalf("missing pubkey result")
	}
	if r.Relay.RelayCount24h != 3 {
		t.Errorf("RelayCount24h=%d want 3", r.Relay.RelayCount24h)
	}
	if !r.Relay.RelayActive {
		t.Errorf("RelayActive=false want true")
	}
	if r.Relay.LastRelayed != fs {
		t.Errorf("LastRelayed=%q want %q", r.Relay.LastRelayed, fs)
	}
	want := 3.0 / 12.0
	if r.Usefulness < want-0.0001 || r.Usefulness > want+0.0001 {
		t.Errorf("Usefulness=%v want %v", r.Usefulness, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/server && go test -run TestComputeNodeRelayFromRollup -v`
Expected: FAIL — `undefined: computeNodeRelayFromRollup`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/server/node_rollup_read.go`:

```go
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
```

(`rfIntPlaceholders` is in `rf_rollup_read.go`; `parseRelayTS` in `repeater_liveness.go`; `RepeaterRelayInfo` in `repeater_liveness.go`. The `analyticsSQLBackend` field and `s.db` are existing `PacketStore` fields — confirm the exact field name is `analyticsSQLBackend` by grepping `store.go`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd cmd/server && go test -run TestComputeNodeRelayFromRollup -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/node_rollup_read.go cmd/server/node_rollup_test.go
git add cmd/server/node_rollup_read.go cmd/server/node_rollup_test.go
git commit -m "feat(node-rollup): bulk read path + cache/flag wrapper"
```

---

## Task 6: route handleNodes through GetBulkNodeRelay

**Files:**
- Modify: `cmd/server/routes.go:1248-1268`

This replaces the per-node `GetRepeaterRelayInfo` / `GetRepeaterUsefulnessScore` calls inside the `handleNodes` enrichment loop with one bulk call.

- [ ] **Step 1: Read the current code**

Read `cmd/server/routes.go:1248-1268`. The current block:

```go
	if s.store != nil {
		hashInfo := s.store.GetNodeHashSizeInfo()
		mbCap := s.store.GetMultiByteCapMap()
		relayWindow := s.cfg.GetHealthThresholds().RelayActiveHours
		for _, node := range nodes {
			if pk, ok := node["public_key"].(string); ok {
				EnrichNodeWithHashSize(node, hashInfo[pk])
				EnrichNodeWithMultiByte(node, mbCap[pk])
				if role, _ := node["role"].(string); role == "repeater" || role == "room" {
					info := s.store.GetRepeaterRelayInfo(pk, relayWindow)
					if info.LastRelayed != "" {
						node["last_relayed"] = info.LastRelayed
					}
					node["relay_active"] = info.RelayActive
					node["relay_count_1h"] = info.RelayCount1h
					node["relay_count_24h"] = info.RelayCount24h
					node["usefulness_score"] = s.store.GetRepeaterUsefulnessScore(pk)
				}
			}
		}
	}
```

- [ ] **Step 2: Replace it**

Replace the whole block above with:

```go
	if s.store != nil {
		hashInfo := s.store.GetNodeHashSizeInfo()
		mbCap := s.store.GetMultiByteCapMap()
		relayWindow := s.cfg.GetHealthThresholds().RelayActiveHours
		// Collect repeater/room pubkeys for one bulk relay/usefulness lookup
		// instead of a per-node scan (the /api/nodes hot path, issue: ~13s).
		var relayPubkeys []string
		for _, node := range nodes {
			if pk, ok := node["public_key"].(string); ok {
				if role, _ := node["role"].(string); role == "repeater" || role == "room" {
					relayPubkeys = append(relayPubkeys, pk)
				}
			}
		}
		relayInfo := s.store.GetBulkNodeRelay(relayPubkeys, relayWindow)
		for _, node := range nodes {
			if pk, ok := node["public_key"].(string); ok {
				EnrichNodeWithHashSize(node, hashInfo[pk])
				EnrichNodeWithMultiByte(node, mbCap[pk])
				if r, ok := relayInfo[pk]; ok {
					if r.Relay.LastRelayed != "" {
						node["last_relayed"] = r.Relay.LastRelayed
					}
					node["relay_active"] = r.Relay.RelayActive
					node["relay_count_1h"] = r.Relay.RelayCount1h
					node["relay_count_24h"] = r.Relay.RelayCount24h
					node["usefulness_score"] = r.Usefulness
				}
			}
		}
	}
```

(`relayInfo` only contains entries for repeater/room pubkeys, so the `r, ok :=` check replaces the old `role ==` check.)

- [ ] **Step 3: Build and run the route tests**

Run: `cd cmd/server && go build ./... && go test -run 'TestHandleNodes|Nodes' -v`
Expected: build OK; existing node-route tests PASS (the JSON shape is unchanged).

- [ ] **Step 4: Commit**

```bash
gofmt -l cmd/server/routes.go
git add cmd/server/routes.go
git commit -m "feat(node-rollup): route handleNodes relay enrichment through the bulk path"
```

---

## Task 7: wire schema + backfill + maintenance into main.go

**Files:**
- Modify: `cmd/server/main.go:248` (ensure table) and `cmd/server/main.go:643` (backfill + ticker)

- [ ] **Step 1: Add the ensure-table call**

In `cmd/server/main.go`, after the `ensureChannelRollupTable` block (around line 248), add:

```go
	if err := ensureNodeRollupTable(dbPath); err != nil {
		log.Fatalf("ensureNodeRollupTable: %v", err)
	}
```

So the block reads:

```go
	if err := ensureRFRollupTable(dbPath); err != nil {
		log.Fatalf("ensureRFRollupTable: %v", err)
	}
	if err := ensureChannelRollupTable(dbPath); err != nil {
		log.Fatalf("ensureChannelRollupTable: %v", err)
	}
	if err := ensureNodeRollupTable(dbPath); err != nil {
		log.Fatalf("ensureNodeRollupTable: %v", err)
	}
```

- [ ] **Step 2: Add the backfill + maintenance ticker**

In `cmd/server/main.go`, inside the `if cfg.PacketStore != nil && cfg.PacketStore.AnalyticsSQLBackend {` block, after the channel-rollup ticker goroutine (around line 643, just before the closing `}` of that block), add:

```go
		go backfillNodeRollupAsync(dbPath)
		nodeRollupTicker := time.NewTicker(5 * time.Minute)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[node-rollup] maintenance panic recovered: %v", r)
				}
			}()
			for range nodeRollupTicker.C {
				rw, err := cachedRW(dbPath)
				if err != nil {
					log.Printf("[node-rollup] maintenance open rw: %v", err)
					continue
				}
				runNodeRollupMaintenanceGuarded(rw)
			}
		}()
```

- [ ] **Step 3: Build**

Run: `cd cmd/server && go build ./...`
Expected: build OK.

- [ ] **Step 4: Run the full package test suite**

Run: `cd cmd/server && go test ./... 2>&1 | tail -20`
Expected: `ok` — no regressions.

- [ ] **Step 5: Commit**

```bash
gofmt -l cmd/server/main.go
git add cmd/server/main.go
git commit -m "feat(node-rollup): wire schema, backfill, maintenance into startup"
```

---

## Task 8: parity test — rollup vs in-memory

**Files:**
- Test: `cmd/server/node_rollup_test.go` (append)

Verifies `GetBulkNodeRelay` via the rollup matches the in-memory `GetRepeaterRelayInfo` for `RelayCount24h` on a fixture with raw-prefix-only paths (no `resolved_path`), where both paths key hops identically.

- [ ] **Step 1: Write the test**

First, update the `import` block of `cmd/server/node_rollup_test.go` to add `fmt`:

```go
import (
	"fmt"
	"testing"
	"time"
)
```

Then append to `cmd/server/node_rollup_test.go`:

```go
func TestNodeRollupParity(t *testing.T) {
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	// Three non-advert transmissions in the current hour, each routed
	// through hop "ab" (the 1-byte prefix of the node under test).
	hour := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		fs := hour.Add(time.Duration(i) * time.Minute).Format("2006-01-02T15:04:05Z")
		ts := hour.Add(time.Duration(i) * time.Minute).Unix()
		mustExec(t, db, `INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
			VALUES (?,?,?,?,1)`, i, "aa", fmt.Sprintf("h%d", i), fs)
		mustExec(t, db, `INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
			VALUES (?,1,?,'["ab"]')`, i, ts)
	}
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	pk := "ab00000000000000000000000000000000000000000000000000000000000000"

	// In-memory reference.
	ps := loadStore(t, db.path, 0)
	memInfo := ps.GetRepeaterRelayInfo(pk, 24)

	// Rollup path.
	ps.analyticsSQLBackend = true
	bulk := ps.GetBulkNodeRelay([]string{pk}, 24)
	rollupInfo := bulk[pk].Relay

	if rollupInfo.RelayCount24h != memInfo.RelayCount24h {
		t.Fatalf("RelayCount24h rollup=%d in-memory=%d",
			rollupInfo.RelayCount24h, memInfo.RelayCount24h)
	}
	if rollupInfo.RelayCount24h != 3 {
		t.Fatalf("RelayCount24h=%d want 3", rollupInfo.RelayCount24h)
	}
}
```

- [ ] **Step 2: Run the test**

Run: `cd cmd/server && go test -run TestNodeRollupParity -v`
Expected: PASS. If `RelayCount24h` differs, the recompute or read window math is wrong — fix before continuing.

- [ ] **Step 3: Commit**

```bash
gofmt -l cmd/server/node_rollup_test.go
git add cmd/server/node_rollup_test.go
git commit -m "test(node-rollup): rollup vs in-memory parity"
```

---

## Task 9: performance benchmark

**Files:**
- Test: `cmd/server/node_rollup_test.go` (append)

- [ ] **Step 1: Write the perf test**

Append to `cmd/server/node_rollup_test.go`:

```go
func TestNodeRollupPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	db := setupTestDBFile(t)
	if err := ensureNodeRollupTable(db.path); err != nil {
		t.Fatal(err)
	}
	rw, err := cachedRW(db.path)
	if err != nil {
		t.Fatal(err)
	}
	// ~60k non-advert transmissions spread over ~7 days, each routed through
	// one of 200 distinct 1-byte hop prefixes.
	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	tx, err := rw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 60000; i++ {
		fs := base.Add(time.Duration(i) * 9 * time.Second).Format("2006-01-02T15:04:05Z")
		hop := fmt.Sprintf("%02x", i%200)
		if _, err := tx.Exec(`INSERT INTO transmissions(id,raw_hex,hash,first_seen,payload_type)
			VALUES (?,?,?,?,1)`, i, "aa", fmt.Sprintf("h%d", i), fs); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO observations(transmission_id,observer_idx,timestamp,path_json)
			VALUES (?,1,?,?)`, i, base.Add(time.Duration(i)*9*time.Second).Unix(),
			`["`+hop+`"]`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := runNodeRollupMaintenance(rw); err != nil {
		t.Fatal(err)
	}

	// Build 50 pubkeys whose prefixes are in the rolled-up set.
	pubkeys := make([]string, 50)
	for i := range pubkeys {
		pubkeys[i] = fmt.Sprintf("%02x", i) +
			"00000000000000000000000000000000000000000000000000000000000000"
	}
	start := time.Now()
	res, err := computeNodeRelayFromRollup(db, pubkeys, 24)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 50 {
		t.Fatalf("got %d results want 50", len(res))
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("bulk read took %s, want < 500ms", elapsed)
	}
	t.Logf("bulk read of 50 pubkeys over 60k-tx rollup: %s", elapsed)
}
```

- [ ] **Step 2: Run the perf test**

Run: `cd cmd/server && go test -run TestNodeRollupPerf -v`
Expected: PASS; logged read time well under 500ms.

- [ ] **Step 3: Commit**

```bash
gofmt -l cmd/server/node_rollup_test.go
git add cmd/server/node_rollup_test.go
git commit -m "test(node-rollup): bulk-read performance benchmark"
```

---

## Task 10: final full-suite verification

- [ ] **Step 1: Run the whole package suite**

Run: `cd cmd/server && go test ./... 2>&1 | tail -20`
Expected: `ok github.com/corescope/server` — every test passing, no regressions in route/store tests.

- [ ] **Step 2: gofmt audit of every touched file**

Run: `cd cmd/server && gofmt -l node_rollup.go node_rollup_maintain.go node_rollup_read.go node_rollup_test.go routes.go main.go`
Expected: empty output.

- [ ] **Step 3: confirm no stray changes**

Run: `git status --short` (from worktree root)
Expected: only the intended new/modified files; nothing uncommitted.

---

## Self-review notes (spec coverage)

- Schema (`node_rollup`, `node_rollup_total`, `node_rollup_meta`, indexes) → Task 1.
- Positional `resolved_path` ?? `path_json` hop keys → Task 2.
- Single-hour recompute, indexed `first_seen` range, raw read outside the write tx, short write tx, nullable columns scanned as `sql.Null*` → Task 3.
- Watermark on `observations.id`, touched-hours, 50ms yield, guard mutex, backfill, `nodeRollupReady` → Task 4.
- Bulk read, 7-day usefulness window, prefix folding, `RelayCount1h/24h`, `RelayActive`, cache/flag wrapper with in-memory fallback → Task 5.
- `handleNodes` one bulk call → Task 6.
- `main.go` wiring, flag-gated → Task 7.
- Parity + perf gate → Tasks 8-9.
- Full-suite regression check → Task 10.

**Deploy (after merge to main):** Coolify CLI, app uuid `yngsizj96krk25x05a08u8ib`, `coolify deploy uuid <uuid> --force`. The flag is already on → the deploy auto-runs the node-rollup backfill. Watch `coolify app logs yngsizj96krk25x05a08u8ib` for `[node-rollup] backfill complete` and **no SQLITE_BUSY storm**. Verify `/api/nodes` latency live.
