package main

import (
	"reflect"
	"testing"
	"time"
)

// fleetSkewTestStore builds a PacketStore with one node that has clock-skew data.
// GetFleetClockSkew needs a non-nil DB (it consults the node name/role cache),
// so the store is backed by the shared in-memory test DB fixture.
func fleetSkewTestStore(t *testing.T) *PacketStore {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	ps := NewPacketStore(db, nil)
	pt := 4 // ADVERT
	tx1 := &StoreTx{
		Hash:        "fleet-hash1",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700002320}}`,
		Observations: []*StoreObs{
			{ObserverID: "obs1", Timestamp: "2023-11-14T22:50:00Z"},
			{ObserverID: "obs2", Timestamp: "2023-11-14T22:50:00Z"},
		},
	}
	tx2 := &StoreTx{
		Hash:        "fleet-hash2",
		PayloadType: &pt,
		DecodedJSON: `{"payload":{"timestamp":1700005920}}`,
		Observations: []*StoreObs{
			{ObserverID: "obs1", Timestamp: "2023-11-14T23:50:00Z"},
			{ObserverID: "obs2", Timestamp: "2023-11-14T23:50:00Z"},
		},
	}
	ps.mu.Lock()
	ps.byNode["AABB"] = []*StoreTx{tx1, tx2}
	ps.byPayloadType[4] = []*StoreTx{tx1, tx2}
	ps.clockSkew.computeInterval = 0 // force initial recompute
	ps.mu.Unlock()
	return ps
}

// TestGetFleetClockSkew_CacheHit verifies a second call within the 30s TTL
// returns the identical cached slice without recomputing.
func TestGetFleetClockSkew_CacheHit(t *testing.T) {
	ps := fleetSkewTestStore(t)

	first := ps.GetFleetClockSkew()
	if len(first) == 0 {
		t.Fatal("expected fleet clock-skew results on first call")
	}

	// Mutate byNode after the first call. A cache hit must NOT observe this
	// change; a recompute would (it iterates s.byNode).
	ps.mu.Lock()
	delete(ps.byNode, "AABB")
	ps.mu.Unlock()

	second := ps.GetFleetClockSkew()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("expected cached result identical to first call; first=%d second=%d", len(first), len(second))
	}

	// Confirm the cache fields were populated.
	ps.clockSkew.fleetCacheMu.Lock()
	cached := ps.clockSkew.fleetCache
	cachedAt := ps.clockSkew.fleetCachedAt
	ps.clockSkew.fleetCacheMu.Unlock()
	if cached == nil {
		t.Error("expected fleetCache to be populated")
	}
	if cachedAt.IsZero() {
		t.Error("expected fleetCachedAt to be set")
	}
}

// TestGetFleetClockSkew_CacheExpiry verifies that once the cache is stale the
// result is recomputed and reflects the current store state.
func TestGetFleetClockSkew_CacheExpiry(t *testing.T) {
	ps := fleetSkewTestStore(t)

	first := ps.GetFleetClockSkew()
	if len(first) == 0 {
		t.Fatal("expected fleet clock-skew results on first call")
	}

	// Expire the cache by backdating fleetCachedAt past the 30s TTL.
	ps.clockSkew.fleetCacheMu.Lock()
	ps.clockSkew.fleetCachedAt = time.Now().Add(-31 * time.Second)
	ps.clockSkew.fleetCacheMu.Unlock()

	// Remove the node so a recompute yields an empty fleet.
	ps.mu.Lock()
	delete(ps.byNode, "AABB")
	ps.mu.Unlock()

	second := ps.GetFleetClockSkew()
	if len(second) != 0 {
		t.Fatalf("expected empty fleet after expiry + node removal, got %d", len(second))
	}
}
