# Epic 05: Go Agent SDK

## Business Meaning

Customers write their business logic — the LLM calls, tool use, conversation management, and custom integrations — in a function that receives a session context and runs for the life of the call. The Go Agent SDK is Gato's customer-facing API: it abstracts away WebRTC, VAD, STT, and TTS plumbing, and gives customers a clean interface to build voice agents in Go.

This epic also provides the Go equivalent of what `agent-sdk` (Python) does: session lifecycle, init payload delivery, and a clean entrypoint function contract.

---

## Background

The Python `agent-sdk` exposes:
- `WorkerOptions{entrypoint: Callable, deployment_id, max_sessions, ...}`
- `SessionContext{session_id, payload: dict}` with `ctx.accept(sample_rate)` → transport
- `run_app(options)` → infinite reconnect loop

The Go SDK does the same, but rather than subprocess-per-session, everything runs in goroutines in one process. The SDK connects directly to Switchboard (Epic 01). Customers import `github.com/recruit41/voqalcloud/gato/sdk/voqal` and write:

```go
func main() {
    voqal.Run(voqal.Options{
        DeploymentID: os.Getenv("DEPLOYMENT_ID"),
        MaxSessions:  20,
        Entrypoint:   myAgent,
    })
}

func myAgent(ctx context.Context, session *voqal.Session) error {
    transcript, err := session.ListenForTurn(ctx)
    if err != nil { return err }
    response := callMyLLM(transcript)
    return session.Speak(ctx, response)
}
```

**References:**
- Python SDK: `agent-sdk/src/voqalcloud/worker/_app.py`, `_types.py`, `_child.py`
- Gato session: `experiments/gato/experiments/exp-008/pipeline.go`

---

## Tasks

### Task 5.1 — Public API types

Create `gato/sdk/voqal/agent.go`. This is the customer-facing package. Design it conservatively — every exported symbol is a compatibility promise.

```go
// Options configures a Gato worker node.
type Options struct {
    DeploymentID string
    NodeID       string        // optional; default: hostname
    MaxSessions  int           // default: 20
    SwitchboardURL string      // override; default: GATO_SB_URL env var
    SwitchboardToken string    // override; default: GATO_SB_TOKEN env var
    Entrypoint   AgentFunc
    Logger       zerolog.Logger // optional; default: JSON to stdout
}

// AgentFunc is the function customers implement. Called once per session.
// Return nil to end the session cleanly; return an error to end with a log entry.
type AgentFunc func(ctx context.Context, session *Session) error

// Run connects to Switchboard and runs the agent forever.
// It only returns on SIGTERM after draining.
func Run(opts Options)
```

### Task 5.2 — Session type

```go
// Session represents one active browser call.
type Session struct {
    // Read-only after construction.
    ID      SessionID
    Payload map[string]any  // from Switchboard init_payload (JSON-decoded)
    
    // internal
    pipeline *internal.Pipeline
    turnCh   chan Turn
    interruptCh chan InterruptEvent
    log      zerolog.Logger
}

// ListenForTurn blocks until the user finishes speaking and STT delivers a final transcript.
// Returns ErrInterrupted if the session is cancelled or a new turn starts before the
// previous one ends.
func (s *Session) ListenForTurn(ctx context.Context) (Turn, error)

// Speak synthesizes text and streams it to the browser. Blocks until playback finishes
// or the context is cancelled (e.g., user interrupts).
// Returns the HeardText if interrupted mid-utterance.
func (s *Session) Speak(ctx context.Context, text string) (heardText string, err error)

// SpeakStream synthesizes a stream of text chunks (for LLM streaming output).
// Starts playing audio as soon as the first chunk arrives.
func (s *Session) SpeakStream(ctx context.Context, chunks <-chan string) (heardText string, err error)

// SendSignal sends a raw signal to the browser (for RTVI-style side-channel messages).
func (s *Session) SendSignal(payload []byte) error
```

```go
// Turn holds the result of one user speech turn.
type Turn struct {
    Transcript string
    Interrupted bool  // true if the turn ended via interruption (user spoke mid-bot)
    HeardText   string  // what the bot had said before being interrupted
}
```

### Task 5.3 — TTS client interface

Create `gato/sdk/voqal/tts.go`. Customers can swap TTS providers:

```go
type TTSClient interface {
    // Synthesize returns 24kHz mono s16le PCM bytes.
    Synthesize(ctx context.Context, text string) ([]byte, error)
}
```

