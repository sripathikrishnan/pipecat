package main

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"
)

// checkCredentials skips the test if no GCP credentials are available.
// We detect credentials by checking GOOGLE_APPLICATION_CREDENTIALS or
// whether `gcloud auth print-access-token` succeeds.
func checkCredentials(t *testing.T) {
	t.Helper()

	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return
	}

	// Try gcloud ADC token; if that fails we have no credentials.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
	if err := cmd.Run(); err != nil {
		t.Skip("no GCP credentials available (set GOOGLE_APPLICATION_CREDENTIALS or run gcloud auth login); skipping")
	}
}

// makeSilence returns n bytes of silence (all zeros) as raw PCM int16.
func makeSilence(bytes int) []byte {
	return make([]byte, bytes)
}

// makeSineWave returns durationMs milliseconds of 440 Hz sine as raw PCM int16 at 16 kHz.
func makeSineWave(durationMs int) []byte {
	samples := 16000 * durationMs / 1000
	buf := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		t := float64(i) / 16000.0
		sample := int16(16000 * math.Sin(2*math.Pi*440*t))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}
	return buf
}

// sendChunked sends audio in 100 ms chunks with no sleep between them.
func sendChunked(t *testing.T, client *StreamingSTTClient, audio []byte) {
	t.Helper()
	chunkSize := 16000 * 2 * 100 / 1000 // 100 ms at 16 kHz int16
	for i := 0; i < len(audio); i += chunkSize {
		end := i + chunkSize
		if end > len(audio) {
			end = len(audio)
		}
		if err := client.SendAudio(audio[i:end]); err != nil {
			t.Errorf("SendAudio: %v", err)
			return
		}
	}
}

// drainResults reads from the results channel until it is closed or the timeout fires.
// Returns the collected results.
func drainResults(ch <-chan TranscriptionResult, timeout time.Duration) []TranscriptionResult {
	var out []TranscriptionResult
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, r)
		case <-deadline.C:
			return out
		}
	}
}

