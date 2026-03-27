package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// Server holds shared state for route handlers.
type Server struct {
	db        *DB
	cfg       *Config
	hub       *Hub
	store     *PacketStore // in-memory packet store (nil = fallback to DB)
	startedAt time.Time
	perfStats *PerfStats
	version   string
	commit    string
}

// PerfStats tracks request performance.
type PerfStats struct {
	Requests    int64
	TotalMs     float64
	Endpoints   map[string]*EndpointPerf
	SlowQueries []map[string]interface{}
	StartedAt   time.Time
}

type EndpointPerf struct {
	Count   int
	TotalMs float64
	MaxMs   float64
	Recent  []float64
}

func NewPerfStats() *PerfStats {
	return &PerfStats{
		Endpoints:   make(map[string]*EndpointPerf),
		SlowQueries: make([]map[string]interface{}, 0),
		StartedAt:   time.Now(),
	}
}

func NewServer(db *DB, cfg *Config, hub *Hub) *Server {
	return &Server{
		db:        db,
		cfg:       cfg,
		hub:       hub,
		startedAt: time.Now(),
		perfStats: NewPerfStats(),
		version:   resolveVersion(),
		commit:    resolveCommit(),
	}
}

// RegisterRoutes sets up all HTTP routes on the given router.
func (s *Server) RegisterRoutes(r *mux.Router) {
	// Performance instrumentation middleware
	r.Use(s.perfMiddleware)

	// Config endpoints
	r.HandleFunc("/api/config/cache", s.handleConfigCache).Methods("GET")
	r.HandleFunc("/api/config/client", s.handleConfigClient).Methods("GET")
	r.HandleFunc("/api/config/regions", s.handleConfigRegions).Methods("GET")
	r.HandleFunc("/api/config/theme", s.handleConfigTheme).Methods("GET")
	r.HandleFunc("/api/config/map", s.handleConfigMap).Methods("GET")

	// System endpoints
	r.HandleFunc("/api/health", s.handleHealth).Methods("GET")
	r.HandleFunc("/api/stats", s.handleStats).Methods("GET")
	r.HandleFunc("/api/perf", s.handlePerf).Methods("GET")
	r.HandleFunc("/api/perf/reset", s.handlePerfReset).Methods("POST")

	// Packet endpoints
	r.HandleFunc("/api/packets/timestamps", s.handlePacketTimestamps).Methods("GET")
	r.HandleFunc("/api/packets/{id}", s.handlePacketDetail).Methods("GET")
	r.HandleFunc("/api/packets", s.handlePackets).Methods("GET")
	r.HandleFunc("/api/packets", s.handlePostPacket).Methods("POST")

	// Decode endpoint
	r.HandleFunc("/api/decode", s.handleDecode).Methods("POST")

	// Node endpoints — fixed routes BEFORE parameterized
	r.HandleFunc("/api/nodes/search", s.handleNodeSearch).Methods("GET")
	r.HandleFunc("/api/nodes/bulk-health", s.handleBulkHealth).Methods("GET")
	r.HandleFunc("/api/nodes/network-status", s.handleNetworkStatus).Methods("GET")
	r.HandleFunc("/api/nodes/{pubkey}/health", s.handleNodeHealth).Methods("GET")
	r.HandleFunc("/api/nodes/{pubkey}/paths", s.handleNodePaths).Methods("GET")
	r.HandleFunc("/api/nodes/{pubkey}/analytics", s.handleNodeAnalytics).Methods("GET")
	r.HandleFunc("/api/nodes/{pubkey}", s.handleNodeDetail).Methods("GET")
	r.HandleFunc("/api/nodes", s.handleNodes).Methods("GET")

	// Analytics endpoints
	r.HandleFunc("/api/analytics/rf", s.handleAnalyticsRF).Methods("GET")
	r.HandleFunc("/api/analytics/topology", s.handleAnalyticsTopology).Methods("GET")
	r.HandleFunc("/api/analytics/channels", s.handleAnalyticsChannels).Methods("GET")
	r.HandleFunc("/api/analytics/distance", s.handleAnalyticsDistance).Methods("GET")
	r.HandleFunc("/api/analytics/hash-sizes", s.handleAnalyticsHashSizes).Methods("GET")
	r.HandleFunc("/api/analytics/subpaths", s.handleAnalyticsSubpaths).Methods("GET")
	r.HandleFunc("/api/analytics/subpath-detail", s.handleAnalyticsSubpathDetail).Methods("GET")

	// Other endpoints
	r.HandleFunc("/api/resolve-hops", s.handleResolveHops).Methods("GET")
	r.HandleFunc("/api/channels/{hash}/messages", s.handleChannelMessages).Methods("GET")
	r.HandleFunc("/api/channels", s.handleChannels).Methods("GET")
	r.HandleFunc("/api/observers/{id}/analytics", s.handleObserverAnalytics).Methods("GET")
	r.HandleFunc("/api/observers/{id}", s.handleObserverDetail).Methods("GET")
	r.HandleFunc("/api/observers", s.handleObservers).Methods("GET")
	r.HandleFunc("/api/traces/{hash}", s.handleTraces).Methods("GET")
	r.HandleFunc("/api/iata-coords", s.handleIATACoords).Methods("GET")
	r.HandleFunc("/api/audio-lab/buckets", s.handleAudioLabBuckets).Methods("GET")
}

func (s *Server) perfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		ms := float64(time.Since(start).Microseconds()) / 1000.0

		s.perfStats.Requests++
		s.perfStats.TotalMs += ms

		// Normalize key
		re := regexp.MustCompile(`[0-9a-f]{8,}`)
		key := re.ReplaceAllString(r.URL.Path, ":id")
		if _, ok := s.perfStats.Endpoints[key]; !ok {
			s.perfStats.Endpoints[key] = &EndpointPerf{Recent: make([]float64, 0, 100)}
		}
		ep := s.perfStats.Endpoints[key]
		ep.Count++
		ep.TotalMs += ms
		if ms > ep.MaxMs {
			ep.MaxMs = ms
		}
		ep.Recent = append(ep.Recent, ms)
		if len(ep.Recent) > 100 {
			ep.Recent = ep.Recent[1:]
		}
		if ms > 100 {
			slow := map[string]interface{}{
				"path": r.URL.Path, "ms": round(ms, 1),
				"time": time.Now().UTC().Format(time.RFC3339), "status": 200,
			}
			s.perfStats.SlowQueries = append(s.perfStats.SlowQueries, slow)
			if len(s.perfStats.SlowQueries) > 50 {
				s.perfStats.SlowQueries = s.perfStats.SlowQueries[1:]
			}
		}
	})
}

// --- Config Handlers ---

func (s *Server) handleConfigCache(w http.ResponseWriter, r *http.Request) {
	ct := s.cfg.CacheTTL
	if ct == nil {
		ct = map[string]interface{}{}
	}
	writeJSON(w, ct)
}

func (s *Server) handleConfigClient(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"roles":              s.cfg.Roles,
		"healthThresholds":   s.cfg.HealthThresholds,
		"tiles":              s.cfg.Tiles,
		"snrThresholds":      s.cfg.SnrThresholds,
		"distThresholds":     s.cfg.DistThresholds,
		"maxHopDist":         s.cfg.MaxHopDist,
		"limits":             s.cfg.Limits,
		"perfSlowMs":         s.cfg.PerfSlowMs,
		"wsReconnectMs":      s.cfg.WsReconnectMs,
		"cacheInvalidateMs":  s.cfg.CacheInvalidMs,
		"externalUrls":       s.cfg.ExternalUrls,
		"propagationBufferMs": s.cfg.PropagationBufferMs(),
	})
}

func (s *Server) handleConfigRegions(w http.ResponseWriter, r *http.Request) {
	regions := make(map[string]string)
	for k, v := range s.cfg.Regions {
		regions[k] = v
	}
	codes, _ := s.db.GetDistinctIATAs()
	for _, c := range codes {
		if _, ok := regions[c]; !ok {
			regions[c] = c
		}
	}
	writeJSON(w, regions)
}

func (s *Server) handleConfigTheme(w http.ResponseWriter, r *http.Request) {
	theme := LoadTheme(".")

	branding := mergeMap(map[string]interface{}{
		"siteName": "MeshCore Analyzer",
		"tagline":  "Real-time MeshCore LoRa mesh network analyzer",
	}, s.cfg.Branding, theme.Branding)

	themeColors := mergeMap(map[string]interface{}{
		"accent":      "#4a9eff",
		"accentHover": "#6db3ff",
		"navBg":       "#0f0f23",
		"navBg2":      "#1a1a2e",
	}, s.cfg.Theme, theme.Theme)

	nodeColors := mergeMap(map[string]interface{}{
		"repeater":  "#dc2626",
		"companion": "#2563eb",
		"room":      "#16a34a",
		"sensor":    "#d97706",
		"observer":  "#8b5cf6",
	}, s.cfg.NodeColors, theme.NodeColors)

	themeDark := mergeMap(map[string]interface{}{}, s.cfg.ThemeDark, theme.ThemeDark)
	typeColors := mergeMap(map[string]interface{}{}, s.cfg.TypeColors, theme.TypeColors)

	var home interface{}
	if theme.Home != nil {
		home = theme.Home
	} else if s.cfg.Home != nil {
		home = s.cfg.Home
	}

	writeJSON(w, map[string]interface{}{
		"branding":   branding,
		"theme":      themeColors,
		"themeDark":  themeDark,
		"nodeColors": nodeColors,
		"typeColors": typeColors,
		"home":       home,
	})
}

