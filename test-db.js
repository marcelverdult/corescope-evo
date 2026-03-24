'use strict';

// Test db.js functions with a temp database
const path = require('path');
const fs = require('fs');
const os = require('os');

const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'meshcore-db-test-'));
const dbPath = path.join(tmpDir, 'test.db');
process.env.DB_PATH = dbPath;

// Now require db.js — it will use our temp DB
const db = require('./db');

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log(`  ✅ ${msg}`); }
  else { failed++; console.error(`  ❌ ${msg}`); }
}

function cleanup() {
  try { db.db.close(); } catch {}
  try { fs.rmSync(tmpDir, { recursive: true }); } catch {}
}

console.log('── db.js tests ──\n');

// --- Schema ---
console.log('Schema:');
{
  const tables = db.db.prepare("SELECT name FROM sqlite_master WHERE type='table'").all().map(r => r.name);
  assert(tables.includes('nodes'), 'nodes table exists');
  assert(tables.includes('observers'), 'observers table exists');
  assert(tables.includes('transmissions'), 'transmissions table exists');
  assert(tables.includes('observations'), 'observations table exists');
}

// --- upsertNode ---
console.log('\nupsertNode:');
{
  db.upsertNode({ public_key: 'aabbccdd11223344aabbccdd11223344', name: 'TestNode', role: 'repeater', lat: 37.0, lon: -122.0 });
  const node = db.getNode('aabbccdd11223344aabbccdd11223344');
  assert(node !== null, 'node inserted');
  assert(node.name === 'TestNode', 'name correct');
  assert(node.role === 'repeater', 'role correct');
  assert(node.lat === 37.0, 'lat correct');

  // Update
  db.upsertNode({ public_key: 'aabbccdd11223344aabbccdd11223344', name: 'UpdatedNode', role: 'room' });
  const node2 = db.getNode('aabbccdd11223344aabbccdd11223344');
  assert(node2.name === 'UpdatedNode', 'name updated');
  assert(node2.advert_count === 2, 'advert_count incremented');
}

// --- upsertObserver ---
console.log('\nupsertObserver:');
{
  db.upsertObserver({ id: 'obs-1', name: 'Observer One', iata: 'SFO' });
  const observers = db.getObservers();
  assert(observers.length >= 1, 'observer inserted');
  assert(observers.some(o => o.id === 'obs-1'), 'observer found by id');
  assert(observers.find(o => o.id === 'obs-1').name === 'Observer One', 'observer name correct');

  // Upsert again
  db.upsertObserver({ id: 'obs-1', name: 'Observer Updated' });
  const obs2 = db.getObservers().find(o => o.id === 'obs-1');
  assert(obs2.name === 'Observer Updated', 'observer name updated');
  assert(obs2.packet_count === 2, 'packet_count incremented');
}

// --- updateObserverStatus ---
console.log('\nupdateObserverStatus:');
{
  db.updateObserverStatus({ id: 'obs-2', name: 'Status Observer', iata: 'LAX', model: 'T-Deck' });
  const obs = db.getObservers().find(o => o.id === 'obs-2');
  assert(obs !== null, 'observer created via status update');
  assert(obs.model === 'T-Deck', 'model set');
  assert(obs.packet_count === 0, 'packet_count stays 0 for status update');
}

// --- insertTransmission ---
console.log('\ninsertTransmission:');
{
  const result = db.insertTransmission({
    raw_hex: '0400aabbccdd',
    hash: 'hash-001',
    timestamp: '2025-01-01T00:00:00Z',
    observer_id: 'obs-1',
    observer_name: 'Observer One',
    direction: 'rx',
    snr: 10.5,
    rssi: -85,
    route_type: 1,
    payload_type: 4,
    payload_version: 1,
    path_json: '["aabb","ccdd"]',
    decoded_json: '{"type":"ADVERT","pubKey":"aabbccdd11223344aabbccdd11223344","name":"TestNode"}',
  });
  assert(result !== null, 'transmission inserted');
  assert(result.transmissionId > 0, 'has transmissionId');
  assert(result.observationId > 0, 'has observationId');

  // Duplicate hash = same transmission, new observation
  const result2 = db.insertTransmission({
    raw_hex: '0400aabbccdd',
    hash: 'hash-001',
    timestamp: '2025-01-01T00:01:00Z',
    observer_id: 'obs-2',
    observer_name: 'Observer Two',
    direction: 'rx',
    snr: 8.0,
    rssi: -90,
    route_type: 1,
    payload_type: 4,
    path_json: '["aabb"]',
    decoded_json: '{"type":"ADVERT","pubKey":"aabbccdd11223344aabbccdd11223344","name":"TestNode"}',
  });
  assert(result2.transmissionId === result.transmissionId, 'same transmissionId for duplicate hash');

  // No hash = null
  const result3 = db.insertTransmission({ raw_hex: '0400' });
  assert(result3 === null, 'no hash returns null');
}

