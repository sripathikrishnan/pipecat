# Custom LLM Service — Frame Analysis

Design notes for building a self-managing LLM service with simplified user and assistant aggregators.

## Context

A self-managing LLM service owns its own prompt, tools, and configuration internally. Pipecat is not responsible for context management. This changes which frames the aggregators need to handle.

---

## Frames Safe to Drop

These only exist to shuttle context/config between external callers and the standard LLM service. A self-managing LLM service makes them redundant.

| Frame | Why it exists | Safe to drop? |
|---|---|---|
| `LLMContextFrame` | Triggers LLM inference by carrying the full context upstream | Yes — your LLM triggers itself |
| `LLMRunFrame` | External "run inference now" signal | Yes |
| `LLMMessagesAppendFrame` / `LLMMessagesUpdateFrame` / `LLMMessagesTransformFrame` | External context mutation | Yes |
| `LLMSetToolsFrame` / `LLMSetToolChoiceFrame` | External tool management | Yes |
| `LLMContextAssistantTimestampFrame` | Internal timestamp for transcript analytics | Yes (or keep for your own telemetry) |
| `LLMAssistantPushAggregationFrame` | TTS→aggregator "I just spoke this, add to context" signal | Yes — if LLM owns context, it tracks its own output |
| `LLMMarkerFrame` | Turn-completion detection markers (✓/○/◐) | Yes, unless using `FilterIncompleteUserTurnStrategies` |
| `LLMThoughtStartFrame` / `LLMThoughtTextFrame` / `LLMThoughtEndFrame` | Reasoning tokens → context | Yes, if your LLM handles thinking internally |

---

## Function Call Frames — Conditional

The assistant aggregator uses all four to manage context, but **RTVI also consumes some of them** to relay function call events to the client.

| Frame | Used by | Drop? |
|---|---|---|
| `FunctionCallsStartedFrame` | Assistant aggregator + RTVI | Keep if RTVI is in the stack |
| `FunctionCallResultFrame` | Assistant aggregator + RTVI | Keep if RTVI is in the stack |
| `FunctionCallInProgressFrame` | Assistant aggregator only | Safe to drop |
| `FunctionCallCancelFrame` | Assistant aggregator only | Safe to drop |

**Rule of thumb:** If RTVI is in the stack, emit `FunctionCallsStartedFrame` and `FunctionCallResultFrame`. Otherwise drop all four.

---

## Frames That Must Be Retained

These are consumed by TTS, transports, RTVI, and observers — independent of which aggregator or LLM is used.

| Frame | Who needs it |
|---|---|
| `LLMFullResponseStartFrame` / `LLMFullResponseEndFrame` | TTS reads `skip_tts` from these; RTVI uses them for bot-speaking events; simplified assistant aggregator needs them to bracket turns |
| `LLMTextFrame` | TTS service — this is the text it speaks |
| `InterruptionFrame` | TTS clears its queue on this; the entire interruption chain depends on it |
| `UserStartedSpeakingFrame` / `UserStoppedSpeakingFrame` | TTS pauses; transport notifies client; assistant aggregator tracks `_user_speaking` to avoid re-triggering LLM while user speaks |
| `BotStartedSpeakingFrame` / `BotStoppedSpeakingFrame` | Transports, audio mixers, deferred-push logic in the assistant aggregator |
| `TranscriptionFrame` | How STT delivers text to the simplified user aggregator (unless the LLM receives audio directly) |
| `VADUserStartedSpeakingFrame` / `VADUserStoppedSpeakingFrame` | Needed if VAD is in the pipeline — the turn controller consumes these |

---

## Minimal Aggregator Responsibilities

### Simplified User Aggregator

- Receive `TranscriptionFrame` → forward text to LLM directly (not via `LLMContextFrame`)
- VAD/turn logic → emit `UserStartedSpeakingFrame`, `UserStoppedSpeakingFrame`, `InterruptionFrame`
- Pass `StartFrame` / `EndFrame` / `CancelFrame` through

No context storage. No tool or message frames.

### Simplified Assistant Aggregator

- Track `LLMFullResponseStartFrame` / `LLMFullResponseEndFrame` → emit `on_assistant_turn_stopped` event with transcript
- Track `UserStartedSpeakingFrame` / `BotStoppedSpeakingFrame` state (avoid re-triggering LLM while user is speaking)
- Handle `InterruptionFrame` → reset the in-progress turn
- Optionally accumulate `TextFrame` / `LLMTextFrame` for the turn transcript in the turn-stopped event

No context storage. No function call handling. No tool or message frames.

### Minimum Frames the LLM Service Must Emit

| Frame | Reason |
|---|---|
| `LLMFullResponseStartFrame` | Brackets the response for TTS and RTVI |
| `LLMFullResponseEndFrame` | Signals end of response |
| `LLMTextFrame` | Text for TTS to speak |
| `InterruptionFrame` | When user speaks and should interrupt the bot (if not handled by user aggregator) |
