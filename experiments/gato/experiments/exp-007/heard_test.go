package main

import (
	"fmt"
	"testing"
	"time"
)

// makeSilence produces durationMs of silent 48 kHz mono 16-bit PCM.
// Silence is fine for this experiment — we are testing text tracking, not audio quality.
func makeSilence(durationMs int) []byte {
	samples := sampleRate * durationMs / 1000
	return make([]byte, samples*bytesPerSample*channels)
}

// buildTransport creates an OutputTransport loaded with the three FakeTTS segments:
//
//	Segment 1: "Hello world"  — 500 ms → 50 chunks
//	Segment 2: "How are you"  — 500 ms → 50 chunks
//	Segment 3: "today"        — 300 ms → 30 chunks
//
// Total: 1300 ms of audio = 130 chunks × 10 ms.
// Seal (close queue) is called here so the audio task exits when all chunks drain.
func buildTransport() *OutputTransport {
	t := newOutputTransport()
	t.enqueueSegment("Hello world", makeSilence(500))
	t.enqueueSegment("How are you", makeSilence(500))
	t.enqueueSegment("today", makeSilence(300))
	t.sealQueue()
	return t
}

// waitSegments blocks until n segments have fully played, returning their texts.
func waitSegments(tr *OutputTransport, n int) []string {
	var segs []string
	for i := 0; i < n; i++ {
		select {
		case s := <-tr.segmentDone:
			segs = append(segs, s)
		case <-time.After(10 * time.Second):
			panic("timed out waiting for segment to complete")
		}
	}
	return segs
}

// TestHEARD_0ms — interrupt before any chunk plays; heardText must be empty.
// Wall-clock timing is fine here: we interrupt immediately on creation.
func TestHEARD_0ms(t *testing.T) {
	t.Parallel()
	for run := 0; run < 10; run++ {
		tr := buildTransport()
		got := tr.handleInterrupt()
		if got != "" {
			t.Errorf("run %d: got %q, want %q", run, got, "")
		}
	}
}

// TestHEARD_MidSeg1 — interrupt 200 ms in; segment 1 takes ~550 ms wall-clock,
// so there is a 350 ms margin. Heard text must still be empty.
func TestHEARD_MidSeg1(t *testing.T) {
	t.Parallel()
	for run := 0; run < 10; run++ {
		tr := buildTransport()
		time.Sleep(200 * time.Millisecond)
		got := tr.handleInterrupt()
		if got != "" {
			t.Errorf("run %d interruptAt=200ms: got %q, want %q", run, got, "")
		}
	}
}

// TestHEARD_AfterSeg1 — interrupt immediately after segment 1 completes.
// Uses segmentDone channel for precise synchronization instead of wall-clock timing.
// Expected: "Hello world".
func TestHEARD_AfterSeg1(t *testing.T) {
	t.Parallel()
	const want = "Hello world"
	for run := 0; run < 10; run++ {
		tr := buildTransport()
		waitSegments(tr, 1)
		got := tr.handleInterrupt()
		if got != want {
			t.Errorf("run %d: got %q, want %q", run, got, want)
		}
	}
}

// TestHEARD_AfterSeg2 — interrupt immediately after segment 2 completes.
// Expected: "Hello world How are you".
func TestHEARD_AfterSeg2(t *testing.T) {
	t.Parallel()
	const want = "Hello world How are you"
	for run := 0; run < 10; run++ {
		tr := buildTransport()
		waitSegments(tr, 2)
		got := tr.handleInterrupt()
		if got != want {
			t.Errorf("run %d: got %q, want %q", run, got, want)
		}
	}
}

// TestHEARD_AfterAll — interrupt after the audio task has already drained the queue
// and exited. handleInterrupt must still return the full heard text.
// Expected: "Hello world How are you today".
func TestHEARD_AfterAll(t *testing.T) {
	t.Parallel()
	const want = "Hello world How are you today"
	for run := 0; run < 10; run++ {
		tr := buildTransport()
		waitSegments(tr, 3)
		got := tr.handleInterrupt()
		if got != want {
			t.Errorf("run %d: got %q, want %q", run, got, want)
		}
	}
}

// TestHEARD_Summary is a non-parallel convenience run that prints all cases
// with timing measurements for the results log.
func TestHEARD_Summary(t *testing.T) {
	type row struct {
		label string
		fn    func(tr *OutputTransport) string
		want  string
	}

	rows := []row{
		{
			label: "interrupt@0ms (before any chunk)",
			fn:    func(tr *OutputTransport) string { return tr.handleInterrupt() },
			want:  "",
		},
		{
			label: "interrupt@200ms (mid seg1, 350ms margin)",
			fn: func(tr *OutputTransport) string {
				time.Sleep(200 * time.Millisecond)
				return tr.handleInterrupt()
			},
			want: "",
		},
		{
			label: "interrupt after seg1 done",
			fn: func(tr *OutputTransport) string {
				waitSegments(tr, 1)
				return tr.handleInterrupt()
			},
			want: "Hello world",
		},
		{
			label: "interrupt after seg2 done",
			fn: func(tr *OutputTransport) string {
				waitSegments(tr, 2)
				return tr.handleInterrupt()
			},
			want: "Hello world How are you",
		},
		{
			label: "interrupt after all done",
			fn: func(tr *OutputTransport) string {
				waitSegments(tr, 3)
				return tr.handleInterrupt()
			},
			want: "Hello world How are you today",
		},
	}

	for _, row := range rows {
		start := time.Now()
		tr := buildTransport()
		got := row.fn(tr)
		elapsed := time.Since(start)

		status := "PASS"
		if got != row.want {
			status = "FAIL"
			t.Errorf("%s: got %q, want %q", row.label, got, row.want)
		}
		t.Log(fmt.Sprintf("%s  %-42s  elapsed=%-8v  playedMs=%d  heard=%q",
			status, row.label, elapsed.Round(time.Millisecond), tr.playedMs.Load(), got))
	}
}