// --- getPackets ---
console.log('\ngetPackets:');
{
  const { rows, total } = db.getPackets({ limit: 10 });
  assert(total >= 1, 'has packets');
  assert(rows.length >= 1, 'returns rows');
  assert(rows[0].hash === 'hash-001', 'correct hash');

  // Filter by type
  const { rows: r2 } = db.getPackets({ type: 4 });
  assert(r2.length >= 1, 'filter by type works');

  const { rows: r3 } = db.getPackets({ type: 99 });
  assert(r3.length === 0, 'filter by nonexistent type returns empty');

  // Filter by hash
  const { rows: r4 } = db.getPackets({ hash: 'hash-001' });
  assert(r4.length >= 1, 'filter by hash works');
}

// --- getPacket ---
console.log('\ngetPacket:');
{
  const { rows } = db.getPackets({ limit: 1 });
  const pkt = db.getPacket(rows[0].id);
  assert(pkt !== null, 'getPacket returns packet');
  assert(pkt.hash === 'hash-001', 'correct packet');

  const missing = db.getPacket(999999);
  assert(missing === null, 'missing packet returns null');
}

// --- getTransmission ---
console.log('\ngetTransmission:');
{
  const tx = db.getTransmission(1);
  assert(tx !== null, 'getTransmission returns data');
  assert(tx.hash === 'hash-001', 'correct hash');

  const missing = db.getTransmission(999999);
  assert(missing === null, 'missing transmission returns null');
}

// --- getNodes ---
console.log('\ngetNodes:');
{
  const { rows, total } = db.getNodes({ limit: 10 });
  assert(total >= 1, 'has nodes');
  assert(rows.length >= 1, 'returns node rows');

  // Sort by name
  const { rows: r2 } = db.getNodes({ sortBy: 'name' });
  assert(r2.length >= 1, 'sort by name works');

  // Invalid sort falls back to last_seen
  const { rows: r3 } = db.getNodes({ sortBy: 'DROP TABLE nodes' });
  assert(r3.length >= 1, 'invalid sort is safe');
}

// --- getNode ---
console.log('\ngetNode:');
{
  const node = db.getNode('aabbccdd11223344aabbccdd11223344');
  assert(node !== null, 'getNode returns node');
  assert(Array.isArray(node.recentPackets), 'has recentPackets');

  const missing = db.getNode('nonexistent');
  assert(missing === null, 'missing node returns null');
}

// --- searchNodes ---
console.log('\nsearchNodes:');
{
  const results = db.searchNodes('Updated');
  assert(results.length >= 1, 'search by name');

  const r2 = db.searchNodes('aabbcc');
  assert(r2.length >= 1, 'search by pubkey prefix');

  const r3 = db.searchNodes('nonexistent_xyz');
  assert(r3.length === 0, 'no results for nonexistent');
}

// --- getStats ---
console.log('\ngetStats:');
{
  const stats = db.getStats();
  assert(stats.totalNodes >= 1, 'totalNodes');
  assert(stats.totalObservers >= 1, 'totalObservers');
  assert(typeof stats.totalPackets === 'number', 'totalPackets is number');
  assert(typeof stats.packetsLastHour === 'number', 'packetsLastHour is number');
}

// --- getNodeHealth ---
console.log('\ngetNodeHealth:');
{
  const health = db.getNodeHealth('aabbccdd11223344aabbccdd11223344');
  assert(health !== null, 'returns health data');
  assert(health.node.name === 'UpdatedNode', 'has node info');
  assert(typeof health.stats.totalPackets === 'number', 'has totalPackets stat');
  assert(Array.isArray(health.observers), 'has observers array');
  assert(Array.isArray(health.recentPackets), 'has recentPackets array');

  const missing = db.getNodeHealth('nonexistent');
  assert(missing === null, 'missing node returns null');
}

// --- getNodeAnalytics ---
console.log('\ngetNodeAnalytics:');
{
  const analytics = db.getNodeAnalytics('aabbccdd11223344aabbccdd11223344', 7);
  assert(analytics !== null, 'returns analytics');
  assert(analytics.node.name === 'UpdatedNode', 'has node info');
  assert(Array.isArray(analytics.activityTimeline), 'has activityTimeline');
  assert(Array.isArray(analytics.snrTrend), 'has snrTrend');
  assert(Array.isArray(analytics.packetTypeBreakdown), 'has packetTypeBreakdown');
  assert(Array.isArray(analytics.observerCoverage), 'has observerCoverage');
  assert(Array.isArray(analytics.hopDistribution), 'has hopDistribution');
  assert(Array.isArray(analytics.peerInteractions), 'has peerInteractions');
  assert(Array.isArray(analytics.uptimeHeatmap), 'has uptimeHeatmap');
  assert(typeof analytics.computedStats.availabilityPct === 'number', 'has availabilityPct');
  assert(typeof analytics.computedStats.signalGrade === 'string', 'has signalGrade');

  const missing = db.getNodeAnalytics('nonexistent', 7);
  assert(missing === null, 'missing node returns null');
}

// --- seed ---
console.log('\nseed:');
{
  // Already has data, should return false
  const result = db.seed();
  assert(result === false, 'seed returns false when data exists');
}

cleanup();
delete process.env.DB_PATH;

console.log(`\n═══════════════════════════════════════`);
console.log(`  PASSED: ${passed}`);
console.log(`  FAILED: ${failed}`);
console.log(`═══════════════════════════════════════`);
if (failed > 0) process.exit(1);
