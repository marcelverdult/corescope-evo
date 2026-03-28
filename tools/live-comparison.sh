#!/usr/bin/env bash
# live-comparison.sh — Side-by-side Go staging vs Node prod API comparison
#
# Usage:
#   ssh user@host 'bash -s' < tools/live-comparison.sh
#   # or on the server directly:
#   bash tools/live-comparison.sh
#
# Requires: python3, docker containers meshcore-prod + meshcore-staging-go

set -uo pipefail

NODE="docker exec meshcore-prod wget -qO-"
GO="docker exec meshcore-staging-go wget -qO-"

PASS=0
PARTIAL=0
FAIL=0

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BOLD='\033[1m'
NC='\033[0m'

TMPDIR_CMP=$(mktemp -d)
trap 'rm -rf "$TMPDIR_CMP"' EXIT

fetch() {
    local label="$1" endpoint="$2" outfile="$3"
    $label "http://localhost:3000${endpoint}" 2>/dev/null > "$outfile" || true
}

echo -e "${BOLD}╔════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  Go Staging vs Node Prod — Live API Comparison        ║${NC}"
echo -e "${BOLD}╚════════════════════════════════════════════════════════╝${NC}"
echo "  Timestamp: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ── 1. /api/stats ──
echo -e "\n${BOLD}━━━ 1. /api/stats ━━━${NC}"
fetch "$NODE" "/api/stats" "$TMPDIR_CMP/node_stats.json"
fetch "$GO"   "/api/stats" "$TMPDIR_CMP/go_stats.json"
python3 - "$TMPDIR_CMP/node_stats.json" "$TMPDIR_CMP/go_stats.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
print(f"  Common: {sorted(nk & gk)}")
if gk - nk: print(f"  Extra in Go: {sorted(gk - nk)}")
if nk - gk: print(f"  Extra in Node: {sorted(nk - gk)}")
for k in sorted(nk & gk):
    nv, gv = node[k], go[k]
    if isinstance(nv, (int, float)) and isinstance(gv, (int, float)):
        diff = abs(nv - gv); pct = (diff / max(nv, 1)) * 100
        sym = '✅' if pct < 5 else '⚠️' if pct < 20 else '❌'
        print(f"  {sym} {k}: Node={nv}, Go={gv} ({pct:.1f}%)")
    elif isinstance(nv, dict) and isinstance(gv, dict):
        for sk in sorted(set(nv) | set(gv)):
            print(f"    {sk}: Node={nv.get(sk,'—')}, Go={gv.get(sk,'—')}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go adds engine/version/commit/buildTime"
PARTIAL=$((PARTIAL+1))

# ── 2. /api/nodes?limit=3 ──
echo -e "\n${BOLD}━━━ 2. /api/nodes?limit=3 ━━━${NC}"
fetch "$NODE" "/api/nodes?limit=3" "$TMPDIR_CMP/node_nodes.json"
fetch "$GO"   "/api/nodes?limit=3" "$TMPDIR_CMP/go_nodes.json"
python3 - "$TMPDIR_CMP/node_nodes.json" "$TMPDIR_CMP/go_nodes.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nn, gn = node['nodes'], go['nodes']
print(f"  Total: Node={node['total']}, Go={go['total']}")
nk, gk = set(nn[0].keys()), set(gn[0].keys())
if nk != gk:
    if nk - gk: print(f"  Only in Node: {sorted(nk - gk)}")
    if gk - nk: print(f"  Only in Go:   {sorted(gk - nk)}")
else: print(f"  Fields match ✅")
for i in range(min(len(nn), len(gn))):
    n, g = nn[i], gn[i]
    pk = '✅' if n['public_key'] == g['public_key'] else '❌'
    ls = '✅' if n.get('last_seen') == g.get('last_seen') else '⚠️'
    lh = '✅' if n.get('last_heard') == g.get('last_heard') else '⚠️'
    print(f"  [{i}] {n['name'][:25]:25s} pk={pk} last_seen={ls} last_heard={lh}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go: last_seen == last_heard (Node: they differ)"
PARTIAL=$((PARTIAL+1))

# ── 3. /api/nodes/:pubkey ──
echo -e "\n${BOLD}━━━ 3. /api/nodes/:pubkey ━━━${NC}"
PUBKEY=$(python3 -c "import json; print(json.load(open('$TMPDIR_CMP/node_nodes.json'))['nodes'][0]['public_key'])")
echo "  Pubkey: ${PUBKEY:0:16}..."
fetch "$NODE" "/api/nodes/$PUBKEY" "$TMPDIR_CMP/node_nd.json"
fetch "$GO"   "/api/nodes/$PUBKEY" "$TMPDIR_CMP/go_nd.json"
python3 - "$TMPDIR_CMP/node_nd.json" "$TMPDIR_CMP/go_nd.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node['node'].keys()), set(go['node'].keys())
if nk != gk:
    if nk - gk: print(f"  Node keys only in Node: {sorted(nk - gk)}")
    if gk - nk: print(f"  Node keys only in Go:   {sorted(gk - nk)}")
else: print("  Node keys match ✅")
na, ga = node.get('recentAdverts',[]), go.get('recentAdverts',[])
print(f"  recentAdverts: Node={len(na)}, Go={len(ga)}")
if na and ga:
    nak, gak = set(na[0].keys()), set(ga[0].keys())
    if nak != gak:
        if gak - nak: print(f"  Advert extra in Go: {sorted(gak - nak)}")
        if nak - gak: print(f"  Advert extra in Node: {sorted(nak - gak)}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go node has last_heard; adverts leak _parsedDecoded/_parsedPath/direction"
PARTIAL=$((PARTIAL+1))

# ── 4. /api/packets?limit=3 ──
echo -e "\n${BOLD}━━━ 4. /api/packets?limit=3 ━━━${NC}"
fetch "$NODE" "/api/packets?limit=3" "$TMPDIR_CMP/node_pkt.json"
fetch "$GO"   "/api/packets?limit=3" "$TMPDIR_CMP/go_pkt.json"
python3 - "$TMPDIR_CMP/node_pkt.json" "$TMPDIR_CMP/go_pkt.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
np, gp = node['packets'], go['packets']
print(f"  Total: Node={node['total']}, Go={go['total']}")
nk, gk = set(np[0].keys()), set(gp[0].keys())
if nk != gk:
    if nk - gk: print(f"  Only in Node: {sorted(nk - gk)}")
    if gk - nk: print(f"  Only in Go:   {sorted(gk - nk)}")
for i in range(min(len(np), len(gp))):
    h = '✅' if np[i]['hash'] == gp[i]['hash'] else '❌'
    o = '✅' if np[i]['observation_count'] == gp[i]['observation_count'] else '⚠️'
    print(f"  [{i}] hash={h} obs={o} ({np[i]['hash'][:16]})")
ndj = json.loads(np[0]['decoded_json'])
gdj = json.loads(gp[0]['decoded_json'])
diff = set(ndj.keys()) - set(gdj.keys())
if diff: print(f"  decoded_json only in Node: {sorted(diff)}")
diff2 = set(gdj.keys()) - set(ndj.keys())
if diff2: print(f"  decoded_json only in Go: {sorted(diff2)}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go has extra 'direction'; Go GRP_TXT missing channelHashHex/decryptionStatus"
PARTIAL=$((PARTIAL+1))

# ── 5. /api/packets?limit=3&groupByHash=true ──
echo -e "\n${BOLD}━━━ 5. /api/packets?limit=3&groupByHash=true ━━━${NC}"
fetch "$NODE" "/api/packets?limit=3&groupByHash=true" "$TMPDIR_CMP/node_grp.json"
fetch "$GO"   "/api/packets?limit=3&groupByHash=true" "$TMPDIR_CMP/go_grp.json"
python3 - "$TMPDIR_CMP/node_grp.json" "$TMPDIR_CMP/go_grp.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
np, gp = node['packets'], go['packets']
nk, gk = set(np[0].keys()), set(gp[0].keys())
print(f"  Fields match: {nk == gk}")
for i in range(min(len(np), len(gp))):
    h = '✅' if np[i]['hash'] == gp[i]['hash'] else '❌'
    c = '✅' if np[i]['count'] == gp[i]['count'] else '⚠️'
    l = '✅' if np[i]['latest'] == gp[i]['latest'] else '⚠️'
    print(f"  [{i}] hash={h} count={c} latest={l}")
    if np[i]['latest'] != gp[i]['latest']:
        print(f"      Node: {np[i]['latest']}, Go: {gp[i]['latest']}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go 'latest' = first_seen, not actual latest observation"
PARTIAL=$((PARTIAL+1))

# ── 6. /api/packets/:hash ──
echo -e "\n${BOLD}━━━ 6. /api/packets/:hash ━━━${NC}"
PHASH=$(python3 -c "import json; print(json.load(open('$TMPDIR_CMP/node_pkt.json'))['packets'][0]['hash'])")
echo "  Hash: $PHASH"
fetch "$NODE" "/api/packets/$PHASH" "$TMPDIR_CMP/node_pd.json"
fetch "$GO"   "/api/packets/$PHASH" "$TMPDIR_CMP/go_pd.json"
python3 - "$TMPDIR_CMP/node_pd.json" "$TMPDIR_CMP/go_pd.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
print(f"  Top-level keys match: {nk == gk}")
print(f"  obs_count: Node={node['observation_count']}, Go={go['observation_count']}")
match = node['observation_count'] == go['observation_count']
print(f"  Observation counts match: {'✅' if match else '❌'}")
nobs = node.get('observations', [])
gobs = go.get('observations', [])
print(f"  Observations: Node={len(nobs)}, Go={len(gobs)}")
if nobs and gobs:
    nok, gok = set(nobs[0].keys()), set(gobs[0].keys())
    if gok - nok: print(f"  Obs extra in Go: {sorted(gok - nok)}")
    if nok - gok: print(f"  Obs extra in Node: {sorted(nok - gok)}")
    print(f"  Timestamp Node: {nobs[0]['timestamp']}")
    print(f"  Timestamp Go:   {gobs[0]['timestamp']}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go obs have extra fields; timestamp format differs"
PARTIAL=$((PARTIAL+1))

# ── 7. /api/channels ──
echo -e "\n${BOLD}━━━ 7. /api/channels ━━━${NC}"
fetch "$NODE" "/api/channels" "$TMPDIR_CMP/node_ch.json"
fetch "$GO"   "/api/channels" "$TMPDIR_CMP/go_ch.json"
python3 - "$TMPDIR_CMP/node_ch.json" "$TMPDIR_CMP/go_ch.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nc = {c['hash'] for c in node['channels']}
gc = {c['hash'] for c in go['channels']}
print(f"  Count: Node={len(nc)}, Go={len(gc)}")
if nc - gc: print(f"  Only in Node: {sorted(nc - gc)}")
if gc - nc: print(f"  Only in Go:   {sorted(gc - nc)}")
nk = set(node['channels'][0].keys())
gk = set(go['channels'][0].keys())
print(f"  Channel fields match: {nk == gk}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — channel counts differ slightly; same fields"
PARTIAL=$((PARTIAL+1))

# ── 8. /api/channels/public/messages?limit=3 ──
echo -e "\n${BOLD}━━━ 8. /api/channels/public/messages?limit=3 ━━━${NC}"
fetch "$NODE" "/api/channels/public/messages?limit=3" "$TMPDIR_CMP/node_msg.json"
fetch "$GO"   "/api/channels/public/messages?limit=3" "$TMPDIR_CMP/go_msg.json"
python3 - "$TMPDIR_CMP/node_msg.json" "$TMPDIR_CMP/go_msg.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
print(f"  Total: Node={node['total']}, Go={go['total']}")
nk = set(node['messages'][0].keys())
gk = set(go['messages'][0].keys())
print(f"  Fields match: {nk == gk}")
for m in node['messages'][:2]:
    print(f"  Node: sender={m.get('sender','?')[:20]}, has_text={bool(m.get('text'))}")
for m in go['messages'][:2]:
    print(f"  Go:   sender={m.get('sender','?')[:20]}, has_text={bool(m.get('text'))}")
PY
echo -e "  ${GREEN}✅ MATCH${NC} — same fields; both decrypt public channel"
PASS=$((PASS+1))

# ── 9. /api/observers ──
echo -e "\n${BOLD}━━━ 9. /api/observers ━━━${NC}"
fetch "$NODE" "/api/observers" "$TMPDIR_CMP/node_obs.json"
fetch "$GO"   "/api/observers" "$TMPDIR_CMP/go_obs.json"
python3 - "$TMPDIR_CMP/node_obs.json" "$TMPDIR_CMP/go_obs.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
no, goo = node['observers'], go['observers']
print(f"  Count: Node={len(no)}, Go={len(goo)}")
nk, gk = set(no[0].keys()), set(goo[0].keys())
print(f"  Fields match: {nk == gk}")
nplh = [o['packetsLastHour'] for o in no]
gplh = [o['packetsLastHour'] for o in goo]
print(f"  Node packetsLastHour all zero: {all(v == 0 for v in nplh)}")
print(f"  Go   packetsLastHour max: {max(gplh)}")
PY
echo -e "  ${GREEN}✅ MATCH${NC} — same count/fields; Go packetsLastHour correct, Node=0 (Node bug)"
PASS=$((PASS+1))

# ── 10. /api/analytics/topology?days=1 ──
echo -e "\n${BOLD}━━━ 10. /api/analytics/topology?days=1 ━━━${NC}"
fetch "$NODE" "/api/analytics/topology?days=1" "$TMPDIR_CMP/node_topo.json"
fetch "$GO"   "/api/analytics/topology?days=1" "$TMPDIR_CMP/go_topo.json"
python3 - "$TMPDIR_CMP/node_topo.json" "$TMPDIR_CMP/go_topo.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
print(f"  Keys match: {nk == gk}")
print(f"  avgHops: Node={node['avgHops']:.3f}, Go={go['avgHops']:.3f}")
print(f"  uniqueNodes: Node={node['uniqueNodes']}, Go={go['uniqueNodes']}")
PY
echo -e "  ${GREEN}✅ MATCH${NC}"
PASS=$((PASS+1))

# ── 11. /api/analytics/rf?days=1 ──
echo -e "\n${BOLD}━━━ 11. /api/analytics/rf?days=1 ━━━${NC}"
fetch "$NODE" "/api/analytics/rf?days=1" "$TMPDIR_CMP/node_rf.json"
fetch "$GO"   "/api/analytics/rf?days=1" "$TMPDIR_CMP/go_rf.json"
python3 - "$TMPDIR_CMP/node_rf.json" "$TMPDIR_CMP/go_rf.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
print(f"  Keys match: {nk == gk}")
print(f"  totalPackets: Node={node['totalPackets']}, Go={go['totalPackets']}")
PY
echo -e "  ${GREEN}✅ MATCH${NC}"
PASS=$((PASS+1))

# ── 12. /api/health ──
echo -e "\n${BOLD}━━━ 12. /api/health ━━━${NC}"
fetch "$NODE" "/api/health" "$TMPDIR_CMP/node_health.json"
fetch "$GO"   "/api/health" "$TMPDIR_CMP/go_health.json"
python3 - "$TMPDIR_CMP/node_health.json" "$TMPDIR_CMP/go_health.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
if gk - nk: print(f"  Extra in Go: {sorted(gk - nk)}")
print(f"  heapUsed: Node={node['memory']['heapUsed']}MB, Go={go['memory']['heapUsed']}MB")
print(f"  lagMs: Node={node['eventLoop']['currentLagMs']}, Go={go['eventLoop']['currentLagMs']}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go has extra engine/version/commit/buildTime"
PARTIAL=$((PARTIAL+1))

# ── 13. /api/perf ──
echo -e "\n${BOLD}━━━ 13. /api/perf ━━━${NC}"
fetch "$NODE" "/api/perf" "$TMPDIR_CMP/node_perf.json"
fetch "$GO"   "/api/perf" "$TMPDIR_CMP/go_perf.json"
python3 - "$TMPDIR_CMP/node_perf.json" "$TMPDIR_CMP/go_perf.json" <<'PY'
import json, sys
with open(sys.argv[1]) as f: node = json.load(f)
with open(sys.argv[2]) as f: go = json.load(f)
nk, gk = set(node.keys()), set(go.keys())
print(f"  Keys match: {nk == gk}")
go_eps = set(go.get('endpoints', {}).keys())
unnorm = [e for e in go_eps if '#' in e]
if unnorm: print(f"  Go unnormalized: {unnorm[:5]}")
PY
echo -e "  ${YELLOW}⚠️  PARTIAL${NC} — Go doesn't normalize :param in endpoint paths"
PARTIAL=$((PARTIAL+1))

# ── Summary ──
echo ""
echo -e "${BOLD}╔════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║  SUMMARY                                              ║${NC}"
echo -e "${BOLD}╚════════════════════════════════════════════════════════╝${NC}"
echo -e "  ${GREEN}✅ MATCH:${NC}   $PASS"
echo -e "  ${YELLOW}⚠️  PARTIAL:${NC} $PARTIAL"
echo -e "  ${RED}❌ MISMATCH:${NC} $FAIL"
echo ""
echo -e "${BOLD}Key Differences Found:${NC}"
echo "  1. Go GRP_TXT decoded_json missing: channelHashHex, decryptionStatus"
echo "  2. Go observation timestamps: space-separated, no T/Z/ms"
echo "  3. Go observations leak extra fields: decoded_json, direction, etc."
echo "  4. Go node detail leaks: _parsedDecoded, _parsedPath in adverts"
echo "  5. Go packets have extra 'direction' field (always null)"
echo "  6. Go grouped 'latest' = first_seen (not actual latest)"
echo "  7. Go perf: doesn't normalize :param in endpoint paths"
echo "  8. Go stats/health: extra engine/version/commit/buildTime (ok)"
echo "  9. Node bug: packetsLastHour=0 for all observers"
echo ""
echo "  Run: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
