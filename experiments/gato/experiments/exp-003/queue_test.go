package main

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// --- Unit tests ---

func TestBasicPutGet(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	for i := 0; i < 10; i++ {
		q.Put(&AudioFrame{ID: i})
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		f, err := q.Get(ctx)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		af, ok := f.(*AudioFrame)
		if !ok {
			t.Fatalf("expected *AudioFrame, got %T", f)
		}
		if af.ID != i {
			t.Errorf("FIFO violated: got ID %d, want %d", af.ID, i)
		}
	}
}

func TestHasUninterruptible_False(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	for i := 0; i < 5; i++ {
		q.Put(&AudioFrame{ID: i})
	}
	if q.HasUninterruptible() {
		t.Error("HasUninterruptible should be false with only AudioFrames")
	}
}

func TestHasUninterruptible_True(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	for i := 0; i < 5; i++ {
		q.Put(&AudioFrame{ID: i})
	}
	q.Put(&EndFrame{})
	if !q.HasUninterruptible() {
		t.Error("HasUninterruptible should be true after putting EndFrame")
	}
	// Drain AudioFrames, then EndFrame.
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		q.Get(ctx)
	}
	if !q.HasUninterruptible() {
		t.Error("HasUninterruptible should still be true (EndFrame not yet dequeued)")
	}
	q.Get(ctx) // dequeue EndFrame
	if q.HasUninterruptible() {
		t.Error("HasUninterruptible should be false after EndFrame dequeued")
	}
}

func TestReset_PreservesEndFrame(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	q.Put(&AudioFrame{ID: 0})
	q.Put(&AudioFrame{ID: 1})
	q.Put(&EndFrame{})
	q.Put(&AudioFrame{ID: 2})
	q.Put(&AudioFrame{ID: 3})

	q.Reset()

	if q.Len() != 1 {
		t.Fatalf("after Reset: len=%d, want 1 (EndFrame)", q.Len())
	}
	ctx := context.Background()
	f, _ := q.Get(ctx)
	if _, ok := f.(*EndFrame); !ok {
		t.Errorf("expected *EndFrame after Reset, got %T", f)
	}
	if q.HasUninterruptible() {
		t.Error("HasUninterruptible should be false after EndFrame dequeued")
	}
}

func TestReset_WhenEmpty(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	q.Reset() // must not panic
	if q.Len() != 0 {
		t.Errorf("empty reset: len=%d, want 0", q.Len())
	}
}

func TestReset_ConcurrentPutReset(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	var wg sync.WaitGroup

	// Producer goroutine 1
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			q.Put(&AudioFrame{ID: i})
		}
	}()

	// Producer goroutine 2
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 50; i < 100; i++ {
			q.Put(&AudioFrame{ID: i})
		}
	}()

	// Reset goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			q.Reset()
			time.Sleep(time.Millisecond)
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: concurrent put+reset did not complete within 2s")
	}
}

func TestGet_ContextCancel(t *testing.T) {
	t.Parallel()
	q := NewFrameQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	go wakeOnCancel(ctx, q)

	f, err := q.Get(ctx)
	if err == nil {
		t.Errorf("expected context error, got frame: %v", f)
	}
}

// --- Integration test ---

// TestIntegration_InterruptMidPlayback simulates the output transport audio task.
func TestIntegration_InterruptMidPlayback(t *testing.T) {
	t.Parallel()
	goroutinesBefore := runtime.NumGoroutine()

	q := NewFrameQueue()

	// Feed 20 AudioFrames + EndFrame.
	const totalAudio = 20
	for i := 0; i < totalAudio; i++ {
		q.Put(&AudioFrame{ID: i})
	}
	q.Put(&EndFrame{})

	// Start the audio task: reads from queue, records frames, exits on EndFrame.
	var played []Frame
	var playedMu sync.Mutex
	taskCtx, taskCancel := context.WithCancel(context.Background())
	defer taskCancel()

	var taskWg sync.WaitGroup
	taskWg.Add(1)
	go wakeOnCancel(taskCtx, q) // wake Get() when context cancelled
	go func() {
		defer taskWg.Done()
		for {
			f, err := q.Get(taskCtx)
			if err != nil || f == nil {
				return
			}
			playedMu.Lock()
			played = append(played, f)
			playedMu.Unlock()

			if _, ok := f.(*EndFrame); ok {
				return
			}
			// Simulate 1 ms processing per frame (real-time pace would be 10 ms).
			time.Sleep(time.Millisecond)
		}
	}()

	// After 5 ms, inject Reset (simulating InterruptionFrame with EndFrame present).
	time.Sleep(5 * time.Millisecond)
	q.Reset()
	// Re-enqueue a new EndFrame (the interrupt handler always does this to terminate the session).
	q.Put(&EndFrame{})

	// Wait for audio task to exit.
	done := make(chan struct{})
	go func() { taskWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("audio task did not exit within 500 ms — EndFrame was not delivered")
	}

	// Assertions.
	playedMu.Lock()
	nPlayed := len(played)
	endCount := 0
	for _, f := range played {
		if _, ok := f.(*EndFrame); ok {
			endCount++
		}
	}
	playedMu.Unlock()

	if nPlayed >= totalAudio+1 {
		t.Errorf("all %d frames played — Reset() did not drain the queue", totalAudio)
	}
	if endCount != 1 {
		t.Errorf("EndFrame count = %d, want exactly 1", endCount)
	}
	if nPlayed > 0 {
		t.Logf("AudioFrames played before interrupt: %d (of %d)", nPlayed-endCount, totalAudio)
	}

	// Goroutine leak check.
	// Give goroutines 100 ms to fully exit.
	time.Sleep(100 * time.Millisecond)
	goroutinesAfter := runtime.NumGoroutine()
	delta := goroutinesAfter - goroutinesBefore
	if delta > 2 {
		t.Errorf("goroutine leak: before=%d after=%d delta=%d (want ≤2 for wakeOnCancel)",
			goroutinesBefore, goroutinesAfter, delta)
	}
	t.Logf("goroutine delta: %d (acceptable ≤2)", delta)
}