// TestSTT_BasicConnection verifies that we can open a stream, send 2 seconds of
// audio, and receive no error. We do not assert on transcript content because
// the sine wave does not produce real speech, but the connection must succeed.
func TestSTT_BasicConnection(t *testing.T) {
	if testing.Short() {
		checkCredentials(t)
	} else {
		checkCredentials(t)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewStreamingSTTClient(ctx)
	if err != nil {
		t.Fatalf("NewStreamingSTTClient: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Logf("Close: %v", err)
		}
	}()

	client.Start(ctx)

	// Send 2 seconds of audio.
	audio := makeSineWave(2000)
	sendChunked(t, client, audio)

	// Close and wait for results to drain.
	if err := client.Close(); err != nil {
		t.Logf("Close: %v (non-fatal)", err)
	}

	// Drain up to 5 seconds for any results.
	_ = drainResults(client.Results(), 5*time.Second)

	// If we got here without a panic or fatal error, the test passes.
}

// TestSTT_StreamRestart tests the reconnect path.
//
// Part 1: exercises the replayBuffer directly by pushing audio into it (no
// network required) and verifying capacity capping + reset.
//
// Part 2: opens a real stream, sends audio for 3 seconds, then closes and
// reopens a new stream. Verifies that the new stream can be created without
// error (exercising the reconnect code path). Requires GCP credentials.
func TestSTT_StreamRestart(t *testing.T) {
	// Part 1: replay buffer unit verification (no credentials required).
	t.Run("replayBuffer", func(t *testing.T) {
		buf := newReplayBuffer(500 * time.Millisecond)

		// Push 1 second of audio in 100 ms chunks (10 chunks × 3200 bytes).
		const chunkBytes = 3200 // 100 ms at 16 kHz int16
		for i := 0; i < 10; i++ {
			buf.push(make([]byte, chunkBytes))
		}

		// Buffer must hold at most 500 ms = 16000 bytes.
		const maxBytes = 16000
		total := 0
		for _, c := range buf.snapshot() {
			total += len(c)
		}
		if total > maxBytes {
			t.Errorf("buffer holds %d bytes, want <= %d (500 ms)", total, maxBytes)
		}
		t.Logf("replay buffer after 1s push: %d bytes", total)

		buf.reset()
		if s := buf.snapshot(); len(s) != 0 {
			t.Errorf("after reset, buffer has %d items, want 0", len(s))
		}
	})

	// Part 2: real reconnect path (requires credentials).
	t.Run("reconnect", func(t *testing.T) {
		checkCredentials(t)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Open first stream, send 2 seconds, close it.
		client1, err := NewStreamingSTTClient(ctx)
		if err != nil {
			t.Fatalf("NewStreamingSTTClient (stream 1): %v", err)
		}
		client1.Start(ctx)
		sendChunked(t, client1, makeSineWave(2000))
		if err := client1.Close(); err != nil {
			t.Logf("Close stream 1: %v (non-fatal)", err)
		}
		drainResults(client1.Results(), 3*time.Second)

		// Simulate restart: open a fresh stream (as the reconnect loop would).
		client2, err := NewStreamingSTTClient(ctx)
		if err != nil {
			t.Fatalf("NewStreamingSTTClient (stream 2 / restart): %v", err)
		}
		client2.Start(ctx)
		sendChunked(t, client2, makeSineWave(1000))
		if err := client2.Close(); err != nil {
			t.Logf("Close stream 2: %v (non-fatal)", err)
		}
		drainResults(client2.Results(), 3*time.Second)

		t.Log("reconnect path exercised successfully")
	})
}

// TestSTT_SilenceHandling sends 5 seconds of silence and verifies the client
// does not panic and the stream remains alive.
//
// In a -short run we still hit the network; silence is cheap (very few bytes,
// no STT computation) so this should complete quickly.
func TestSTT_SilenceHandling(t *testing.T) {
	checkCredentials(t)

	// Use a shorter duration under -short to keep CI fast.
	silenceSecs := 5
	if testing.Short() {
		silenceSecs = 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(silenceSecs+20)*time.Second)
	defer cancel()

	client, err := NewStreamingSTTClient(ctx)
	if err != nil {
		t.Fatalf("NewStreamingSTTClient: %v", err)
	}
	defer client.Close()

	client.Start(ctx)

	// Send silence in 100 ms chunks.
	chunkSize := 16000 * 2 * 100 / 1000 // 100 ms
	numChunks := silenceSecs * 10        // chunks per second = 10 at 100 ms each
	for i := 0; i < numChunks; i++ {
		if err := client.SendAudio(makeSilence(chunkSize)); err != nil {
			t.Fatalf("SendAudio (chunk %d): %v", i, err)
		}
	}

	// Close and drain — no panic is the success criterion.
	if err := client.Close(); err != nil {
		t.Logf("Close: %v (non-fatal)", err)
	}
	results := drainResults(client.Results(), 5*time.Second)
	t.Logf("received %d results during silence", len(results))
}

// TestReplayBuffer_Unit tests the replay buffer in isolation (no network required).
func TestReplayBuffer_Unit(t *testing.T) {
	buf := newReplayBuffer(500 * time.Millisecond)

	// Push 600 ms of audio in 100 ms chunks.
	// Each 100 ms chunk at 16 kHz int16 = 3200 bytes.
	chunkSize := 3200
	for i := 0; i < 6; i++ {
		chunk := make([]byte, chunkSize)
		buf.push(chunk)
	}

	// Buffer must not exceed 500 ms.
	const maxBytes = 16000 * 2 / 2 // 500 ms = 16000 bytes
	total := 0
	for _, c := range buf.snapshot() {
		total += len(c)
	}
	if total > maxBytes {
		t.Errorf("buffer holds %d bytes, want <= %d", total, maxBytes)
	}

	// Reset clears.
	buf.reset()
	if s := buf.snapshot(); len(s) != 0 {
		t.Errorf("after reset snapshot has %d items", len(s))
	}
}
