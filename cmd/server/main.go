package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
)

// Set via -ldflags at build time
var Version string
var Commit string
var BuildTime string

func resolveCommit() string {
	if Commit != "" {
		return Commit
	}
	// Try .git-commit file (baked by Docker / CI)
	if data, err := os.ReadFile(".git-commit"); err == nil {
		if c := strings.TrimSpace(string(data)); c != "" && c != "unknown" {
			return c
		}
	}
	// Try git rev-parse at runtime
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}

func resolveVersion() string {
	if Version != "" {
		return Version
	}
	return "unknown"
}

func resolveBuildTime() string {
	if BuildTime != "" {
		return BuildTime
	}
	return "unknown"
}

func main() {
	// pprof profiling — off by default, enable with ENABLE_PPROF=true
	if os.Getenv("ENABLE_PPROF") == "true" {
		pprofPort := os.Getenv("PPROF_PORT")
		if pprofPort == "" {
			pprofPort = "6060"
		}
		// Bind to loopback only — the pprof endpoints expose heap/goroutine
		// dumps and must never be reachable from outside the host. Operators
		// who need remote access should tunnel (e.g. SSH port-forward).
		pprofAddr := "127.0.0.1:" + pprofPort
		go func() {
			log.Printf("[pprof] profiling UI at http://%s/debug/pprof/", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("[pprof] failed to start: %v (non-fatal)", err)
			}
		}()
	}

	var (
		configDir  string
		port       int
		dbPath     string
		publicDir  string
		pollMs     int
	)

	flag.StringVar(&configDir, "config-dir", ".", "Directory containing config.json")
	flag.IntVar(&port, "port", 0, "HTTP port (overrides config)")
	flag.StringVar(&dbPath, "db", "", "SQLite database path (overrides config/env)")
	flag.StringVar(&publicDir, "public", "public", "Directory to serve static files from")
	flag.IntVar(&pollMs, "poll-ms", 1000, "SQLite poll interval for WebSocket broadcast (ms)")
	flag.Parse()

	// Load config
	cfg, err := LoadConfig(configDir)
	if err != nil {
		log.Printf("[config] warning: %v (using defaults)", err)
	}

	// CLI flags override config
	if port > 0 {
		cfg.Port = port
	}
	if cfg.Port == 0 {
		cfg.Port = 3000
	}
	if dbPath != "" {
		cfg.DBPath = dbPath
	}
	if cfg.APIKey == "" {
		log.Printf("[security] WARNING: no apiKey configured — write endpoints are BLOCKED (set apiKey in config.json to enable them)")
	} else if IsWeakAPIKey(cfg.APIKey) {
		log.Printf("[security] WARNING: API key is weak or a known default — write endpoints are vulnerable")
	}

	// Load the active branding template (templates/<name>/template.json).
	tmpl := LoadTemplate(cfg.Template, configDir, ".")
	log.Printf("[template] active template: %s", tmpl.Name)

	// Apply Go runtime soft memory limit (#836).
	// Honors GOMEMLIMIT if set; otherwise derives from packetStore.maxMemoryMB.
	{
		_, envSet := os.LookupEnv("GOMEMLIMIT")
		maxMB := 0
		if cfg.PacketStore != nil {
			maxMB = cfg.PacketStore.MaxMemoryMB
		}
		limit, source := applyMemoryLimit(maxMB, envSet)
		switch source {
		case "env":
			log.Printf("[memlimit] using GOMEMLIMIT from environment (%s)", os.Getenv("GOMEMLIMIT"))
		case "derived":
			log.Printf("[memlimit] derived from packetStore.maxMemoryMB=%d → %d MiB (1.5x headroom)", maxMB, limit/(1024*1024))
		default:
			log.Printf("[memlimit] no soft memory limit set (GOMEMLIMIT unset, packetStore.maxMemoryMB=0); recommend setting one to avoid container OOM-kill")
		}
	}

	// Resolve DB path
	resolvedDB := cfg.ResolveDBPath(configDir)
	log.Printf("[config] port=%d db=%s public=%s", cfg.Port, resolvedDB, publicDir)
	if len(cfg.NodeBlacklist) > 0 {
		log.Printf("[config] nodeBlacklist: %d node(s) will be hidden from API", len(cfg.NodeBlacklist))
		for _, pk := range cfg.NodeBlacklist {
			if trimmed := strings.ToLower(strings.TrimSpace(pk)); trimmed != "" {
				log.Printf("[config]   blacklisted: %s", trimmed)
			}
		}
	}

	// Open database
	database, err := OpenDB(resolvedDB)
	if err != nil {
		log.Fatalf("[db] failed to open %s: %v", resolvedDB, err)
	}
	var dbCloseOnce sync.Once
	dbClose := func() error {
		var err error
		dbCloseOnce.Do(func() { err = database.Close() })
		return err
	}
	defer dbClose()

	// Verify DB has expected tables
	var tableName string
	err = database.conn.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='transmissions'").Scan(&tableName)
	if err == sql.ErrNoRows {
		log.Fatalf("[db] table 'transmissions' not found — is this a CoreScope database?")
	}

	stats, err := database.GetStats()
	if err != nil {
		log.Printf("[db] warning: could not read stats: %v", err)
	} else {
		log.Printf("[db] transmissions=%d observations=%d nodes=%d observers=%d",
			stats.TotalTransmissions, stats.TotalObservations, stats.TotalNodes, stats.TotalObservers)
	}

	// Check auto_vacuum mode and optionally migrate (#919)
	checkAutoVacuum(database, cfg, resolvedDB)

	// Ensure indexes the server's SQL fallback path depends on
	// (mirrors ingestor schema for DBs created by old server-only builds).
	if err := ensureServerIndexes(resolvedDB); err != nil {
		log.Printf("[db] warning: could not ensure server indexes: %v", err)
	}

	// In-memory packet store
	store := NewPacketStore(database, cfg.PacketStore, cfg.CacheTTL)
	if err := store.Load(); err != nil {
		log.Fatalf("[store] failed to load: %v", err)
	}
	if store.hotStartupHours > 0 {
		log.Printf("[store] starting background load: filling retentionHours=%gh from hotStartupHours=%gh",
			store.retentionHours, store.hotStartupHours)
		go store.loadBackgroundChunks()
	}

	// Initialize persisted neighbor graph
	dbPath = database.path
	if err := ensureNeighborEdgesTable(dbPath); err != nil {
		log.Printf("[neighbor] warning: could not create neighbor_edges table: %v", err)
	}
	// Add resolved_path column if missing.
	// NOTE on startup ordering (review item #10): ensureResolvedPathColumn runs AFTER
	// OpenDB/detectSchema, so db.hasResolvedPath will be false on first run with a
	// pre-existing DB. This means Load() won't SELECT resolved_path from SQLite.
	// Async backfill runs after HTTP starts (see backfillResolvedPathsAsync below)
	// AND to SQLite. On next restart, detectSchema finds the column and Load() reads it.
	if err := ensureResolvedPathColumn(dbPath); err != nil {
		log.Printf("[store] warning: could not add resolved_path column: %v", err)
	} else {
		database.hasResolvedPath = true // detectSchema ran before column was added; fix the flag
	}

	// Ensure observers.inactive column exists (PR #954 filters on it; ingestor migration
	// adds it but server may run against DBs ingestor never touched, e.g. e2e fixture).
	if err := ensureObserverInactiveColumn(dbPath); err != nil {
		log.Printf("[store] warning: could not add observers.inactive column: %v", err)
	}

	// Ensure observers.last_packet_at column exists (PR #905 reads it; ingestor migration
	// adds it but server may run against DBs ingestor never touched, e.g. e2e fixture).
	if err := ensureLastPacketAtColumn(dbPath); err != nil {
		log.Printf("[store] warning: could not add observers.last_packet_at column: %v", err)
	}

	// Ensure nodes.foreign_advert column exists (#730 reads it on every /api/nodes
	// scan; ingestor migration foreign_advert_v1 adds it but server may run against
	// DBs ingestor never touched, e.g. e2e fixture).
	if err := ensureForeignAdvertColumn(dbPath); err != nil {
		log.Printf("[store] warning: could not add nodes.foreign_advert column: %v", err)
	}

	// Ensure transmissions.from_pubkey column + index exists (#1143). Backfill
	// for legacy NULL rows runs async after HTTP starts so it can't block boot
	// even on prod-sized DBs (100K+ transmissions).
	if err := ensureFromPubkeyColumn(dbPath); err != nil {
		log.Printf("[store] warning: could not add transmissions.from_pubkey column: %v", err)
	}

	// Soft-delete observers that are in the blacklist (mark inactive=1) so
	// historical data from a prior unblocked window is hidden too.
	if len(cfg.ObserverBlacklist) > 0 {
		softDeleteBlacklistedObservers(dbPath, cfg.ObserverBlacklist)
	}

	// WaitGroup for background init steps that gate /api/healthz readiness.
	var initWg sync.WaitGroup

	// Load or build neighbor graph
	if neighborEdgesTableExists(database.conn) {
		store.graph.Store(loadNeighborEdgesFromDB(database.conn))
		log.Printf("[neighbor] loaded persisted neighbor graph")
	} else {
		log.Printf("[neighbor] no persisted edges found, will build in background...")
		store.graph.Store(NewNeighborGraph()) // empty graph — gets populated by background goroutine
		initWg.Add(1)
		go func() {
			defer initWg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[neighbor] graph build panic recovered: %v", r)
				}
			}()
			rw, rwErr := cachedRW(dbPath)
			if rwErr == nil {
				edgeCount := buildAndPersistEdges(store, rw)
				log.Printf("[neighbor] persisted %d edges", edgeCount)
			}
			built := BuildFromStore(store)
			store.graph.Store(built)
			log.Printf("[neighbor] graph build complete")
		}()
	}

	// Initial pickBestObservation runs in background — doesn't need to block HTTP.
	// API serves best-effort data until this completes (~10s for 100K txs).
	// Processes in chunks of 5000, releasing the lock between chunks so API
	// handlers remain responsive.
	initWg.Add(1)
	go func() {
		defer initWg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[store] pickBestObservation panic recovered: %v", r)
			}
		}()
		const chunkSize = 5000
		store.mu.RLock()
		totalPackets := len(store.packets)
		store.mu.RUnlock()

		for i := 0; i < totalPackets; i += chunkSize {
			end := i + chunkSize
			if end > totalPackets {
				end = totalPackets
			}
			store.mu.Lock()
			for j := i; j < end && j < len(store.packets); j++ {
				pickBestObservation(store.packets[j])
			}
			store.mu.Unlock()
			if end < totalPackets {
				time.Sleep(10 * time.Millisecond) // yield to API handlers
			}
		}
		log.Printf("[store] initial pickBestObservation complete (%d transmissions)", totalPackets)
	}()

	// Mark server ready once all background init completes.
	go func() {
		initWg.Wait()
		readiness.Store(1)
		log.Printf("[server] readiness: ready=true (background init complete)")
	}()

	// WebSocket hub
	hub := NewHub()
	// Validate WebSocket upgrade Origin against the same allowlist used for
	// CORS — prevents cross-site WebSocket hijacking (same-origin still works).
	hub.allowedOrigins = cfg.CORSAllowedOrigins

	// HTTP server
	srv := NewServer(database, cfg, hub)
	srv.tmpl = tmpl
	srv.store = store
	srv.channelKeys = loadServerChannelKeys(cfg, configDir)

	// API response cache. TTL from CORESCOPE_API_CACHE_TTL (seconds);
	// 0 disables caching. Default 10s.
	apiCacheTTL := 10 * time.Second
	if v := os.Getenv("CORESCOPE_API_CACHE_TTL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			apiCacheTTL = time.Duration(n) * time.Second
		} else {
			log.Printf("[cache] invalid CORESCOPE_API_CACHE_TTL %q, using default %v", v, apiCacheTTL)
		}
	}
	if apiCacheTTL > 0 {
		srv.respCache = newResponseCache(apiCacheTTL)
		log.Printf("[cache] API response cache enabled, TTL %v", apiCacheTTL)
	} else {
		log.Printf("[cache] API response cache disabled")
	}

	router := mux.NewRouter()
	srv.RegisterRoutes(router)

	// In-app cookie session gate (replaces external Traefik basic-auth).
	// Disabled when CORESCOPE_WEB_USER/CORESCOPE_WEB_PASSWORD are unset —
	// a true pass-through no-op in that case.
	webAuth := newWebAuth(os.Getenv("CORESCOPE_WEB_USER"), os.Getenv("CORESCOPE_WEB_PASSWORD"))
	webAuth.logStartup()
	webAuth.registerWebAuthRoutes(router)
	router.Use(webAuth.middleware)

	// WebSocket endpoint
	router.HandleFunc("/ws", hub.ServeWS)

	// Template assets (must be registered before the catch-all static handler)
	srv.registerTemplateAssets(router)

	// Static files + SPA fallback
	absPublic, _ := filepath.Abs(publicDir)
	if _, err := os.Stat(absPublic); err == nil {
		fs := http.FileServer(http.Dir(absPublic))
		router.PathPrefix("/").Handler(wsOrStatic(hub, srv.spaHandler(absPublic, fs)))
		log.Printf("[static] serving %s", absPublic)
	} else {
		log.Printf("[static] directory %s not found — API-only mode", absPublic)
		router.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<!DOCTYPE html><html><body><h1>CoreScope</h1><p>Frontend not found. API available at /api/</p></body></html>`))
		})
	}

	// Start SQLite poller for WebSocket broadcast
	poller := NewPoller(database, hub, time.Duration(pollMs)*time.Millisecond)
	poller.store = store
	go poller.Start()

	// Start periodic eviction
	stopEviction := store.StartEvictionTicker()
	defer stopEviction()

	// Start perf-history collector — snapshots perf metrics into an
	// in-memory ring buffer once a minute for /api/perf/history.
	stopPerfHistory := srv.startPerfHistoryCollector()
	defer stopPerfHistory()

	// Auto-prune old packets if retention.packetDays is configured
	vacuumPages := cfg.IncrementalVacuumPages()
	var stopPrune func()
	if cfg.Retention != nil && cfg.Retention.PacketDays > 0 {
		days := cfg.Retention.PacketDays
		pruneTicker := time.NewTicker(24 * time.Hour)
		pruneDone := make(chan struct{})
		stopPrune = func() {
			pruneTicker.Stop()
			close(pruneDone)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[prune] panic recovered: %v", r)
				}
			}()
			time.Sleep(1 * time.Minute)
			if n, err := database.PruneOldPackets(days); err != nil {
				log.Printf("[prune] error: %v", err)
			} else {
				log.Printf("[prune] deleted %d transmissions older than %d days", n, days)
				if n > 0 {
					runIncrementalVacuum(resolvedDB, vacuumPages)
				}
			}
			for {
				select {
				case <-pruneTicker.C:
					if n, err := database.PruneOldPackets(days); err != nil {
						log.Printf("[prune] error: %v", err)
					} else {
						log.Printf("[prune] deleted %d transmissions older than %d days", n, days)
						if n > 0 {
							runIncrementalVacuum(resolvedDB, vacuumPages)
						}
					}
				case <-pruneDone:
					return
				}
			}
		}()
		log.Printf("[prune] auto-prune enabled: packets older than %d days will be removed daily", days)
	}

	// Auto-prune old metrics
	var stopMetricsPrune func()
	{
		metricsDays := cfg.MetricsRetentionDays()
		metricsPruneTicker := time.NewTicker(24 * time.Hour)
		metricsPruneDone := make(chan struct{})
		stopMetricsPrune = func() {
			metricsPruneTicker.Stop()
			close(metricsPruneDone)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[metrics-prune] panic recovered: %v", r)
				}
			}()
			time.Sleep(2 * time.Minute) // stagger after packet prune
			database.PruneOldMetrics(metricsDays)
			runIncrementalVacuum(resolvedDB, vacuumPages)
			for {
				select {
				case <-metricsPruneTicker.C:
					database.PruneOldMetrics(metricsDays)
					runIncrementalVacuum(resolvedDB, vacuumPages)
				case <-metricsPruneDone:
					return
				}
			}
		}()
		log.Printf("[metrics-prune] auto-prune enabled: metrics older than %d days", metricsDays)
	}

	// Auto-prune stale observers
	var stopObserverPrune func()
	{
		observerDays := cfg.ObserverDaysOrDefault()
		if observerDays <= -1 {
			// -1 means keep forever, skip
		} else {
			observerPruneTicker := time.NewTicker(24 * time.Hour)
			observerPruneDone := make(chan struct{})
			stopObserverPrune = func() {
				observerPruneTicker.Stop()
				close(observerPruneDone)
			}
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[observer-prune] panic recovered: %v", r)
					}
				}()
				time.Sleep(3 * time.Minute) // stagger after metrics prune
				database.RemoveStaleObservers(observerDays)
				runIncrementalVacuum(resolvedDB, vacuumPages)
				for {
					select {
					case <-observerPruneTicker.C:
						database.RemoveStaleObservers(observerDays)
						runIncrementalVacuum(resolvedDB, vacuumPages)
					case <-observerPruneDone:
						return
					}
				}
			}()
			log.Printf("[observer-prune] auto-prune enabled: observers not seen in %d days will be removed", observerDays)
		}
	}

	// Auto-prune old neighbor edges
	var stopEdgePrune func()
	{
		maxAgeDays := cfg.NeighborMaxAgeDays()
		edgePruneTicker := time.NewTicker(24 * time.Hour)
		edgePruneDone := make(chan struct{})
		stopEdgePrune = func() {
			edgePruneTicker.Stop()
			close(edgePruneDone)
		}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[neighbor-prune] panic recovered: %v", r)
				}
			}()
			time.Sleep(4 * time.Minute) // stagger after metrics prune
			g := store.graph.Load()
			PruneNeighborEdges(dbPath, g, maxAgeDays)
			runIncrementalVacuum(resolvedDB, vacuumPages)
			for {
				select {
				case <-edgePruneTicker.C:
					g := store.graph.Load()
					PruneNeighborEdges(dbPath, g, maxAgeDays)
					runIncrementalVacuum(resolvedDB, vacuumPages)
				case <-edgePruneDone:
					return
				}
			}
		}()
		log.Printf("[neighbor-prune] auto-prune enabled: edges older than %d days", maxAgeDays)
	}

	// Graceful shutdown
	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("[server] received %v, shutting down...", sig)

		// 1. Stop accepting new WebSocket/poll data
		poller.Stop()

		// 1b. Stop auto-prune ticker
		if stopPrune != nil {
			stopPrune()
		}
		if stopMetricsPrune != nil {
			stopMetricsPrune()
		}
		if stopObserverPrune != nil {
			stopObserverPrune()
		}
		if stopEdgePrune != nil {
			stopEdgePrune()
		}

		// 2. Gracefully drain HTTP connections (up to 15s)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("[server] HTTP shutdown error: %v", err)
		}

		// 3. Close WebSocket hub
		hub.Close()

		// 4. Close database (release SQLite WAL lock)
		if err := dbClose(); err != nil {
			log.Printf("[server] DB close error: %v", err)
		}
		log.Println("[server] shutdown complete")
	}()

	log.Printf("[server] CoreScope (Go) listening on http://localhost:%d", cfg.Port)

	// Start async backfill in background — HTTP is now available.
	go backfillResolvedPathsAsync(store, dbPath, 5000, 100*time.Millisecond, cfg.BackfillHours())
	// #1143: backfill from_pubkey for legacy ADVERT rows. Async so even
	// 100K+ rows can't block boot; queries handle NULL gracefully.
	// startFromPubkeyBackfill wraps the goroutine dispatch so the async
	// contract is testable (see TestBackfillFromPubkey_DoesNotBlockBoot).
	startFromPubkeyBackfill(dbPath, 5000, 100*time.Millisecond)

	// Migrate old content hashes in background (one-time, idempotent).
	go migrateContentHashesAsync(store, 5000, 100*time.Millisecond)

	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("[server] %v", err)
	}
}

// themeCSSMap mirrors THEME_CSS_MAP in public/customize-v2.js — a theme config
// key → CSS custom property. Used to inline the server theme into index.html so
// the page paints correct colors before the JS theme pipeline runs.
var themeCSSMap = [][2]string{
	{"accent", "--accent"}, {"accentHover", "--accent-hover"},
	{"navBg", "--nav-bg"}, {"navBg2", "--nav-bg2"}, {"navText", "--nav-text"}, {"navTextMuted", "--nav-text-muted"},
	{"background", "--surface-0"}, {"text", "--text"}, {"textMuted", "--text-muted"}, {"border", "--border"},
	{"statusGreen", "--status-green"}, {"statusYellow", "--status-yellow"}, {"statusRed", "--status-red"},
	{"surface1", "--surface-1"}, {"surface2", "--surface-2"}, {"surface3", "--surface-3"},
	{"sectionBg", "--section-bg"},
	{"cardBg", "--card-bg"}, {"contentBg", "--content-bg"}, {"detailBg", "--detail-bg"},
	{"inputBg", "--input-bg"}, {"rowStripe", "--row-stripe"}, {"rowHover", "--row-hover"}, {"selectedBg", "--selected-bg"},
	{"font", "--font"}, {"mono", "--mono"},
}

// themeVarsBlock emits "--var:value;" declarations for a theme map, skipping
// values containing characters that could break out of the <style> context.
func themeVarsBlock(theme map[string]interface{}) string {
	var b strings.Builder
	for _, kv := range themeCSSMap {
		raw, ok := theme[kv[0]]
		if !ok {
			continue
		}
		val, ok := raw.(string)
		if !ok || val == "" || strings.ContainsAny(val, "<>{};\n\r") {
			continue
		}
		b.WriteString(kv[1])
		b.WriteByte(':')
		b.WriteString(val)
		b.WriteByte(';')
	}
	return b.String()
}

// buildThemeStyleTag renders an inline <style> block setting the server theme's
// CSS variables for light, manual-dark, and OS-dark scopes — matching the
// cascade in style.css and isDarkMode() in customize-v2.js. This eliminates the
// color flash that occurred while app.js fetched /api/config/theme.
func buildThemeStyleTag(tr ThemeResponse) string {
	light := themeVarsBlock(tr.Theme)
	darkMerged := map[string]interface{}{}
	for k, v := range tr.Theme {
		darkMerged[k] = v
	}
	for k, v := range tr.ThemeDark {
		darkMerged[k] = v
	}
	dark := themeVarsBlock(darkMerged)
	if light == "" && dark == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<style id="server-theme">`)
	if light != "" {
		b.WriteString(":root{" + light + "}")
	}
	if dark != "" {
		b.WriteString(`[data-theme="dark"]{` + dark + "}")
		b.WriteString(`@media (prefers-color-scheme:dark){:root:not([data-theme="light"]):not([data-theme="dark"]){` + dark + "}}")
	}
	b.WriteString("</style>")
	return b.String()
}