Default implementation: `internal.GoogleTTSClient`. Customers can inject a custom TTS via `Options.TTSClient`.

Similarly, customers who want to swap STT provide `Options.STTFactory func(ctx context.Context) (STTClient, error)`.

### Task 5.4 — Prewarm pool

When `MaxSessions=20` and `Options.PrewarmCount=5` (optional, default 0), Gato pre-initializes 5 "warm" goroutines that have completed any expensive startup (model loading, connection pre-heating). On a `SessionProbe`, a warm goroutine is popped from the pool (O(1)) rather than cold-starting.

This mirrors the Python SDK's prewarm mechanism (`_child.py: prewarm_child_main`). In Go, this is a pool of goroutines blocked on a channel, not a pool of subprocesses.

Implementation: a `sync.Pool`-like structure holding pre-created `*Session` objects that have completed `setupPipeline()` but haven't started `AgentFunc` yet.

This is a performance optimization; implement after the baseline works.

### Task 5.5 — Init payload delivery

The `SessionReady` Switchboard message carries `init_payload bytes` (JSON). In `handleSessionReady`:

1. Unmarshal `init_payload` as `map[string]any`
2. Store in `Session.Payload`
3. Make `Session.Payload` available before `AgentFunc` is called

Customers use this for per-session configuration: user ID, language, voice style, etc. The control plane populates `init_payload` at session creation time.

### Task 5.6 — Error handling and session isolation

Each `AgentFunc` runs in its own goroutine. Panics in customer code must be recovered and logged, not crashed:

```go
defer func() {
    if r := recover(); r != nil {
        s.log.Error().Interface("panic", r).Str("stack", debug.Stack()).Msg("agent panic recovered")
        s.metrics.Incr("gato_agent_panics_total")
    }
}()
```

If `AgentFunc` returns an error, log at warn level (not error — customer code errors are expected). If it returns `ErrFatal`, escalate to error and terminate the session immediately.

Session goroutines are tracked in a `sync.WaitGroup` per session (not per process). On drain, Gato calls `session.wg.Wait()` before decrementing the global session count.

---

## Definition of Done

- [ ] Customer can write a 20-line `main.go` using the SDK and deploy it as a Gato worker
- [ ] `Session.ListenForTurn()` blocks until STT final transcript; returns on context cancellation
- [ ] `Session.Speak()` plays TTS to the browser and returns when done; returns `heardText` if interrupted
- [ ] `Session.Payload` populated from `init_payload` before `AgentFunc` is called
- [ ] Panic in `AgentFunc` is recovered; other sessions continue unaffected
- [ ] SDK has a working example in `gato/examples/hello-world/`

---

## Verification

### Unit Tests

- `TestSession_ListenForTurn_Delivers`: inject a `Turn` into `session.turnCh`; assert `ListenForTurn` returns it
- `TestSession_ListenForTurn_ContextCancel`: cancel ctx before turn arrives; assert `ErrInterrupted` returned
- `TestSession_Speak_InterruptMidway`: stub TTS; inject interrupt mid-playback; assert `heardText` matches last completed segment; assert `Speak` returns before TTS finishes
- `TestSession_PanicRecovery`: `AgentFunc` that panics; assert goroutine exits cleanly; assert other sessions unaffected
- `TestInitPayload_JSONDecode`: send `init_payload: {"lang": "es-ES"}`; assert `session.Payload["lang"] == "es-ES"`

### Integration Tests

- `TestSDK_HelloWorld`: run the `examples/hello-world` with a `FakeSwitchboard` + `FakeBrowser`; assert the `AgentFunc` is called once per session probe/ack cycle
- `TestSDK_PrewarmPool`: set `PrewarmCount=3`; send 3 simultaneous probes; assert all 3 accepted within 50ms (prewarm eliminates cold-start latency)
- `TestSDK_Drain`: call `Run(ctx)` with cancellable ctx; send a probe; start `AgentFunc` (sleeps 2s); cancel ctx; assert `Run` exits only after `AgentFunc` returns (drain respects in-flight sessions)

### E2E

Build and run `gato/examples/hello-world` (echo agent: repeats what the user said):
```go
func myAgent(ctx context.Context, s *voqal.Session) error {
    for {
        turn, err := s.ListenForTurn(ctx)
        if err != nil { return err }
        _, err = s.Speak(ctx, "You said: " + turn.Transcript)
        if err != nil { return err }
    }
}
```

Connect with aiortc client. Speak a sentence. Assert the bot echoes it. Speak again mid-reply. Assert the bot stops and echoes the new sentence.
