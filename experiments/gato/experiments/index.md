# Gato — Experiments Index

Each experiment is a small, self-contained Go program that answers one specific question.
No Gato framework code is required to run an experiment — the point is to discover the right
implementation before committing to it.

Experiments are ordered by the dependency graph. Run them in order.
If an experiment fails, stop and redesign before proceeding.

---

## Dependency Graph

```
EXP-001 (Output transport end-to-end)
  └── EXP-002 (Resampling — best Go implementation)
        └── EXP-003 (Interrupt-safe audio queue)
              └── EXP-007 ([HEARD] end-to-end)
                    └── EXP-008 (Hello world session)
                          └── EXP-009 (Performance, stability, production hardening)

EXP-004 (Silero VAD / CGO)        ─────────────────────┐
EXP-005 (FrameProcessor priority)  ────────────────────┤
EXP-006 (Google STT streaming)    ─────────────────────┤
                                                        └── EXP-008 → EXP-009
```

---

## Experiment List

| ID      | Title                                    | Risk Addressed                       | Status | Depends on       |
|---------|------------------------------------------|--------------------------------------|--------|------------------|
| EXP-001 | Output transport end-to-end              | Pacing, chunk norm, BotSpeaking, interrupt | [ ] | —           |
| EXP-002 | Audio resampling (24 kHz → 48 kHz)       | Best Go resampling implementation    | [ ]    | EXP-001          |
| EXP-003 | Interrupt-safe audio queue               | FrameQueue semantics, race-free      | [ ]    | EXP-001, EXP-002 |
| EXP-004 | Silero VAD via CGO/ONNX                  | VAD latency, multi-session isolation | [ ]    | —                |
| EXP-005 | FrameProcessor priority queue            | SystemFrame priority guarantee       | [ ]    | —                |
| EXP-006 | Google Cloud STT gRPC streaming          | Stream reconnect, latency            | [ ]    | —                |
| EXP-007 | [HEARD] end-to-end interruption          | Exact heard-text accuracy            | [ ]    | EXP-003          |
| EXP-008 | Hello world session                      | Full pipeline integration            | [ ]    | EXP-003–007      |
| EXP-009 | Pipeline performance, stability, hardening | Sessions/process, leaks, failure isolation | [ ] | EXP-008   |

Status: `[ ]` not started · `[~]` in progress · `[x]` complete · `[!]` blocked/redesign needed

---

## Notes on EXP-001 (Output Transport)

EXP-001 is the most important experiment. The output transport is the hardest component to
port from pipecat and the one most likely to have subtle bugs. It covers:

- **Chunk normalization**: TTS blobs re-chunked into 10 ms pieces
- **Real-time pacing**: Pion push model vs aiortc pull model — sleep 10 ms per chunk
- **BotStarted/Stopped state machine**: both-direction frame push
- **Interruption**: audio stops within one chunk; EndFrame survives
- **Resume**: new audio plays without restarting peer connection

If EXP-001 reveals that 10 ms sleep pacing is insufficient (e.g. Pion's internal buffering
adds latency), all interrupt latency guarantees must be revisited before proceeding.

---

## Notes on EXP-002 (Resampling)

48 kHz is mandatory — browser WebRTC / Opus requires it. Google TTS outputs at 24 kHz.
Resampling is unavoidable. EXP-002 finds the most efficient Go implementation for the
2:1 integer upsampling ratio. Default choice is pure Go linear interpolation unless benchmarks
or quality tests reveal a problem.

---

## Notes on EXP-009 (Performance & Stability)

EXP-009 uses mock STT and TTS to isolate the CPU-constrained path. The CPU hot path is:
VAD (CGO/ONNX every 20 ms) + TurnDetect (CGO/ONNX on silence) + FrameProcessor priority
queues + real-time audio output pacing. This is what determines sessions-per-process capacity.

Run time: 10 minutes per load level (L1–L5), then a 30-minute stability run at 50 sessions.
Use `-race` on L1 and L2; drop it for L4/L5 (race detector has ~5–10× overhead).

---

## What a Passing Run Looks Like

An experiment passes when all its **Success Criteria** are met.
Record measurements in the experiment file under a `## Results` section
(add it when the experiment runs; do not pre-fill).

A failing experiment is not a failure — it is the point. It tells you
the architecture assumption was wrong before you built 3000 lines on top of it.