// metaStr safely reads a string value from a meta map, returning fallback when
// absent or when the value contains characters that could break out of an HTML
// attribute or tag context.
func metaStr(m map[string]interface{}, key, fallback string) string {
	raw, ok := m[key]
	if !ok {
		return fallback
	}
	s, ok := raw.(string)
	if !ok || s == "" || strings.ContainsAny(s, "<>\"\n\r") {
		return fallback
	}
	return s
}

// buildSiteMetaTag renders the <title> and OpenGraph/Twitter meta tags from the
// active template/config meta map. Replaces the static __SITE_META__ placeholder
// in index.html so social crawlers see template-specific values.
func buildSiteMetaTag(tr ThemeResponse) string {
	m := tr.Meta
	if m == nil {
		m = map[string]interface{}{}
	}
	title := metaStr(m, "title", "CoreScope-EVO")
	desc := metaStr(m, "description", "Real-time MeshCore LoRa mesh network analyzer")
	ogImage := metaStr(m, "ogImage", "")
	ogURL := metaStr(m, "ogUrl", "")
	themeColor := metaStr(m, "themeColor", "#0a0a0a")
	var b strings.Builder
	b.WriteString("<title>" + title + "</title>")
	b.WriteString(`<meta name="description" content="` + desc + `">`)
	b.WriteString(`<meta property="og:title" content="` + title + `">`)
	b.WriteString(`<meta property="og:description" content="` + desc + `">`)
	if ogImage != "" {
		b.WriteString(`<meta property="og:image" content="` + ogImage + `">`)
		b.WriteString(`<meta name="twitter:image" content="` + ogImage + `">`)
	}
	if ogURL != "" {
		b.WriteString(`<meta property="og:url" content="` + ogURL + `">`)
	}
	b.WriteString(`<meta property="og:type" content="website">`)
	b.WriteString(`<meta name="twitter:card" content="summary_large_image">`)
	b.WriteString(`<meta name="twitter:title" content="` + title + `">`)
	b.WriteString(`<meta name="twitter:description" content="` + desc + `">`)
	b.WriteString(`<meta name="theme-color" content="` + themeColor + `">`)
	return b.String()
}

