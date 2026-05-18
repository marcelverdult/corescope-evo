package main

import (
	"net/http"
	"runtime"
	"time"
)

// perfHistoryCap bounds the in-memory perf-history ring buffer. The dashboard
// long-history view holds 48 h at 1-minute resolution (2880 samples); the
// server-side buffer matches so a fresh page load can backfill the full
// window. Oldest samples are dropped once the cap is hit.
const perfHistoryCap = 2880

// perfHistoryInterval is how often the background goroutine snapshots metrics
// into the ring buffer — 1 sample per minute, matching the dashboard's
// long-history resolution.
const perfHistoryInterval = 60 * time.Second

// collectPerfSample gathers a flat snapshot of the metrics the Performance
// dashboard charts. It reuses the same sources handlePerf reads: cached
// MemStats, the packet store, SQLite size stats, the observer health query,
// the WebSocket hub, and the process CPU sampler.
func (s *Server) collectPerfSample() PerfSample {
	ms := s.getMemStats()

	sample := PerfSample{
		Ts:          time.Now().UnixMilli(),
		CpuPercent:  getCPUPercent(),
		Goroutines:  runtime.NumGoroutine(),
		LastPauseMs: float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6,
		HeapAllocMB: float64(ms.HeapAlloc) / 1024 / 1024,
		HeapInuseMB: float64(ms.HeapInuse) / 1024 / 1024,
		HeapSysMB:   float64(ms.HeapSys) / 1024 / 1024,
		TotalSysMB:  float64(ms.Sys) / 1024 / 1024,
	}

	if s.perfStats != nil {
		s.perfStats.mu.Lock()
		sample.AvgMs = safeAvg(s.perfStats.TotalMs, float64(s.perfStats.Requests))
		s.perfStats.mu.Unlock()
	}

	if s.store != nil {
		cs := s.store.GetCacheStatsTyped()
		sample.CacheHitRate = cs.HitRate
		ps := s.store.GetPerfStoreStatsTyped()
		sample.PacketsInRAM = ps.InMemory
		sample.TrackedMB = ps.TrackedMB
	}

	if s.db != nil {
		ss := s.db.GetDBSizeStatsTyped()
		sample.DbSizeMB = ss.DbSizeMB
		sample.WalSizeMB = ss.WalSizeMB
		if oc := s.db.GetObserverCounts(); oc != nil {
			sample.TotalObservers = oc.Total
			sample.OnlineObservers = oc.Online
		}
	}

	if s.hub != nil {
		sample.WsClients = s.hub.ClientCount()
	}

	return sample
}

// storePerfSample appends a sample to the ring buffer, dropping the oldest
// entry once perfHistoryCap is exceeded. Safe for concurrent use.
func (s *Server) storePerfSample(sample PerfSample) {
	s.perfHistoryMu.Lock()
	defer s.perfHistoryMu.Unlock()
	s.perfHistory = append(s.perfHistory, sample)
	if len(s.perfHistory) > perfHistoryCap {
		// Drop the oldest by re-slicing onto a fresh backing array so the
		// dropped samples can be garbage-collected (a plain [1:] would keep
		// the full backing array alive forever).
		trimmed := make([]PerfSample, perfHistoryCap)
		copy(trimmed, s.perfHistory[len(s.perfHistory)-perfHistoryCap:])
		s.perfHistory = trimmed
	}
}

// startPerfHistoryCollector launches the background goroutine that snapshots
// perf metrics into the ring buffer every perfHistoryInterval. It returns a
// stop function the caller invokes on shutdown. Mirrors the auto-prune
// goroutine pattern in main.go (ticker + done channel + panic recovery).
func (s *Server) startPerfHistoryCollector() func() {
	ticker := time.NewTicker(perfHistoryInterval)
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Diagnostics only — a panic here must not crash the server.
			}
		}()
		// Seed one sample immediately so /api/perf/history isn't empty for
		// the first minute after startup.
		s.storePerfSample(s.collectPerfSample())
		for {
			select {
			case <-ticker.C:
				s.storePerfSample(s.collectPerfSample())
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

// PerfHistoryResponse is the /api/perf/history payload — the full ring-buffer
// contents, oldest sample first.
type PerfHistoryResponse struct {
	Samples []PerfSample `json:"samples"`
}

// handlePerfHistory returns the in-memory perf-metrics ring buffer so the
// dashboard can backfill its charts on page load. Unauthenticated, like the
// other /api/perf* read endpoints.
func (s *Server) handlePerfHistory(w http.ResponseWriter, r *http.Request) {
	s.perfHistoryMu.Lock()
	samples := make([]PerfSample, len(s.perfHistory))
	copy(samples, s.perfHistory)
	s.perfHistoryMu.Unlock()
	writeJSON(w, PerfHistoryResponse{Samples: samples})
}
