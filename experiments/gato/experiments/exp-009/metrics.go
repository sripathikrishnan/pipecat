package main

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"
)

// MetricsSnapshot records system state at one point in time.
type MetricsSnapshot struct {
	Timestamp       time.Time
	Sessions        int
	Goroutines      int
	HeapAllocMB     float64
	GCPauseP99Ms    float64
	TurnAroundP50Ms float64
	TurnAroundP99Ms float64
	VADLatencyP99Ms float64
	InterruptCount  int
}

// Collector gathers metrics from all sessions periodically.
type Collector struct {
	mu        sync.Mutex
	sessions  []*SessionMetrics
	snapshots []MetricsSnapshot
}

// AddSession registers a session's metrics for collection.
func (c *Collector) AddSession(m *SessionMetrics) {
	c.mu.Lock()
	c.sessions = append(c.sessions, m)
	c.mu.Unlock()
}

// Collect gathers a snapshot from all registered sessions and appends it.
func (c *Collector) Collect(nSessions int) MetricsSnapshot {
	c.mu.Lock()
	sessions := make([]*SessionMetrics, len(c.sessions))
	copy(sessions, c.sessions)
	c.mu.Unlock()

	// Aggregate turnaround and VAD latencies across all sessions.
	var allTA, allVAD []float64
	totalInterrupts := 0

	for _, s := range sessions {
		ta, vi, ints := s.snapshot()
		allTA = append(allTA, ta...)
		allVAD = append(allVAD, vi...)
		totalInterrupts += ints
	}

	sort.Float64s(allTA)
	sort.Float64s(allVAD)

	// Runtime stats.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	gcPauseP99 := gcPauseP99Ms(&mem)
	heapMB := float64(mem.HeapAlloc) / (1024 * 1024)

	snap := MetricsSnapshot{
		Timestamp:       time.Now(),
		Sessions:        nSessions,
		Goroutines:      runtime.NumGoroutine(),
		HeapAllocMB:     heapMB,
		GCPauseP99Ms:    gcPauseP99,
		TurnAroundP50Ms: percentile(allTA, 50),
		TurnAroundP99Ms: percentile(allTA, 99),
		VADLatencyP99Ms: percentile(allVAD, 99),
		InterruptCount:  totalInterrupts,
	}

	c.mu.Lock()
	c.snapshots = append(c.snapshots, snap)
	c.mu.Unlock()

	return snap
}

// Snapshots returns a copy of all collected snapshots.
func (c *Collector) Snapshots() []MetricsSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]MetricsSnapshot, len(c.snapshots))
	copy(out, c.snapshots)
	return out
}

// PrintTable prints a formatted table of all snapshots to w.
func (c *Collector) PrintTable(w io.Writer) {
	c.mu.Lock()
	snaps := make([]MetricsSnapshot, len(c.snapshots))
	copy(snaps, c.snapshots)
	c.mu.Unlock()

	fmt.Fprintln(w, "Time                 Sessions  Goroutines  Heap(MB)  GCp99(ms)  TAp50(ms)  TAp99(ms)  VADp99(ms)  Interrupts")
	fmt.Fprintln(w, "-------------------  --------  ----------  --------  ---------  ---------  ---------  ----------  ----------")
	for _, s := range snaps {
		fmt.Fprintf(w, "%-19s  %8d  %10d  %8.1f  %9.2f  %9.2f  %9.2f  %10.2f  %10d\n",
			s.Timestamp.Format("2006-01-02 15:04:05"),
			s.Sessions,
			s.Goroutines,
			s.HeapAllocMB,
			s.GCPauseP99Ms,
			s.TurnAroundP50Ms,
			s.TurnAroundP99Ms,
			s.VADLatencyP99Ms,
			s.InterruptCount,
		)
	}
}

// PrintCSV writes snapshots in CSV format to w.
func (c *Collector) PrintCSV(w io.Writer) {
	c.mu.Lock()
	snaps := make([]MetricsSnapshot, len(c.snapshots))
	copy(snaps, c.snapshots)
	c.mu.Unlock()

	fmt.Fprintln(w, "timestamp,sessions,goroutines,heap_mb,gc_p99_ms,ta_p50_ms,ta_p99_ms,vad_p99_ms,interrupts")
	for _, s := range snaps {
		fmt.Fprintf(w, "%s,%d,%d,%.2f,%.3f,%.2f,%.2f,%.2f,%d\n",
			s.Timestamp.Format(time.RFC3339),
			s.Sessions,
			s.Goroutines,
			s.HeapAllocMB,
			s.GCPauseP99Ms,
			s.TurnAroundP50Ms,
			s.TurnAroundP99Ms,
			s.VADLatencyP99Ms,
			s.InterruptCount,
		)
	}
}

// percentile returns the p-th percentile from a sorted slice.
// Returns 0 if the slice is empty.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	// Nearest-rank method.
	rank := p / 100.0 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// gcPauseP99Ms extracts an approximation of GC pause P99 from runtime MemStats.
// runtime.MemStats.PauseNs is a circular buffer of the last 256 GC pause durations.
func gcPauseP99Ms(mem *runtime.MemStats) float64 {
	if mem.NumGC == 0 {
		return 0
	}
	n := int(mem.NumGC)
	if n > 256 {
		n = 256
	}
	pauses := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		idx := (int(mem.NumGC) - 1 - i + 256) % 256
		ns := mem.PauseNs[idx]
		if ns > 0 {
			pauses = append(pauses, float64(ns)/1e6)
		}
	}
	sort.Float64s(pauses)
	return percentile(pauses, 99)
}