// spaHandler serves static files, falling back to index.html for SPA routes.
// It reads index.html once at creation time and replaces the __BUST__ placeholder
// with a Unix timestamp so browsers fetch fresh JS/CSS after each server restart,
// and the __THEME_STYLE__ placeholder with the inlined server theme.
func (s *Server) spaHandler(root string, fs http.Handler) http.Handler {
	// Pre-process index.html: replace __BUST__ with a cache-bust timestamp
	indexPath := filepath.Join(root, "index.html")
	rawHTML, err := os.ReadFile(indexPath)
	if err != nil {
		log.Printf("[static] warning: could not read index.html for cache-bust: %v", err)
		rawHTML = []byte("<!DOCTYPE html><html><body><h1>CoreScope</h1><p>index.html not found</p></body></html>")
	}
	bustValue := fmt.Sprintf("%d", time.Now().Unix())
	processed := strings.ReplaceAll(string(rawHTML), "__BUST__", bustValue)
	processed = strings.ReplaceAll(processed, "__THEME_STYLE__", buildThemeStyleTag(s.buildThemeResponse()))
	processed = strings.ReplaceAll(processed, "__SITE_META__", buildSiteMetaTag(s.buildThemeResponse()))
	indexHTML := []byte(processed)
	log.Printf("[static] cache-bust value: %s", bustValue)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve pre-processed index.html for root and /index.html
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Write(indexHTML)
			return
		}

		path := filepath.Join(root, r.URL.Path)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// SPA fallback — serve pre-processed index.html
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Write(indexHTML)
			return
		}
		// Disable caching for JS/CSS/HTML
		if filepath.Ext(path) == ".js" || filepath.Ext(path) == ".css" || filepath.Ext(path) == ".html" {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		fs.ServeHTTP(w, r)
	})
}
