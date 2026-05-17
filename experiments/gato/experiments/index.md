# Gato — Experiments Index

Each experiment is a small, self-contained Go program that answers one specific question.
No Gato framework code is required to run an experiment — the point is to discover the right
implementation before committing to it.

Experiments are ordered by the dependency graph. Run them in order.
If an experiment fails, stop and redesign before proceeding.

---

## Dependency Graph

```
EXP-001 (Pion audio pacing)
  └── EXP-002 (Sample rate / resampling)
        └── EXP-003 (Interrupt-safe audio queue)
              └── EXP-007 ([HEARD] end-to-end)
                    └── EXP-008 (Hello world session)

EXP-004 (Silero VAD / CGO)       ──────────────────────┐
EXP-005 (FrameProcessor priority) ─────────────────────┤
EXP-006 (Google STT streaming)   ──────────────────────┤
                                                        └── EXP-008
```

---

## Experiment List

| ID      | Title                            | Risk Addressed              | Status | Depends on      |
|---------|----------------------------------|-----------------------------|--------|-----------------|
| EXP-001 | Pion audio pacing                | Output transport clock      | [ ]    | —               |
| EXP-002 | Sample rate & resampling         | Resampler / Opus codec      | [ ]    | EXP-001         |
| EXP-003 | Interrupt-safe audio queue       | FrameQueue + interrupt path | [ ]    | EXP-001, EXP-002|
| EXP-004 | Silero VAD via CGO/ONNX          | VAD latency, multi-session  | [ ]    | —               |
| EXP-005 | FrameProcessor priority queue    | SystemFrame priority        | [ ]    | —               |
| EXP-006 | Google Cloud STT gRPC streaming  | STT reconnect, latency      | [ ]    | —               |
| EXP-007 | [HEARD] end-to-end interruption  | Exact heard-text accuracy   | [ ]    | EXP-003         |
| EXP-008 | Hello world session              | Full pipeline integration   | [ ]    | EXP-003–007     |

Status: `[ ]` not started · `[~]` in progress · `[x]` complete · `[!]` blocked/redesign needed

---

## What a Passing Run Looks Like

An experiment passes when all its **Success Criteria** are met.
Record measurements in the experiment file under a `## Results` section
(add it when the experiment runs; do not pre-fill).

A failing experiment is not a failure — it is the point. It tells you
the architecture assumption was wrong before you built 3000 lines on top of it.
