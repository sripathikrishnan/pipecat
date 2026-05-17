# EXP-009: Pipeline Performance, Stability & Resource Utilization

**Risk addressed**: Can Gato sustain N concurrent voice sessions with the full CPU-constrained
pipeline running? What is the maximum sessions-per-process before latency degrades? Does the
system hold up over 30+ minutes without leaks or drift?

**Status**: [ ]

**Depends on**: EXP-003 (queue), EXP-004 (VAD/CGO), EXP-005 (FrameProcessor), EXP-007 (HEARD)

---

## Scope

This experiment uses **mock STT and TTS** to isolate the CPU-constrained hot path from network
I/O. Real STT and TTS have variable external latency that would confound CPU measurements.

**What is real** (CPU-bound, runs fully):
- Audio input pipeline: PCM ingestion, 20 ms chunk normalization
- Silero VAD (CGO/ONNX) — every 20 ms
- TurnDetectorV3 (CGO/ONNX) — on silence transitions
- FrameProcessor priority queue + observer fan-out — every frame
- Interrupt-safe audio queue (FrameQueue from EXP-003)
- Real-time audio output pacing (10 ms chunks + sleep, from EXP-001)
- UserAggregator + AssistantAggregator
- Resampler 24 kHz → 48 kHz (from EXP-002)

**What is mocked** (deterministic, no network):
- STT: after 200 ms fixed delay from turn end, emits a pre-set `TranscriptionFrame`
- TTS: reads from `testdata/tts_response.raw` (pre-recorded 5-second response) and feeds
  it to the output transport at the rate the transport consumes it
- IPC bridge: in-process function call, no WebSocket

No real browser or Pion peer connection. Audio input is a goroutine feeding raw PCM from a
loop file. Audio output is a goroutine consuming from the audio queue and discarding (or
measuring) the bytes.

---

## Session Simulation

Each simulated session runs this loop forever (until cancelled):

```
1. Feed 8 seconds of "user speech" audio from loop file → VAD fires → TurnDetect fires
2. Mock STT returns transcript after 200 ms
3. UserAggregator emits TurnStarted → mock IPC → AssistantAggregator emits TextFrame
4. Mock TTS feeds 5 sec of audio to output transport at real-time rate
5. Output transport paces audio at 10 ms chunks
6. BotStopped fires → loop restarts after 1 second pause
7. Every 60–90 seconds: inject random InterruptionFrame mid-playback
```

---

## Load Levels

Run the full test suite at each load level. Each level runs for **10 minutes**.
After all load levels: one **30-minute stability run** at the 50-session level.

| Level | Sessions | Expected CPU | Expected memory |
|-------|----------|--------------|-----------------|
| L1    | 1        | baseline     | baseline        |
| L2    | 10       | ~10×         | ~10× per-session|
| L3    | 25       | measure      | measure         |
| L4    | 50       | measure      | measure         |
| L5    | 100      | measure      | measure         |

The break-point is the load level where **p99 turn-around latency exceeds 500 ms** or where
CPU reaches 90% on any core. Record this as "max sessions per process".

---

## Metrics Collection

Every 30 seconds, record:

```
sessions:      N (configured)
goroutines:    runtime.NumGoroutine()
memory_rss:    /proc/self/status or runtime.ReadMemStats
heap_alloc:    runtime.MemStats.HeapAlloc
gc_pause_p99:  runtime.MemStats.PauseNs (last 256 GC pauses)
cpu_percent:   os.Getenv("GOMEMLIMIT") + runtime/pprof CPU sample
```

Per-turn metrics (every completed turn, all sessions):

```
turn_around_p50:   time from VAD-end to first audio byte emitted
turn_around_p99:   same
vad_latency_p99:   time for one Silero inference call
td_latency_p99:    time for one TurnDetect inference call
interrupt_latency: time from InterruptionFrame to audio-stop (when injected)
```

---

## Stability Checks (30-minute run at 50 sessions)

Sample goroutine count every 5 minutes. Assert:
- Goroutine count does not grow by more than 5% over 30 minutes.
- Heap alloc does not grow monotonically (GC is reclaiming memory).
- No panics or errors logged.
- p99 turn-around latency does not degrade by more than 20% from minute 5 to minute 30.

---

## Production Hardening Checklist

Run each of the following failure injection scenarios during the 50-session load run:

| Scenario | How to inject | Expected behaviour |
|----------|---------------|--------------------|
| Mock STT timeout | Mock STT hangs for 5 seconds | Turn-around degraded for that session; other sessions unaffected |
| Mock TTS slow | Mock TTS delivers audio at 0.5× realtime | That session's output slows; no impact on others |
| Rapid interrupts | Send InterruptionFrame every 200 ms for 10 sec | No goroutine leak; session recovers |
| Context cancel (session end) | Cancel one session's context | Goroutine count drops; no panic |
| ONNX inference error | Return error from mock VAD for one session | ErrorFrame emitted upstream; session continues or terminates gracefully |

For each: record whether Gato isolates the failure (other sessions unaffected) and whether
the affected session recovers or terminates cleanly.

---

## Success Criteria

1. **L1–L3 (1–25 sessions)**: p99 turn-around latency < 300 ms.
2. **L4 (50 sessions)**: p99 turn-around latency < 500 ms.
3. **L5 (100 sessions)**: measure only — record latency, do not require passing.
4. **Stability (30 min, 50 sessions)**: goroutine count stable ± 5%; no memory growth trend;
   no degradation in p99 after minute 5.
5. **Production hardening**: all 5 failure scenarios handled without cascading failure.
6. `-race` clean on all load levels.

---

## Program

```
experiments/gato/experiments/exp-009/
  main.go             — CLI: --sessions=N --duration=30m --pprof=:6060
  session.go          — one simulated session (goroutine cluster)
  mock_stt.go         — fixed 200 ms delay, returns canned TranscriptionFrame
  mock_tts.go         — reads testdata/tts_response.raw, feeds at real-time rate
  metrics.go          — periodic metrics printer + CSV writer
  harness_test.go     — the 10-minute load runs (go test -run=TestLoad -v -timeout=2h)
  testdata/
    user_speech.raw     — 10 sec loop, 16 kHz mono (real or synthetic speech)
    tts_response.raw    — 5 sec response, 48 kHz mono (post-resample)
```

Run with pprof enabled (`--pprof=:6060`) for heap/goroutine inspection during the run:
```bash
go tool pprof http://localhost:6060/debug/pprof/goroutine
go tool pprof http://localhost:6060/debug/pprof/heap
```

---

## What Failure Looks Like

| Symptom | Likely cause |
|---------|--------------|
| Goroutine count grows 1 per session per turn | A goroutine is created per turn and never cancelled |
| Heap grows monotonically | Audio byte slices accumulating in a queue; check FrameQueue Reset() |
| p99 latency spikes under L3 | VAD/ONNX goroutine pool too small; increase pool size |
| VAD p99 > 20 ms | ONNX session is being created per-inference; fix to shared session pool |
| One session failure kills others | Context cancellation is shared; fix to per-session context |
| GC pause > 50 ms | Large allocations on hot path; use sync.Pool for audio byte slices |