func (s *Server) handleConfigMap(w http.ResponseWriter, r *http.Request) {
	center := s.cfg.MapDefaults.Center
	if len(center) == 0 {
		center = []float64{37.45, -122.0}
	}
	zoom := s.cfg.MapDefaults.Zoom
	if zoom == 0 {
		zoom = 9
	}
	writeJSON(w, map[string]interface{}{"center": center, "zoom": zoom})
}

// --- System Handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	uptime := time.Since(s.startedAt).Seconds()

	wsClients := 0
	if s.hub != nil {
		wsClients = s.hub.ClientCount()
	}

	// Real packet store stats
	pktCount := 0
	var pktEstMB float64
	if s.store != nil {
		ps := s.store.GetPerfStoreStats()
		if v, ok := ps["totalLoaded"].(int); ok {
			pktCount = v
		}
		if v, ok := ps["estimatedMB"].(float64); ok {
			pktEstMB = v
		}
	}

	// Real cache stats
	cacheStats := map[string]interface{}{
		"entries": 0, "hits": int64(0), "misses": int64(0),
		"staleHits": 0, "recomputes": int64(0), "hitRate": float64(0),
	}
	if s.store != nil {
		cs := s.store.GetCacheStats()
		cacheStats = map[string]interface{}{
			"entries":    cs["size"],
			"hits":       cs["hits"],
			"misses":     cs["misses"],
			"staleHits":  cs["staleHits"],
			"recomputes": cs["recomputes"],
			"hitRate":    cs["hitRate"],
		}
	}

	// Build eventLoop-equivalent from GC pause data (matches Node.js shape)
	var gcPauses []float64
	n := int(m.NumGC)
	if n > 256 {
		n = 256
	}
	for i := 0; i < n; i++ {
		idx := (int(m.NumGC) - n + i) % 256
		gcPauses = append(gcPauses, float64(m.PauseNs[idx])/1e6)
	}
	sortedPauses := sortedCopy(gcPauses)
	var lastPauseMs float64
	if m.NumGC > 0 {
		lastPauseMs = float64(m.PauseNs[(m.NumGC+255)%256]) / 1e6
	}

	writeJSON(w, map[string]interface{}{
		"status":      "ok",
		"engine":      "go",
		"version":     s.version,
		"commit":      s.commit,
		"uptime":      int(uptime),
		"uptimeHuman": fmt.Sprintf("%dh %dm", int(uptime)/3600, (int(uptime)%3600)/60),
		"memory": map[string]interface{}{
			"rss":       int(m.Sys / 1024 / 1024),
			"heapUsed":  int(m.HeapAlloc / 1024 / 1024),
			"heapTotal": int(m.HeapSys / 1024 / 1024),
			"external":  0,
		},
		"eventLoop": map[string]interface{}{
			"currentLagMs": round(lastPauseMs, 1),
			"maxLagMs":     round(percentile(sortedPauses, 1.0), 1),
			"p50Ms":        round(percentile(sortedPauses, 0.5), 1),
			"p95Ms":        round(percentile(sortedPauses, 0.95), 1),
			"p99Ms":        round(percentile(sortedPauses, 0.99), 1),
		},
		"cache":     cacheStats,
		"websocket": map[string]interface{}{"clients": wsClients},
		"packetStore": map[string]interface{}{
			"packets":     pktCount,
			"estimatedMB": pktEstMB,
		},
		"perf": map[string]interface{}{
			"totalRequests": s.perfStats.Requests,
			"avgMs":         safeAvg(s.perfStats.TotalMs, float64(s.perfStats.Requests)),
			"slowQueries":   len(s.perfStats.SlowQueries),
			"recentSlow":    lastN(s.perfStats.SlowQueries, 5),
		},
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var stats *Stats
	var err error
	if s.store != nil {
		stats, err = s.store.GetStoreStats()
	} else {
		stats, err = s.db.GetStats()
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	counts := s.db.GetRoleCounts()
	result := map[string]interface{}{
		"totalPackets":       stats.TotalPackets,
		"totalTransmissions": stats.TotalTransmissions,
		"totalObservations":  stats.TotalObservations,
		"totalNodes":         stats.TotalNodes,
		"totalNodesAllTime":  stats.TotalNodesAllTime,
		"totalObservers":     stats.TotalObservers,
		"packetsLastHour":    stats.PacketsLastHour,
		"engine":             "go",
		"version":            s.version,
		"commit":             s.commit,
		"counts":             counts,
	}
	writeJSON(w, result)
}

func (s *Server) handlePerf(w http.ResponseWriter, r *http.Request) {
	// Endpoint performance summary
	type epEntry struct {
		path string
		data map[string]interface{}
	}
	var entries []epEntry
	for path, ep := range s.perfStats.Endpoints {
		sorted := sortedCopy(ep.Recent)
		d := map[string]interface{}{
			"count": ep.Count,
			"avgMs": round(ep.TotalMs/float64(ep.Count), 1),
			"p50Ms": round(percentile(sorted, 0.5), 1),
			"p95Ms": round(percentile(sorted, 0.95), 1),
			"maxMs": round(ep.MaxMs, 1),
		}
		entries = append(entries, epEntry{path, d})
	}
	// Sort by total time spent (count * avg) descending, matching Node.js
	sort.Slice(entries, func(i, j int) bool {
		ti := float64(entries[i].data["count"].(int)) * entries[i].data["avgMs"].(float64)
		tj := float64(entries[j].data["count"].(int)) * entries[j].data["avgMs"].(float64)
		return ti > tj
	})
	summary := map[string]interface{}{}
	for _, e := range entries {
		summary[e.path] = e.data
	}

	// Cache stats from packet store
	cacheStats := map[string]interface{}{
		"size": 0, "hits": int64(0), "misses": int64(0),
		"staleHits": 0, "recomputes": int64(0), "hitRate": float64(0),
	}
	if s.store != nil {
		cacheStats = s.store.GetCacheStats()
	}

	// Packet store stats
	var pktStoreStats map[string]interface{}
	if s.store != nil {
		pktStoreStats = s.store.GetPerfStoreStats()
	}

	// SQLite stats
	var sqliteStats map[string]interface{}
	if s.db != nil {
		sqliteStats = s.db.GetDBSizeStats()
	}

	uptimeSec := int(time.Since(s.perfStats.StartedAt).Seconds())

	writeJSON(w, map[string]interface{}{
		"uptime":        uptimeSec,
		"totalRequests": s.perfStats.Requests,
		"avgMs":         safeAvg(s.perfStats.TotalMs, float64(s.perfStats.Requests)),
		"endpoints":     summary,
		"slowQueries":   lastN(s.perfStats.SlowQueries, 20),
		"cache":         cacheStats,
		"packetStore":   pktStoreStats,
		"sqlite":        sqliteStats,
	})
}

func (s *Server) handlePerfReset(w http.ResponseWriter, r *http.Request) {
	s.perfStats = NewPerfStats()
	writeJSON(w, map[string]interface{}{"ok": true})
}

// --- Packet Handlers ---

func (s *Server) handlePackets(w http.ResponseWriter, r *http.Request) {
	// Multi-node filter: comma-separated pubkeys (Node.js parity)
	if nodesParam := r.URL.Query().Get("nodes"); nodesParam != "" {
		pubkeys := strings.Split(nodesParam, ",")
		var cleaned []string
		for _, pk := range pubkeys {
			pk = strings.TrimSpace(pk)
			if pk != "" {
				cleaned = append(cleaned, pk)
			}
		}
		order := "DESC"
		if r.URL.Query().Get("order") == "asc" {
			order = "ASC"
		}
		var result *PacketResult
		var err error
		if s.store != nil {
			result = s.store.QueryMultiNodePackets(cleaned,
				queryInt(r, "limit", 50), queryInt(r, "offset", 0),
				order, r.URL.Query().Get("since"), r.URL.Query().Get("until"))
		} else {
			result, err = s.db.QueryMultiNodePackets(cleaned,
				queryInt(r, "limit", 50), queryInt(r, "offset", 0),
				order, r.URL.Query().Get("since"), r.URL.Query().Get("until"))
		}
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{
			"packets": result.Packets,
			"total":   result.Total,
			"limit":   queryInt(r, "limit", 50),
			"offset":  queryInt(r, "offset", 0),
		})
		return
	}

	q := PacketQuery{
		Limit:    queryInt(r, "limit", 50),
		Offset:   queryInt(r, "offset", 0),
		Observer: r.URL.Query().Get("observer"),
		Hash:     r.URL.Query().Get("hash"),
		Since:    r.URL.Query().Get("since"),
		Until:    r.URL.Query().Get("until"),
		Region:   r.URL.Query().Get("region"),
		Node:     r.URL.Query().Get("node"),
		Order:    "DESC",
	}
	if r.URL.Query().Get("order") == "asc" {
		q.Order = "ASC"
	}
	if v := r.URL.Query().Get("type"); v != "" {
		t, _ := strconv.Atoi(v)
		q.Type = &t
	}
	if v := r.URL.Query().Get("route"); v != "" {
		t, _ := strconv.Atoi(v)
		q.Route = &t
	}

	if r.URL.Query().Get("groupByHash") == "true" {
		var result *PacketResult
		var err error
		if s.store != nil {
			result = s.store.QueryGroupedPackets(q)
		} else {
			result, err = s.db.QueryGroupedPackets(q)
		}
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, result)
		return
	}

	var result *PacketResult
	var err error
	if s.store != nil {
		result = s.store.QueryPackets(q)
	} else {
		result, err = s.db.QueryPackets(q)
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Strip observations from default response
	if r.URL.Query().Get("expand") != "observations" {
		for _, p := range result.Packets {
			delete(p, "observations")
		}
	}

	writeJSON(w, result)
}

func (s *Server) handlePacketTimestamps(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	if since == "" {
		writeError(w, 400, "since required")
		return
	}
	if s.store != nil {
		writeJSON(w, s.store.GetTimestamps(since))
		return
	}
	ts, err := s.db.GetTimestamps(since)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, ts)
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (s *Server) handlePacketDetail(w http.ResponseWriter, r *http.Request) {
	param := mux.Vars(r)["id"]
	var packet map[string]interface{}
	var err error

	if s.store != nil {
		// Use in-memory store for lookups
		if hashPattern.MatchString(strings.ToLower(param)) {
			packet = s.store.GetPacketByHash(param)
		}
		if packet == nil {
			id, parseErr := strconv.Atoi(param)
			if parseErr == nil {
				packet = s.store.GetTransmissionByID(id)
				if packet == nil {
					packet = s.store.GetPacketByID(id)
				}
			}
		}
	} else {
		// Fallback to DB
		if hashPattern.MatchString(strings.ToLower(param)) {
			packet, err = s.db.GetPacketByHash(param)
		}
		if packet == nil {
			id, parseErr := strconv.Atoi(param)
			if parseErr == nil {
				packet, err = s.db.GetTransmissionByID(id)
				if packet == nil {
					packet, err = s.db.GetPacketByID(id)
				}
			}
		}
	}
	if err != nil || packet == nil {
		writeError(w, 404, "Not found")
		return
	}

	// Build observation list
	hash, _ := packet["hash"].(string)
	var observations []map[string]interface{}
	if s.store != nil {
		observations = s.store.GetObservationsForHash(hash)
	} else {
		observations, _ = s.db.GetObservationsForHash(hash)
	}
	observationCount := len(observations)
	if observationCount == 0 {
		observationCount = 1
	}

	// Parse path from path_json
	var pathHops []interface{}
	if pj, ok := packet["path_json"]; ok && pj != nil {
		if pjStr, ok := pj.(string); ok && pjStr != "" {
			json.Unmarshal([]byte(pjStr), &pathHops)
		}
	}
	if pathHops == nil {
		pathHops = []interface{}{}
	}

	writeJSON(w, map[string]interface{}{
		"packet":            packet,
		"path":              pathHops,
		"breakdown":         map[string]interface{}{},
		"observation_count": observationCount,
		"observations":      observations,
	})
}

func (s *Server) handleDecode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hex string `json:"hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	hexStr := strings.TrimSpace(body.Hex)
	if hexStr == "" {
		writeError(w, 400, "hex is required")
		return
	}
	decoded, err := DecodePacket(hexStr)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"decoded": map[string]interface{}{
			"header":  decoded.Header,
			"path":    decoded.Path,
			"payload": decoded.Payload,
		},
	})
}

func (s *Server) handlePostPacket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hex      string   `json:"hex"`
		Observer *string  `json:"observer"`
		Snr      *float64 `json:"snr"`
		Rssi     *float64 `json:"rssi"`
		Region   *string  `json:"region"`
		Hash     *string  `json:"hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}
	hexStr := strings.TrimSpace(body.Hex)
	if hexStr == "" {
		writeError(w, 400, "hex is required")
		return
	}
	decoded, err := DecodePacket(hexStr)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	contentHash := ComputeContentHash(hexStr)
	pathJSON := "[]"
	if len(decoded.Path.Hops) > 0 {
		if pj, e := json.Marshal(decoded.Path.Hops); e == nil {
			pathJSON = string(pj)
		}
	}
	decodedJSON := PayloadJSON(&decoded.Payload)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	var obsID, obsName interface{}
	if body.Observer != nil {
		obsID = *body.Observer
	}
	var snr, rssi interface{}
	if body.Snr != nil {
		snr = *body.Snr
	}
	if body.Rssi != nil {
		rssi = *body.Rssi
	}

	res, dbErr := s.db.conn.Exec(`INSERT INTO transmissions (hash, raw_hex, route_type, payload_type, payload_version, path_json, decoded_json, first_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		contentHash, strings.ToUpper(hexStr), decoded.Header.RouteType, decoded.Header.PayloadType,
		decoded.Header.PayloadVersion, pathJSON, decodedJSON, now)

	var insertedID int64
	if dbErr == nil {
		insertedID, _ = res.LastInsertId()
		s.db.conn.Exec(`INSERT INTO observations (transmission_id, observer_id, observer_name, snr, rssi, timestamp)
			VALUES (?, ?, ?, ?, ?, ?)`,
			insertedID, obsID, obsName, snr, rssi, now)
	}

	writeJSON(w, map[string]interface{}{
		"id": insertedID,
		"decoded": map[string]interface{}{
			"header":  decoded.Header,
			"path":    decoded.Path,
			"payload": decoded.Payload,
		},
	})
}

// --- Node Handlers ---

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nodes, total, counts, err := s.db.GetNodes(
		queryInt(r, "limit", 50),
		queryInt(r, "offset", 0),
		q.Get("role"), q.Get("search"), q.Get("before"),
		q.Get("lastHeard"), q.Get("sortBy"), q.Get("region"),
	)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if s.store != nil {
		hashInfo := s.store.GetNodeHashSizeInfo()
		for _, node := range nodes {
			if pk, ok := node["public_key"].(string); ok {
				EnrichNodeWithHashSize(node, hashInfo[pk])
			}
		}
	}
	writeJSON(w, map[string]interface{}{"nodes": nodes, "total": total, "counts": counts})
}

func (s *Server) handleNodeSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeJSON(w, map[string]interface{}{"nodes": []interface{}{}})
		return
	}
	nodes, err := s.db.SearchNodes(strings.TrimSpace(q), 10)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"nodes": nodes})
}

func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	pubkey := mux.Vars(r)["pubkey"]
	node, err := s.db.GetNodeByPubkey(pubkey)
	if err != nil || node == nil {
		writeError(w, 404, "Not found")
		return
	}

	if s.store != nil {
		hashInfo := s.store.GetNodeHashSizeInfo()
		EnrichNodeWithHashSize(node, hashInfo[pubkey])
	}

	name := ""
	if n, ok := node["name"]; ok && n != nil {
		name = fmt.Sprintf("%v", n)
	}
	recentAdverts, _ := s.db.GetRecentTransmissionsForNode(pubkey, name, 20)

	writeJSON(w, map[string]interface{}{
		"node":          node,
		"recentAdverts": recentAdverts,
	})
}

func (s *Server) handleNodeHealth(w http.ResponseWriter, r *http.Request) {
	pubkey := mux.Vars(r)["pubkey"]
	result, err := s.db.GetNodeHealth(pubkey)
	if err != nil || result == nil {
		writeError(w, 404, "Not found")
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleBulkHealth(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	if limit > 200 {
		limit = 200
	}

	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetBulkHealth(limit, region))
		return
	}

	rows, err := s.db.conn.Query("SELECT public_key, name, role, lat, lon, last_seen FROM nodes ORDER BY last_seen DESC LIMIT ?", limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()

	type nodeDbInfo struct {
		pk, name, role, lastSeen string
		lat, lon                 interface{}
	}
	var nodes []nodeDbInfo
	for rows.Next() {
		var pk string
		var name, role, lastSeen sql.NullString
		var lat, lon sql.NullFloat64
		rows.Scan(&pk, &name, &role, &lat, &lon, &lastSeen)
		nodes = append(nodes, nodeDbInfo{
			pk: pk, name: nullStrVal(name), role: nullStrVal(role),
			lastSeen: nullStrVal(lastSeen),
			lat:      nullFloat(lat), lon: nullFloat(lon),
		})
	}

	// Batch query: per-node transmission stats
	todayStart := time.Now().UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	results := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		pk := "%" + n.pk + "%"
		np := "%" + n.name + "%"

		var txCount, obsCount, packetsToday int
		var avgSnr sql.NullFloat64
		var lastHeard sql.NullString

		whereClause := "t.decoded_json LIKE ?"
		queryArgs := []interface{}{pk}
		if n.name != "" {
			whereClause = "(t.decoded_json LIKE ? OR t.decoded_json LIKE ?)"
			queryArgs = []interface{}{pk, np}
		}

		s.db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM transmissions t WHERE %s", whereClause), queryArgs...).Scan(&txCount)

		if txCount > 0 {
			// Observation count
			s.db.conn.QueryRow(fmt.Sprintf(`SELECT COALESCE(SUM(
				(SELECT COUNT(*) FROM observations oi WHERE oi.transmission_id = t.id)
			), 0) FROM transmissions t WHERE %s`, whereClause), queryArgs...).Scan(&obsCount)

			// Packets today
			todayArgs := append(queryArgs, todayStart)
			s.db.conn.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM transmissions t WHERE %s AND t.first_seen > ?", whereClause), todayArgs...).Scan(&packetsToday)

			// Avg SNR from best observation per transmission
			s.db.conn.QueryRow(fmt.Sprintf(`SELECT AVG(o.snr) FROM transmissions t
				LEFT JOIN observations o ON o.id = (
					SELECT id FROM observations WHERE transmission_id = t.id AND snr IS NOT NULL LIMIT 1
				) WHERE %s AND o.snr IS NOT NULL`, whereClause), queryArgs...).Scan(&avgSnr)

			// Last heard
			s.db.conn.QueryRow(fmt.Sprintf("SELECT MAX(t.first_seen) FROM transmissions t WHERE %s", whereClause), queryArgs...).Scan(&lastHeard)
		}

		lh := n.lastSeen
		if lastHeard.Valid && lastHeard.String > lh {
			lh = lastHeard.String
		}

		// Per-observer breakdown
		pvWhere := "pv.decoded_json LIKE ?"
		if n.name != "" {
			pvWhere = "(pv.decoded_json LIKE ? OR pv.decoded_json LIKE ?)"
		}
		obsSQL := fmt.Sprintf(`SELECT pv.observer_id, pv.observer_name, AVG(pv.snr) as avgSnr, AVG(pv.rssi) as avgRssi, COUNT(*) as packetCount
			FROM packets_v pv WHERE %s AND pv.observer_id IS NOT NULL
			GROUP BY pv.observer_id ORDER BY packetCount DESC`, pvWhere)
		obsRows, _ := s.db.conn.Query(obsSQL, queryArgs...)
		observers := make([]map[string]interface{}, 0)
		if obsRows != nil {
			for obsRows.Next() {
				var oID, oName sql.NullString
				var oSnr, oRssi sql.NullFloat64
				var oPktCount int
				obsRows.Scan(&oID, &oName, &oSnr, &oRssi, &oPktCount)
				observers = append(observers, map[string]interface{}{
					"observer_id": nullStr(oID), "observer_name": nullStr(oName),
					"avgSnr": nullFloat(oSnr), "avgRssi": nullFloat(oRssi),
					"packetCount": oPktCount,
				})
			}
			obsRows.Close()
		}

		results = append(results, map[string]interface{}{
			"public_key": n.pk,
			"name":       nilIfEmpty(n.name),
			"role":       nilIfEmpty(n.role),
			"lat":        n.lat,
			"lon":        n.lon,
			"stats": map[string]interface{}{
				"totalTransmissions": txCount,
				"totalObservations":  obsCount,
				"totalPackets":       txCount,
				"packetsToday":       packetsToday,
				"avgSnr":             nullFloat(avgSnr),
				"lastHeard":          nilIfEmpty(lh),
			},
			"observers": observers,
		})
	}
	writeJSON(w, results)
}

func (s *Server) handleNetworkStatus(w http.ResponseWriter, r *http.Request) {
	ht := s.cfg.GetHealthThresholds()
	result, err := s.db.GetNetworkStatus(ht)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleNodePaths(w http.ResponseWriter, r *http.Request) {
	pubkey := mux.Vars(r)["pubkey"]
	node, err := s.db.GetNodeByPubkey(pubkey)
	if err != nil || node == nil {
		writeError(w, 404, "Not found")
		return
	}

	pk := "%" + pubkey[:8] + "%"
	name := ""
	if n, ok := node["name"]; ok && n != nil {
		name = fmt.Sprintf("%v", n)
	}

	whereClause := "path_json LIKE ?"
	args := []interface{}{pk}
	if name != "" {
		whereClause = "(path_json LIKE ? OR path_json LIKE ?)"
		args = append(args, "%"+name+"%")
	}

	pathSQL := fmt.Sprintf("SELECT path_json, hash, MAX(timestamp) as lastSeen, COUNT(*) as cnt FROM packets_v WHERE path_json IS NOT NULL AND path_json != '[]' AND %s GROUP BY path_json ORDER BY cnt DESC LIMIT 50", whereClause)
	rows, _ := s.db.conn.Query(pathSQL, args...)

	paths := make([]map[string]interface{}, 0)
	var totalPaths, totalTx int
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var pj, hash string
			var lastSeen sql.NullString
			var cnt int
			rows.Scan(&pj, &hash, &lastSeen, &cnt)
			var hops []string
			if json.Unmarshal([]byte(pj), &hops) != nil {
				continue
			}
			hopEntries := make([]map[string]interface{}, 0, len(hops))
			for _, h := range hops {
				hopEntries = append(hopEntries, map[string]interface{}{
					"prefix": h, "name": h, "pubkey": nil, "lat": nil, "lon": nil,
				})
			}
			paths = append(paths, map[string]interface{}{
				"hops": hopEntries, "count": cnt,
				"lastSeen": nullStr(lastSeen), "sampleHash": hash,
			})
			totalPaths++
			totalTx += cnt
		}
	}

	writeJSON(w, map[string]interface{}{
		"node": map[string]interface{}{
			"public_key": node["public_key"],
			"name":       node["name"],
			"lat":        node["lat"],
			"lon":        node["lon"],
		},
		"paths":              paths,
		"totalPaths":         totalPaths,
		"totalTransmissions": totalTx,
	})
}

func (s *Server) handleNodeAnalytics(w http.ResponseWriter, r *http.Request) {
	pubkey := mux.Vars(r)["pubkey"]
	days := queryInt(r, "days", 7)
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}

	node, err := s.db.GetNodeByPubkey(pubkey)
	if err != nil || node == nil {
		writeError(w, 404, "Not found")
		return
	}

	name := ""
	if n, ok := node["name"]; ok && n != nil {
		name = fmt.Sprintf("%v", n)
	}

	fromISO := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	toISO := time.Now().Format(time.RFC3339)

	pk := "%" + pubkey + "%"
	np := "%" + name + "%"
	whereClause := "decoded_json LIKE ? OR decoded_json LIKE ?"
	if name == "" {
		whereClause = "decoded_json LIKE ?"
		np = pk
	}
	timeWhere := fmt.Sprintf("(%s) AND timestamp > ?", whereClause)

	// Activity timeline
	actSQL := fmt.Sprintf(`SELECT substr(timestamp, 1, 13) || ':00:00Z' as bucket, COUNT(*) as count
		FROM packets_v WHERE %s GROUP BY bucket ORDER BY bucket`, timeWhere)
	aRows, _ := s.db.conn.Query(actSQL, pk, np, fromISO)
	activityTimeline := make([]map[string]interface{}, 0)
	if aRows != nil {
		defer aRows.Close()
		for aRows.Next() {
			var bucket string
			var count int
			aRows.Scan(&bucket, &count)
			activityTimeline = append(activityTimeline, map[string]interface{}{"bucket": bucket, "count": count})
		}
	}

	// SNR trend
	snrSQL := fmt.Sprintf(`SELECT timestamp, snr, rssi, observer_id, observer_name
		FROM packets_v WHERE %s AND snr IS NOT NULL ORDER BY timestamp`, timeWhere)
	sRows, _ := s.db.conn.Query(snrSQL, pk, np, fromISO)
	snrTrend := make([]map[string]interface{}, 0)
	if sRows != nil {
		defer sRows.Close()
		for sRows.Next() {
			var ts string
			var snr, rssi sql.NullFloat64
			var obsID, obsName sql.NullString
			sRows.Scan(&ts, &snr, &rssi, &obsID, &obsName)
			snrTrend = append(snrTrend, map[string]interface{}{
				"timestamp": ts, "snr": nullFloat(snr), "rssi": nullFloat(rssi),
				"observer_id": nullStr(obsID), "observer_name": nullStr(obsName),
			})
		}
	}

	// Packet type breakdown
	ptSQL := fmt.Sprintf("SELECT payload_type, COUNT(*) as count FROM packets_v WHERE %s GROUP BY payload_type", timeWhere)
	ptRows, _ := s.db.conn.Query(ptSQL, pk, np, fromISO)
	packetTypeBreakdown := make([]map[string]interface{}, 0)
	if ptRows != nil {
		defer ptRows.Close()
		for ptRows.Next() {
			var pt, count int
			ptRows.Scan(&pt, &count)
			packetTypeBreakdown = append(packetTypeBreakdown, map[string]interface{}{"payload_type": pt, "count": count})
		}
	}

	// Observer coverage
	ocSQL := fmt.Sprintf(`SELECT observer_id, observer_name, COUNT(*) as packetCount,
		AVG(snr) as avgSnr, AVG(rssi) as avgRssi, MIN(timestamp) as firstSeen, MAX(timestamp) as lastSeen
		FROM packets_v WHERE %s AND observer_id IS NOT NULL
		GROUP BY observer_id ORDER BY packetCount DESC`, timeWhere)
	ocRows, _ := s.db.conn.Query(ocSQL, pk, np, fromISO)
	observerCoverage := make([]map[string]interface{}, 0)
	if ocRows != nil {
		defer ocRows.Close()
		for ocRows.Next() {
			var obsID, obsName, first, last sql.NullString
			var pktCount int
			var avgSnr, avgRssi sql.NullFloat64
			ocRows.Scan(&obsID, &obsName, &pktCount, &avgSnr, &avgRssi, &first, &last)
			observerCoverage = append(observerCoverage, map[string]interface{}{
				"observer_id": nullStr(obsID), "observer_name": nullStr(obsName),
				"packetCount": pktCount, "avgSnr": nullFloat(avgSnr), "avgRssi": nullFloat(avgRssi),
				"firstSeen": nullStr(first), "lastSeen": nullStr(last),
			})
		}
	}

	// Hop distribution from path_json
	hdSQL := fmt.Sprintf(`SELECT path_json FROM packets_v WHERE %s AND path_json IS NOT NULL AND path_json != '[]'`, timeWhere)
	hdRows, _ := s.db.conn.Query(hdSQL, pk, np, fromISO)
	hopCounts := map[string]int{}
	if hdRows != nil {
		defer hdRows.Close()
		for hdRows.Next() {
			var pj string
			hdRows.Scan(&pj)
			var hops []interface{}
			if json.Unmarshal([]byte(pj), &hops) == nil {
				key := fmt.Sprintf("%d", len(hops))
				if len(hops) >= 4 {
					key = "4+"
				}
				hopCounts[key]++
			}
		}
	}
	// Also count zero-hop packets
	zhSQL := fmt.Sprintf(`SELECT COUNT(*) FROM packets_v WHERE %s AND (path_json IS NULL OR path_json = '[]')`, timeWhere)
	var zeroHops int
	s.db.conn.QueryRow(zhSQL, pk, np, fromISO).Scan(&zeroHops)
	if zeroHops > 0 {
		hopCounts["0"] += zeroHops
	}
	hopDistribution := make([]map[string]interface{}, 0)
	for _, h := range []string{"0", "1", "2", "3", "4+"} {
		if c, ok := hopCounts[h]; ok {
			hopDistribution = append(hopDistribution, map[string]interface{}{"hops": h, "count": c})
		}
	}

	// Uptime heatmap
	uhSQL := fmt.Sprintf(`SELECT
		CAST(strftime('%%w', timestamp) AS INTEGER) as dayOfWeek,
		CAST(strftime('%%H', timestamp) AS INTEGER) as hour,
		COUNT(*) as count
		FROM packets_v WHERE %s
		GROUP BY dayOfWeek, hour ORDER BY count DESC`, timeWhere)
	uhRows, _ := s.db.conn.Query(uhSQL, pk, np, fromISO)
	uptimeHeatmap := make([]map[string]interface{}, 0)
	if uhRows != nil {
		defer uhRows.Close()
		for uhRows.Next() {
			var dow, hr, cnt int
			uhRows.Scan(&dow, &hr, &cnt)
			uptimeHeatmap = append(uptimeHeatmap, map[string]interface{}{"dayOfWeek": dow, "hour": hr, "count": cnt})
		}
	}

	// Computed stats
	totalPackets := 0
	for _, entry := range activityTimeline {
		if c, ok := entry["count"].(int); ok {
			totalPackets += c
		}
	}

	var snrMean, snrStdDev float64
	var snrCount int
	snrStatsSQL := fmt.Sprintf(`SELECT COUNT(*), COALESCE(AVG(snr),0), COALESCE(
		SQRT(AVG(snr*snr) - AVG(snr)*AVG(snr)), 0)
		FROM packets_v WHERE %s AND snr IS NOT NULL`, timeWhere)
	s.db.conn.QueryRow(snrStatsSQL, pk, np, fromISO).Scan(&snrCount, &snrMean, &snrStdDev)
	_ = snrCount

	signalGrade := "D"
	if snrMean >= 10 {
		signalGrade = "A"
	} else if snrMean >= 7 {
		signalGrade = "A-"
	} else if snrMean >= 4 {
		signalGrade = "B+"
	} else if snrMean >= 1 {
		signalGrade = "B"
	} else if snrMean >= -3 {
		signalGrade = "C"
	}

	relayCount := 0
	for _, h := range []string{"1", "2", "3", "4+"} {
		relayCount += hopCounts[h]
	}
	var relayPct float64
	if totalPackets > 0 {
		relayPct = round(float64(relayCount)*100.0/float64(totalPackets), 1)
	}

	var avgPacketsPerDay float64
	if days > 0 {
		avgPacketsPerDay = round(float64(totalPackets)/float64(days), 1)
	}

	// Longest silence
	var longestSilenceMs int
	var longestSilenceStart interface{}
	if len(activityTimeline) >= 2 {
		for i := 1; i < len(activityTimeline); i++ {
			t1Str, _ := activityTimeline[i-1]["bucket"].(string)
			t2Str, _ := activityTimeline[i]["bucket"].(string)
			t1, e1 := time.Parse(time.RFC3339, t1Str)
			t2, e2 := time.Parse(time.RFC3339, t2Str)
			if e1 == nil && e2 == nil {
				gap := int(t2.Sub(t1).Milliseconds())
				if gap > longestSilenceMs {
					longestSilenceMs = gap
					longestSilenceStart = t1Str
				}
			}
		}
	}

	// Availability
	totalHours := float64(days) * 24
	activeHours := float64(len(activityTimeline))
	availabilityPct := round(activeHours*100.0/totalHours, 1)
	if availabilityPct > 100 {
		availabilityPct = 100
	}

	writeJSON(w, map[string]interface{}{
		"node":                node,
		"timeRange":           map[string]interface{}{"from": fromISO, "to": toISO, "days": days},
		"activityTimeline":    activityTimeline,
		"snrTrend":            snrTrend,
		"packetTypeBreakdown": packetTypeBreakdown,
		"observerCoverage":    observerCoverage,
		"hopDistribution":     hopDistribution,
		"peerInteractions":    []interface{}{},
		"uptimeHeatmap":       uptimeHeatmap,
		"computedStats": map[string]interface{}{
			"availabilityPct":     availabilityPct,
			"longestSilenceMs":    longestSilenceMs,
			"longestSilenceStart": longestSilenceStart,
			"signalGrade":         signalGrade,
			"snrMean":             round(snrMean, 1),
			"snrStdDev":           round(snrStdDev, 1),
			"relayPct":            relayPct,
			"totalPackets":        totalPackets,
			"uniqueObservers":     len(observerCoverage),
			"uniquePeers":         0,
			"avgPacketsPerDay":    avgPacketsPerDay,
		},
	})
}

// --- Analytics Handlers ---

func (s *Server) handleAnalyticsRF(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetAnalyticsRF(region))
		return
	}
	// Fallback: basic RF analytics from SQL
	region := r.URL.Query().Get("region")
	regionFilter := ""
	var rArgs []interface{}
	if region != "" {
		regionFilter = "AND observer_id IN (SELECT id FROM observers WHERE iata = ?)"
		rArgs = append(rArgs, region)
	}

	// SNR/RSSI stats
	rfSQL := fmt.Sprintf(`SELECT COUNT(*) as cnt, AVG(snr) as avgSnr, MIN(snr) as minSnr, MAX(snr) as maxSnr,
		AVG(rssi) as avgRssi, MIN(rssi) as minRssi, MAX(rssi) as maxRssi
		FROM packets_v WHERE snr IS NOT NULL %s`, regionFilter)
	var cnt int
	var avgSnr, minSnr, maxSnr, avgRssi, minRssi, maxRssi sql.NullFloat64
	s.db.conn.QueryRow(rfSQL, rArgs...).Scan(&cnt, &avgSnr, &minSnr, &maxSnr, &avgRssi, &minRssi, &maxRssi)

	// Payload type distribution
	ptSQL := fmt.Sprintf(`SELECT payload_type, COUNT(DISTINCT hash) as count FROM packets_v WHERE 1=1 %s GROUP BY payload_type ORDER BY count DESC`, regionFilter)
	ptRows, _ := s.db.conn.Query(ptSQL, rArgs...)
	payloadTypes := make([]map[string]interface{}, 0)
	ptNames := map[int]string{0: "REQ", 1: "RESPONSE", 2: "TXT_MSG", 3: "ACK", 4: "ADVERT", 5: "GRP_TXT", 7: "ANON_REQ", 8: "PATH", 9: "TRACE", 11: "CONTROL"}
	if ptRows != nil {
		defer ptRows.Close()
		for ptRows.Next() {
			var pt, count int
			ptRows.Scan(&pt, &count)
			name := ptNames[pt]
			if name == "" {
				name = fmt.Sprintf("UNK(%d)", pt)
			}
			payloadTypes = append(payloadTypes, map[string]interface{}{"type": pt, "name": name, "count": count})
		}
	}

	// Total counts
	var totalAll int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM packets_v WHERE 1=1 %s", regionFilter)
	s.db.conn.QueryRow(countSQL, rArgs...).Scan(&totalAll)
	var totalTx int
	txSQL := fmt.Sprintf("SELECT COUNT(DISTINCT hash) FROM packets_v WHERE 1=1 %s", regionFilter)
	s.db.conn.QueryRow(txSQL, rArgs...).Scan(&totalTx)

	writeJSON(w, map[string]interface{}{
		"totalPackets":      cnt,
		"totalAllPackets":   totalAll,
		"totalTransmissions": totalTx,
		"snr": map[string]interface{}{
			"min": nullFloat(minSnr), "max": nullFloat(maxSnr),
			"avg": nullFloat(avgSnr), "median": 0, "stddev": 0,
		},
		"rssi": map[string]interface{}{
			"min": nullFloat(minRssi), "max": nullFloat(maxRssi),
			"avg": nullFloat(avgRssi), "median": 0, "stddev": 0,
		},
		"snrValues":     map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0},
		"rssiValues":    map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0},
		"packetSizes":   map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0},
		"minPacketSize": 0, "maxPacketSize": 0, "avgPacketSize": 0,
		"packetsPerHour":  []interface{}{},
		"payloadTypes":    payloadTypes,
		"snrByType":       []interface{}{},
		"signalOverTime":  []interface{}{},
		"scatterData":     []interface{}{},
		"timeSpanHours":   0,
	})
}

func (s *Server) handleAnalyticsTopology(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetAnalyticsTopology(region))
		return
	}
	// SQL fallback — compute basic topology from path_json
	region := r.URL.Query().Get("region")
	regionFilter := ""
	var rArgs []interface{}
	if region != "" {
		regionFilter = "AND observer_id IN (SELECT id FROM observers WHERE iata = ?)"
		rArgs = append(rArgs, region)
	}

	pathSQL := fmt.Sprintf("SELECT path_json, snr FROM packets_v WHERE path_json IS NOT NULL AND path_json != '[]' %s", regionFilter)
	pathRows, _ := s.db.conn.Query(pathSQL, rArgs...)

	hopCountMap := map[int]int{}
	repeaterCounts := map[string]int{}
	pairCounts := map[string]int{}
	nodesSeen := map[string]bool{}
	hopsVsSnrSum := map[int]float64{}
	hopsVsSnrCnt := map[int]int{}

	if pathRows != nil {
		defer pathRows.Close()
		for pathRows.Next() {
			var pj string
			var snr sql.NullFloat64
			pathRows.Scan(&pj, &snr)
			var hops []string
			if json.Unmarshal([]byte(pj), &hops) != nil || len(hops) == 0 {
				continue
			}
			hc := len(hops)
			if hc > 25 {
				hc = 25
			}
			hopCountMap[hc]++
			for _, h := range hops {
				nodesSeen[h] = true
				repeaterCounts[h]++
			}
			for i := 0; i+1 < len(hops); i++ {
				pair := hops[i] + ":" + hops[i+1]
				pairCounts[pair]++
			}
			if snr.Valid {
				hopsVsSnrSum[hc] += snr.Float64
				hopsVsSnrCnt[hc]++
			}
		}
	}

	var totalHopCount, maxHops int
	for h, c := range hopCountMap {
		totalHopCount += h * c
		if h > maxHops {
			maxHops = h
		}
	}
	totalPaths := 0
	for _, c := range hopCountMap {
		totalPaths += c
	}
	var avgHops float64
	if totalPaths > 0 {
		avgHops = round(float64(totalHopCount)/float64(totalPaths), 1)
	}

	hopDistribution := make([]map[string]interface{}, 0)
	for h := 1; h <= maxHops; h++ {
		if c, ok := hopCountMap[h]; ok {
			hopDistribution = append(hopDistribution, map[string]interface{}{"hops": h, "count": c})
		}
	}

	type kv struct {
		k string
		v int
	}
	var repSorted []kv
	for k, v := range repeaterCounts {
		repSorted = append(repSorted, kv{k, v})
	}
	sort.Slice(repSorted, func(i, j int) bool { return repSorted[i].v > repSorted[j].v })
	topRepeaters := make([]map[string]interface{}, 0)
	for i, rp := range repSorted {
		if i >= 20 {
			break
		}
		topRepeaters = append(topRepeaters, map[string]interface{}{"hop": rp.k, "count": rp.v, "name": nil, "pubkey": nil})
	}

	var pairSorted []kv
	for k, v := range pairCounts {
		pairSorted = append(pairSorted, kv{k, v})
	}
	sort.Slice(pairSorted, func(i, j int) bool { return pairSorted[i].v > pairSorted[j].v })
	topPairs := make([]map[string]interface{}, 0)
	for i, p := range pairSorted {
		if i >= 20 {
			break
		}
		parts := strings.SplitN(p.k, ":", 2)
		topPairs = append(topPairs, map[string]interface{}{
			"hopA": parts[0], "hopB": parts[1], "count": p.v,
			"nameA": nil, "nameB": nil, "pubkeyA": nil, "pubkeyB": nil,
		})
	}

	hopsVsSnr := make([]map[string]interface{}, 0)
	for h := 1; h <= maxHops; h++ {
		if cnt, ok := hopsVsSnrCnt[h]; ok && cnt > 0 {
			hopsVsSnr = append(hopsVsSnr, map[string]interface{}{
				"hops": h, "count": cnt, "avgSnr": round(hopsVsSnrSum[h]/float64(cnt), 1),
			})
		}
	}

	obsList := make([]map[string]interface{}, 0)
	obsRows, _ := s.db.conn.Query("SELECT id, name FROM observers")
	if obsRows != nil {
		defer obsRows.Close()
		for obsRows.Next() {
			var oid string
			var oname sql.NullString
			obsRows.Scan(&oid, &oname)
			obsList = append(obsList, map[string]interface{}{"id": oid, "name": nullStr(oname)})
		}
	}

	writeJSON(w, map[string]interface{}{
		"uniqueNodes":      len(nodesSeen),
		"avgHops":          avgHops,
		"medianHops":       0,
		"maxHops":          maxHops,
		"hopDistribution":  hopDistribution,
		"topRepeaters":     topRepeaters,
		"topPairs":         topPairs,
		"hopsVsSnr":        hopsVsSnr,
		"observers":        obsList,
		"perObserverReach": map[string]interface{}{},
		"multiObsNodes":    []interface{}{},
		"bestPathList":     []interface{}{},
	})
}

func (s *Server) handleAnalyticsChannels(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetAnalyticsChannels(region))
		return
	}
	channels, _ := s.db.GetChannels()
	if channels == nil {
		channels = make([]map[string]interface{}, 0)
	}
	writeJSON(w, map[string]interface{}{
		"activeChannels":  len(channels),
		"decryptable":     len(channels),
		"channels":        channels,
		"topSenders":      []interface{}{},
		"channelTimeline": []interface{}{},
		"msgLengths":      []interface{}{},
	})
}

func (s *Server) handleAnalyticsDistance(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetAnalyticsDistance(region))
		return
	}
	// SQL fallback
	region := r.URL.Query().Get("region")
	regionFilter := ""
	var rArgs []interface{}
	if region != "" {
		regionFilter = "AND observer_id IN (SELECT id FROM observers WHERE iata = ?)"
		rArgs = append(rArgs, region)
	}

	nodeLocMap := s.db.GetNodeLocations()
	_ = nodeLocMap

	pathSQL := fmt.Sprintf("SELECT path_json, hash, timestamp, snr FROM packets_v WHERE path_json IS NOT NULL AND path_json != '[]' %s ORDER BY timestamp DESC LIMIT 5000", regionFilter)
	pathRows, _ := s.db.conn.Query(pathSQL, rArgs...)

	var totalHops, totalPaths int
	var maxDist, distSum float64
	topHops := make([]map[string]interface{}, 0)
	topPaths := make([]map[string]interface{}, 0)
	catStats := map[string]interface{}{}
	distOverTime := make([]map[string]interface{}, 0)

	if pathRows != nil {
		defer pathRows.Close()
		for pathRows.Next() {
			var pj, hash string
			var ts sql.NullString
			var snr sql.NullFloat64
			pathRows.Scan(&pj, &hash, &ts, &snr)
			var hops []string
			if json.Unmarshal([]byte(pj), &hops) != nil || len(hops) == 0 {
				continue
			}
			totalPaths++
			totalHops += len(hops)
		}
	}

	var avgDist float64
	if totalHops > 0 {
		avgDist = round(distSum/float64(totalHops), 2)
	}

	writeJSON(w, map[string]interface{}{
		"summary": map[string]interface{}{
			"totalHops": totalHops, "totalPaths": totalPaths,
			"avgDist": avgDist, "maxDist": maxDist,
		},
		"topHops":       topHops,
		"topPaths":      topPaths,
		"catStats":      catStats,
		"distHistogram": []interface{}{},
		"distOverTime":  distOverTime,
	})
}

func (s *Server) handleAnalyticsHashSizes(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		writeJSON(w, s.store.GetAnalyticsHashSizes(region))
		return
	}
	writeJSON(w, map[string]interface{}{
		"total":          0,
		"distribution":   map[string]int{"1": 0, "2": 0, "3": 0},
		"hourly":         []interface{}{},
		"topHops":        []interface{}{},
		"multiByteNodes": []interface{}{},
	})
}

func (s *Server) handleAnalyticsSubpaths(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		minLen := queryInt(r, "minLen", 2)
		if minLen < 2 {
			minLen = 2
		}
		maxLen := queryInt(r, "maxLen", 8)
		limit := queryInt(r, "limit", 100)
		writeJSON(w, s.store.GetAnalyticsSubpaths(region, minLen, maxLen, limit))
		return
	}
	writeJSON(w, map[string]interface{}{
		"subpaths":   []interface{}{},
		"totalPaths": 0,
	})
}

func (s *Server) handleAnalyticsSubpathDetail(w http.ResponseWriter, r *http.Request) {
	hops := r.URL.Query().Get("hops")
	if hops == "" {
		writeJSON(w, map[string]interface{}{"error": "Need at least 2 hops"})
		return
	}
	rawHops := strings.Split(hops, ",")
	if len(rawHops) < 2 {
		writeJSON(w, map[string]interface{}{"error": "Need at least 2 hops"})
		return
	}
	if s.store != nil {
		writeJSON(w, s.store.GetSubpathDetail(rawHops))
		return
	}
	writeJSON(w, map[string]interface{}{
		"hops":             rawHops,
		"nodes":            []interface{}{},
		"totalMatches":     0,
		"firstSeen":        nil,
		"lastSeen":         nil,
		"signal":           map[string]interface{}{"avgSnr": nil, "avgRssi": nil, "samples": 0},
		"hourDistribution": make([]int, 24),
		"parentPaths":      []interface{}{},
		"observers":        []interface{}{},
	})
}

// --- Other Handlers ---

func (s *Server) handleResolveHops(w http.ResponseWriter, r *http.Request) {
	hopsParam := r.URL.Query().Get("hops")
	if hopsParam == "" {
		writeJSON(w, map[string]interface{}{"resolved": map[string]interface{}{}})
		return
	}
	hops := strings.Split(hopsParam, ",")
	resolved := map[string]interface{}{}

	for _, hop := range hops {
		if hop == "" {
			continue
		}
		hopLower := strings.ToLower(hop)
		rows, err := s.db.conn.Query("SELECT public_key, name, lat, lon FROM nodes WHERE LOWER(public_key) LIKE ?", hopLower+"%")
		if err != nil {
			resolved[hop] = map[string]interface{}{"name": nil, "candidates": []interface{}{}, "conflicts": []interface{}{}}
			continue
		}

		var candidates []map[string]interface{}
		for rows.Next() {
			var pk string
			var name sql.NullString
			var lat, lon sql.NullFloat64
			rows.Scan(&pk, &name, &lat, &lon)
			candidates = append(candidates, map[string]interface{}{
				"name": nullStr(name), "pubkey": pk,
				"lat": nullFloat(lat), "lon": nullFloat(lon),
			})
		}
		rows.Close()

		if len(candidates) == 0 {
			resolved[hop] = map[string]interface{}{"name": nil, "candidates": []interface{}{}, "conflicts": []interface{}{}}
		} else if len(candidates) == 1 {
			resolved[hop] = map[string]interface{}{
				"name": candidates[0]["name"], "pubkey": candidates[0]["pubkey"],
				"candidates": candidates, "conflicts": []interface{}{},
			}
		} else {
			resolved[hop] = map[string]interface{}{
				"name": candidates[0]["name"], "pubkey": candidates[0]["pubkey"],
				"ambiguous": true, "candidates": candidates, "conflicts": candidates,
			}
		}
	}
	writeJSON(w, map[string]interface{}{"resolved": resolved})
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		region := r.URL.Query().Get("region")
		channels := s.store.GetChannels(region)
		writeJSON(w, map[string]interface{}{"channels": channels})
		return
	}
	channels, err := s.db.GetChannels()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"channels": channels})
}

func (s *Server) handleChannelMessages(w http.ResponseWriter, r *http.Request) {
	hash := mux.Vars(r)["hash"]
	limit := queryInt(r, "limit", 100)
	offset := queryInt(r, "offset", 0)
	if s.store != nil {
		messages, total := s.store.GetChannelMessages(hash, limit, offset)
		writeJSON(w, map[string]interface{}{"messages": messages, "total": total})
		return
	}
	messages, total, err := s.db.GetChannelMessages(hash, limit, offset)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"messages": messages, "total": total})
}

func (s *Server) handleObservers(w http.ResponseWriter, r *http.Request) {
	observers, err := s.db.GetObservers()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Batch lookup: packetsLastHour per observer
	oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
	pktCounts := s.db.GetObserverPacketCounts(oneHourAgo)

	// Batch lookup: node locations (observer ID may match a node public_key)
	nodeLocations := s.db.GetNodeLocations()

	result := make([]map[string]interface{}, 0, len(observers))
	for _, o := range observers {
		plh := 0
		if c, ok := pktCounts[o.ID]; ok {
			plh = c
		}
		var lat, lon, nodeRole interface{}
		if nodeLoc, ok := nodeLocations[strings.ToLower(o.ID)]; ok {
			lat = nodeLoc["lat"]
			lon = nodeLoc["lon"]
			nodeRole = nodeLoc["role"]
		}

		m := map[string]interface{}{
			"id": o.ID, "name": o.Name, "iata": o.IATA,
			"last_seen": o.LastSeen, "first_seen": o.FirstSeen,
			"packet_count": o.PacketCount,
			"model": o.Model, "firmware": o.Firmware,
			"client_version": o.ClientVersion, "radio": o.Radio,
			"battery_mv": o.BatteryMv, "uptime_secs": o.UptimeSecs,
			"noise_floor": o.NoiseFloor,
			"packetsLastHour": plh,
			"lat": lat, "lon": lon, "nodeRole": nodeRole,
		}
		result = append(result, m)
	}
	writeJSON(w, map[string]interface{}{
		"observers":   result,
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleObserverDetail(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	obs, err := s.db.GetObserverByID(id)
	if err != nil || obs == nil {
		writeError(w, 404, "Observer not found")
		return
	}

	// Compute packetsLastHour from observations
	oneHourAgo := time.Now().Add(-1 * time.Hour).Unix()
	pktCounts := s.db.GetObserverPacketCounts(oneHourAgo)
	plh := 0
	if c, ok := pktCounts[id]; ok {
		plh = c
	}

	writeJSON(w, map[string]interface{}{
		"id": obs.ID, "name": obs.Name, "iata": obs.IATA,
		"last_seen": obs.LastSeen, "first_seen": obs.FirstSeen,
		"packet_count": obs.PacketCount,
		"model": obs.Model, "firmware": obs.Firmware,
		"client_version": obs.ClientVersion, "radio": obs.Radio,
		"battery_mv": obs.BatteryMv, "uptime_secs": obs.UptimeSecs,
		"noise_floor": obs.NoiseFloor,
		"packetsLastHour": plh,
	})
}

func (s *Server) handleObserverAnalytics(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	days := queryInt(r, "days", 7)
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)

	// Timeline
	bucketH := 4
	if days <= 1 {
		bucketH = 1
	} else if days > 7 {
		bucketH = 24
	}
	// Timeline — packet count per time bucket
	bucketFmt := fmt.Sprintf("strftime('%%Y-%%m-%%dT', timestamp) || printf('%%02d', (CAST(strftime('%%H', timestamp) AS INTEGER) / %d) * %d) || ':00:00Z'", bucketH, bucketH)
	tlSQL := fmt.Sprintf(`SELECT %s as label, COUNT(*) as count
		FROM packets_v WHERE observer_id = ? AND timestamp > ?
		GROUP BY label ORDER BY label`, bucketFmt)
	tlRows, _ := s.db.conn.Query(tlSQL, id, since)
	timeline := make([]map[string]interface{}, 0)
	if tlRows != nil {
		defer tlRows.Close()
		for tlRows.Next() {
			var label string
			var count int
			tlRows.Scan(&label, &count)
			timeline = append(timeline, map[string]interface{}{"label": label, "count": count})
		}
	}

	// Nodes timeline — unique nodes per time bucket
	ntSQL := fmt.Sprintf(`SELECT %s as label, COUNT(DISTINCT hash) as count
		FROM packets_v WHERE observer_id = ? AND timestamp > ?
		GROUP BY label ORDER BY label`, bucketFmt)
	ntRows, _ := s.db.conn.Query(ntSQL, id, since)
	nodesTimeline := make([]map[string]interface{}, 0)
	if ntRows != nil {
		defer ntRows.Close()
		for ntRows.Next() {
			var label string
			var count int
			ntRows.Scan(&label, &count)
			nodesTimeline = append(nodesTimeline, map[string]interface{}{"label": label, "count": count})
		}
	}

	// SNR distribution
	snrSQL := `SELECT
		CAST(snr / 2 AS INTEGER) * 2 as rangeStart,
		COUNT(*) as count
		FROM packets_v WHERE observer_id = ? AND timestamp > ? AND snr IS NOT NULL
		GROUP BY rangeStart ORDER BY rangeStart`
	snrRows, _ := s.db.conn.Query(snrSQL, id, since)
	snrDistribution := make([]map[string]interface{}, 0)
	if snrRows != nil {
		defer snrRows.Close()
		for snrRows.Next() {
			var rangeStart, count int
			snrRows.Scan(&rangeStart, &count)
			snrDistribution = append(snrDistribution, map[string]interface{}{
				"range": fmt.Sprintf("%d to %d", rangeStart, rangeStart+2),
				"count": count,
			})
		}
	}

	// Packet type breakdown
	ptSQL := `SELECT payload_type, COUNT(*) as count FROM packets_v WHERE observer_id = ? AND timestamp > ? GROUP BY payload_type`
	ptRows, _ := s.db.conn.Query(ptSQL, id, since)
	packetTypes := map[string]interface{}{}
	if ptRows != nil {
		defer ptRows.Close()
		for ptRows.Next() {
			var pt, count int
			ptRows.Scan(&pt, &count)
			packetTypes[strconv.Itoa(pt)] = count
		}
	}

	// Recent packets
	rpSQL := `SELECT id, raw_hex, timestamp, observer_id, observer_name, direction, snr, rssi, score, hash, route_type, payload_type, payload_version, path_json, decoded_json, created_at
		FROM packets_v WHERE observer_id = ? AND timestamp > ? ORDER BY timestamp DESC LIMIT 20`
	rpRows, _ := s.db.conn.Query(rpSQL, id, since)
	recentPackets := make([]map[string]interface{}, 0)
	if rpRows != nil {
		defer rpRows.Close()
		for rpRows.Next() {
			p := scanPacketRow(rpRows)
			if p != nil {
				recentPackets = append(recentPackets, p)
			}
		}
	}

	writeJSON(w, map[string]interface{}{
		"timeline":        timeline,
		"packetTypes":     packetTypes,
		"nodesTimeline":   nodesTimeline,
		"snrDistribution": snrDistribution,
		"recentPackets":   recentPackets,
	})
}

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	hash := mux.Vars(r)["hash"]
	traces, err := s.db.GetTraces(hash)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"traces": traces})
}

var iataCoords = map[string]map[string]interface{}{
	"SJC": {"lat": 37.3626, "lon": -121.929},
	"SFO": {"lat": 37.6213, "lon": -122.379},
	"OAK": {"lat": 37.7213, "lon": -122.2208},
	"SEA": {"lat": 47.4502, "lon": -122.3088},
	"PDX": {"lat": 45.5898, "lon": -122.5951},
	"LAX": {"lat": 33.9425, "lon": -118.4081},
	"SAN": {"lat": 32.7338, "lon": -117.1933},
	"SMF": {"lat": 38.6954, "lon": -121.5908},
	"MRY": {"lat": 36.587, "lon": -121.843},
	"EUG": {"lat": 44.1246, "lon": -123.2119},
	"RDD": {"lat": 40.509, "lon": -122.2934},
	"MFR": {"lat": 42.3742, "lon": -122.8735},
	"FAT": {"lat": 36.7762, "lon": -119.7181},
	"SBA": {"lat": 34.4262, "lon": -119.8405},
	"RNO": {"lat": 39.4991, "lon": -119.7681},
	"BOI": {"lat": 43.5644, "lon": -116.2228},
	"LAS": {"lat": 36.084, "lon": -115.1537},
	"PHX": {"lat": 33.4373, "lon": -112.0078},
	"SLC": {"lat": 40.7884, "lon": -111.9778},
	"DEN": {"lat": 39.8561, "lon": -104.6737},
	"DFW": {"lat": 32.8998, "lon": -97.0403},
	"IAH": {"lat": 29.9844, "lon": -95.3414},
	"AUS": {"lat": 30.1975, "lon": -97.6664},
	"MSP": {"lat": 44.8848, "lon": -93.2223},
	"ATL": {"lat": 33.6407, "lon": -84.4277},
	"ORD": {"lat": 41.9742, "lon": -87.9073},
	"JFK": {"lat": 40.6413, "lon": -73.7781},
	"EWR": {"lat": 40.6895, "lon": -74.1745},
	"BOS": {"lat": 42.3656, "lon": -71.0096},
	"MIA": {"lat": 25.7959, "lon": -80.287},
	"IAD": {"lat": 38.9531, "lon": -77.4565},
	"CLT": {"lat": 35.2144, "lon": -80.9473},
	"DTW": {"lat": 42.2124, "lon": -83.3534},
	"MCO": {"lat": 28.4312, "lon": -81.3081},
	"BNA": {"lat": 36.1263, "lon": -86.6774},
	"RDU": {"lat": 35.8801, "lon": -78.788},
	"YVR": {"lat": 49.1967, "lon": -123.1815},
	"YYZ": {"lat": 43.6777, "lon": -79.6248},
	"YYC": {"lat": 51.1215, "lon": -114.0076},
	"YEG": {"lat": 53.3097, "lon": -113.58},
	"YOW": {"lat": 45.3225, "lon": -75.6692},
	"LHR": {"lat": 51.47, "lon": -0.4543},
	"CDG": {"lat": 49.0097, "lon": 2.5479},
	"FRA": {"lat": 50.0379, "lon": 8.5622},
	"AMS": {"lat": 52.3105, "lon": 4.7683},
	"MUC": {"lat": 48.3537, "lon": 11.775},
	"SOF": {"lat": 42.6952, "lon": 23.4062},
	"NRT": {"lat": 35.772, "lon": 140.3929},
	"HND": {"lat": 35.5494, "lon": 139.7798},
	"ICN": {"lat": 37.4602, "lon": 126.4407},
	"SYD": {"lat": -33.9461, "lon": 151.1772},
	"MEL": {"lat": -37.669, "lon": 144.841},
}

func (s *Server) handleIATACoords(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"coords": iataCoords})
}

func (s *Server) handleAudioLabBuckets(w http.ResponseWriter, r *http.Request) {
	// Query representative packets by type
	ptSQL := `SELECT payload_type, id, raw_hex, hash, decoded_json, path_json, observer_id, timestamp
		FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY payload_type ORDER BY length(raw_hex)) as rn
			FROM packets_v WHERE raw_hex IS NOT NULL
		) sub WHERE rn <= 8`
	rows, err := s.db.conn.Query(ptSQL)
	if err != nil {
		writeJSON(w, map[string]interface{}{"buckets": map[string]interface{}{}})
		return
	}
	defer rows.Close()

	ptNames := map[int]string{0: "REQ", 1: "RESPONSE", 2: "TXT_MSG", 3: "ACK", 4: "ADVERT", 5: "GRP_TXT", 7: "ANON_REQ", 8: "PATH", 9: "TRACE", 11: "CONTROL"}
	buckets := map[string][]map[string]interface{}{}
	for rows.Next() {
		var pt, id int
		var rawHex, hash, decodedJSON, pathJSON, obsID, ts sql.NullString
		rows.Scan(&pt, &id, &rawHex, &hash, &decodedJSON, &pathJSON, &obsID, &ts)
		typeName := ptNames[pt]
		if typeName == "" {
			typeName = "UNKNOWN"
		}
		if _, ok := buckets[typeName]; !ok {
			buckets[typeName] = make([]map[string]interface{}, 0)
		}
		buckets[typeName] = append(buckets[typeName], map[string]interface{}{
			"hash": nullStr(hash), "raw_hex": nullStr(rawHex),
			"decoded_json": nullStr(decodedJSON), "observation_count": 1,
			"payload_type": pt, "path_json": nullStr(pathJSON),
			"observer_id": nullStr(obsID), "timestamp": nullStr(ts),
		})
	}
	writeJSON(w, map[string]interface{}{"buckets": buckets})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[routes] JSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func mergeMap(base map[string]interface{}, overlays ...map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range base {
		result[k] = v
	}
	for _, o := range overlays {
		if o == nil {
			continue
		}
		for k, v := range o {
			result[k] = v
		}
	}
	return result
}

func safeAvg(total, count float64) float64 {
	if count == 0 {
		return 0
	}
	return round(total/count, 1)
}

func round(val float64, places int) float64 {
	m := 1.0
	for i := 0; i < places; i++ {
		m *= 10
	}
	return float64(int(val*m+0.5)) / m
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func sortedCopy(arr []float64) []float64 {
	cp := make([]float64, len(arr))
	copy(cp, arr)
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	return cp
}

func lastN(arr []map[string]interface{}, n int) []map[string]interface{} {
	if len(arr) <= n {
		return arr
	}
	return arr[len(arr)-n:]
}
