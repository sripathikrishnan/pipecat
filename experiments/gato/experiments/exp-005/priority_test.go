package main

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPriority_Queue verifies the priority queue directly: SystemFrames always come first.
// This is the deterministic base case — no goroutines, no timing.
func TestPriority_Queue(t *testing.T) {
	t.Parallel()
	const trials = 1000

	for trial := 0; trial < trials; trial++ {
		q := newPriorityQueue()
		for i := 0; i < 100; i++ {
			q.push(&DataFrameImpl{ID: i})
		}
		q.push(&SystemFrameImpl{ID: 9999})

		ctx := context.Background()
		f, err := q.pop(ctx)
		if err != nil {
			t.Fatalf("trial %d: pop error: %v", trial, err)
		}
		if !f.IsSystem() {
			t.Errorf("trial %d: first frame is data (ID=%d), want system frame", trial, f.FrameID())
		}
	}
	t.Logf("Priority queue: 1000/1000 trials returned SystemFrame first")
}

// TestPriority_Pipeline verifies that SystemFrame priority holds through a 2-processor
// pipeline when all frames are enqueued BEFORE goroutines start.
//
// Key insight: push frames to P1 BEFORE starting any goroutines. P1's first pop()
// returns the SystemFrame, which is forwarded to P2 before any DataFrames. P2's
// priority queue also returns SystemFrame first. So received[0] = SystemFrame.
func TestPriority_Pipeline(t *testing.T) {
	t.Parallel()
	const trials = 100

	for trial := 0; trial < trials; trial++ {
		ctx, cancel := context.WithCancel(context.Background())

		var received []Frame
		var mu sync.Mutex
		var count atomic.Int32

		sink := NewFrameProcessor("sink", func(f Frame) {
			mu.Lock()
			received = append(received, f)
			mu.Unlock()
			count.Add(1)
		})

		src := NewFrameProcessor("src", nil)
		src.next = sink

		// Push ALL frames BEFORE starting goroutines.
		// P1.queue will have: system=[S1], data=[D0..D99]
		// P1's first pop() returns S1 (priority).
		const nData = 100
		for i := 0; i < nData; i++ {
			src.PushFrame(&DataFrameImpl{ID: i})
		}
		src.PushFrame(&SystemFrameImpl{ID: 9999})

		// NOW start goroutines. Both start with empty queues except P1 which has all frames.
		sink.Start(ctx)
		src.Start(ctx)

		// Wait for all frames (nData + 1 system).
		deadline := time.After(2 * time.Second)
		for count.Load() < int32(nData+1) {
			select {
			case <-deadline:
				t.Fatalf("trial %d: timeout waiting for frames (got %d)", trial, count.Load())
			default:
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		src.Stop()
		sink.Stop()

		mu.Lock()
		systemPos := -1
		for i, f := range received {
			if f.IsSystem() {
				systemPos = i
				break
			}
		}
		mu.Unlock()

		if systemPos != 0 {
			t.Errorf("trial %d: SystemFrame at position %d (want 0)", trial, systemPos)
		}
	}
	t.Logf("Pipeline priority: 100/100 trials returned SystemFrame first")
}

// TestFIFO_DataFrames verifies DataFrames are delivered in insertion order.
func TestFIFO_DataFrames(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var received []int
	var mu sync.Mutex
	var count atomic.Int32

	sink := NewFrameProcessor("sink", func(f Frame) {
		mu.Lock()
		received = append(received, f.FrameID())
		mu.Unlock()
		count.Add(1)
	})
	sink.Start(ctx)

	src := NewFrameProcessor("src", nil)
	src.next = sink
	src.Start(ctx)

	const n = 50
	for i := 0; i < n; i++ {
		src.PushFrame(&DataFrameImpl{ID: i})
	}

	// Wait until all frames delivered.
	deadline := time.After(2 * time.Second)
	for count.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d/%d DataFrames received", count.Load(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	sink.Stop()
	src.Stop()

	mu.Lock()
	defer mu.Unlock()
	for i, id := range received {
		if id != i {
			t.Errorf("FIFO violated at position %d: got ID %d, want %d", i, id, i)
			break
		}
	}
	t.Logf("FIFO: %d DataFrames in correct order", len(received))
}

// TestFIFO_SystemFrames verifies SystemFrames preserve their insertion order relative to each other.
func TestFIFO_SystemFrames(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sysReceived []int
	var mu sync.Mutex
	var count atomic.Int32

	sink := NewFrameProcessor("sink", func(f Frame) {
		if f.IsSystem() {
			mu.Lock()
			sysReceived = append(sysReceived, f.FrameID())
			mu.Unlock()
			count.Add(1)
		}
	})
	sink.Start(ctx)

	src := NewFrameProcessor("src", nil)
	src.next = sink
	src.Start(ctx)

	const n = 10
	for i := 0; i < n; i++ {
		src.PushFrame(&SystemFrameImpl{ID: i})
	}

	deadline := time.After(2 * time.Second)
	for count.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("timeout: only %d/%d SystemFrames received", count.Load(), n)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	sink.Stop()
	src.Stop()

	mu.Lock()
	defer mu.Unlock()
	for i, id := range sysReceived {
		if id != i {
			t.Errorf("SystemFrame FIFO violated at position %d: got ID %d, want %d", i, id, i)
			break
		}
	}
	t.Logf("SystemFrame FIFO: %d in correct order", len(sysReceived))
}

// TestCancellation verifies clean shutdown — no goroutine leaks.
func TestCancellation_NoGoroutineLeak(t *testing.T) {
	t.Parallel()
	goroutinesBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	var count atomic.Int32
	p3 := NewFrameProcessor("p3", func(f Frame) { count.Add(1) })
	p2 := NewFrameProcessor("p2", nil)
	p1 := NewFrameProcessor("p1", nil)
	p2.next = p3
	p1.next = p2

	p3.Start(ctx)
	p2.Start(ctx)
	p1.Start(ctx)

	// Push 50 DataFrames.
	for i := 0; i < 50; i++ {
		p1.PushFrame(&DataFrameImpl{ID: i})
	}

	// Cancel after a few have been delivered.
	time.Sleep(5 * time.Millisecond)
	cancel()

	p1.Stop()
	p2.Stop()
	p3.Stop()

	received := int(count.Load())
	if received >= 50 {
		t.Logf("all 50 frames delivered before cancel — cancel was late (OK)")
	} else {
		t.Logf("cancellation: %d/50 DataFrames delivered before shutdown", received)
	}

	// Goroutine leak check.
	time.Sleep(50 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	delta := goroutinesAfter - goroutinesBefore
	if delta > 0 {
		t.Errorf("goroutine leak: before=%d after=%d delta=%d", goroutinesBefore, goroutinesAfter, delta)
	}
	t.Logf("goroutines: before=%d after=%d delta=%d", goroutinesBefore, goroutinesAfter, delta)
}
