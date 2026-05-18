package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestGetRecentDirectPacketsForNode verifies that the query returns only
// transmissions whose first observed path hop is a prefix of the node's pubkey,
// i.e. transmissions the node heard directly from the original sender.
func TestGetRecentDirectPacketsForNode(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	// Node aabbccdd11223344 — observations on transmission 1 have first hop
	// "aa", a prefix of the pubkey, so transmission 1 should be returned.
	packets, err := db.GetRecentDirectPacketsForNode("aabbccdd11223344", 20, 0)
	if err != nil {
		t.Fatalf("GetRecentDirectPacketsForNode: %v", err)
	}
	if len(packets) != 1 {
		t.Fatalf("expected 1 direct packet for aabbccdd11223344, got %d", len(packets))
	}
	if id, ok := packets[0]["id"].(int); !ok || id != 1 {
		t.Errorf("expected transmission id 1, got %v", packets[0]["id"])
	}
	// Observations should be attached.
	if obs, ok := packets[0]["observations"].([]map[string]interface{}); !ok || len(obs) == 0 {
		t.Errorf("expected observations attached, got %v", packets[0]["observations"])
	}
}

// TestGetRecentDirectPacketsForNode_NoMatch verifies a node that never appears
// as a first hop returns an empty (non-nil) slice.
func TestGetRecentDirectPacketsForNode_NoMatch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	// Node eeff00112233aabb appears only as second hop ("bb"), never first.
	packets, err := db.GetRecentDirectPacketsForNode("eeff00112233aabb", 20, 0)
	if err != nil {
		t.Fatalf("GetRecentDirectPacketsForNode: %v", err)
	}
	if packets == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(packets) != 0 {
		t.Errorf("expected 0 direct packets for eeff00112233aabb, got %d", len(packets))
	}
}

// TestGetRecentDirectPacketsForNode_LimitClamp verifies the limit is clamped to
// the documented 1..2000 range.
func TestGetRecentDirectPacketsForNode_LimitClamp(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	// limit <= 0 and limit > 2000 must not error (clamped internally).
	if _, err := db.GetRecentDirectPacketsForNode("aabbccdd11223344", 0, 0); err != nil {
		t.Fatalf("limit=0: %v", err)
	}
	if _, err := db.GetRecentDirectPacketsForNode("aabbccdd11223344", 999999, 0); err != nil {
		t.Fatalf("limit=999999: %v", err)
	}
}

// TestGetRecentDirectPacketsForNode_SinceFilter verifies the sinceHours filter
// is applied without error and a tight window excludes older rows.
func TestGetRecentDirectPacketsForNode_SinceFilter(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	seedTestData(t, db)

	// Transmission 1 has first_seen ~1h ago — a 24h window includes it.
	wide, err := db.GetRecentDirectPacketsForNode("aabbccdd11223344", 20, 24)
	if err != nil {
		t.Fatalf("since=24: %v", err)
	}
	if len(wide) != 1 {
		t.Errorf("24h window: expected 1 packet, got %d", len(wide))
	}
}

// TestHandleNodeDirectPackets_JSONShape verifies the HTTP endpoint returns the
// upstream-compatible {"packets": [...], "truncated": bool} response shape.
func TestHandleNodeDirectPackets_JSONShape(t *testing.T) {
	_, router := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/nodes/aabbccdd11223344/direct-packets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Packets   []map[string]interface{} `json:"packets"`
		Truncated bool                     `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if resp.Packets == nil {
		t.Error("expected non-nil packets array")
	}
	if len(resp.Packets) != 1 {
		t.Errorf("expected 1 direct packet, got %d", len(resp.Packets))
	}
	if resp.Truncated {
		t.Error("expected truncated=false when result count (1) < limit (20)")
	}
}

// TestHandleNodeDirectPackets_Truncated verifies truncated=true when the result
// count equals the requested limit.
func TestHandleNodeDirectPackets_Truncated(t *testing.T) {
	_, router := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/nodes/aabbccdd11223344/direct-packets?limit=1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Packets   []map[string]interface{} `json:"packets"`
		Truncated bool                     `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Packets) != 1 || !resp.Truncated {
		t.Errorf("limit=1: expected 1 packet + truncated=true, got %d packets truncated=%v",
			len(resp.Packets), resp.Truncated)
	}
}
