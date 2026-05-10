# voqalcloud — Turn Traceability for Pipecat

## The Problem

Pipecat has no consistent identifier that flows from a user utterance through to the bot's committed response. The TTS service generates its own opaque UUID when `LLMFullResponseStartFrame` arrives, completely disconnected from which user turn triggered it. When `on_assistant_turn_stopped` fires you know *what* was said but not *which turn* it belongs to.

## The Solution

voqalcloud assigns a `turn_id` at the moment a user turn starts and threads it through every pipeline hop as a **frame attribute**, never as shared global state. At the end, `BotTurnCompletedFrame` and `AssistantTurnStoppedMessage.turn_id` give you unambiguous traceability.

## Propagation Chain

```
TurnAwareUserAggregator
  _turn_id = uuid4()              ← instance var (safe: sequential)
  UserTurnStartedFrame.turn_id    ← emitted downstream for observers
  LLMContextFrame.turn_id = t1   ← stamped on the frame
         │
         ▼  (frame travels downstream)
TurnAwareLLMMixin (on your LLM service)
  _current_turn_id = t1           ← set from DOWNSTREAM LLMContextFrame only
  LLMFullResponseStartFrame.turn_id = t1  ← stamped inline in push_frame
  LLMTextFrame.turn_id            = t1
  LLMThoughtTextFrame.turn_id     = t1
  LLMThoughtStartFrame.turn_id    = t1
  LLMThoughtEndFrame.turn_id      = t1
  LLMFullResponseEndFrame.turn_id = t1
         │
         ▼  (frame travels downstream)
TurnAwareTTSMixin (on your TTS service)
  _pending_turn_id = t1           ← TTSAudioRawFrame / TTSTextFrame carry t1
         │
         ▼  (frame travels downstream)
TurnAwareAssistantAggregator
  _turn_id = frame.turn_id        ← read from LLMFullResponseStartFrame
  AssistantTurnStoppedMessage.turn_id = t1
  BotTurnCompletedFrame(turn_id=t1, text=..., interrupted=...)
```

### Why instance variables are safe everywhere

Every standard pipecat LLM service (OpenAI, Anthropic, etc.) processes `LLMContextFrame` **inline** inside `process_frame` — it `await`s the full streaming response before returning. The processor's input queue loop blocks on `process_frame`, so a second `LLMContextFrame` can never arrive until the first is complete. There are no concurrent LLM invocations.

`TurnAwareLLMMixin`, `TurnAwareUserAggregator`, `TurnAwareTTSMixin`, and `TurnAwareAssistantAggregator` are all sequential processors — at most one turn is active at a time in each. A plain `self._current_turn_id` instance variable is correct and sufficient throughout.

### Direction gate on context frames

`TurnAwareLLMMixin.process_frame` only updates `_current_turn_id` from **downstream** `LLMContextFrame`s. Upstream `LLMContextFrame`s — emitted by the assistant aggregator for function-call result follow-ups — must not overwrite the active turn's ID with stale state.

### Non-VAD paths (greetings, LLMRunFrame)

When `_on_user_turn_started` is never called (startup greeting, `LLMRunFrame`, `LLMMessagesAppendFrame(run_llm=True)`), `TurnAwareUserAggregator._get_context_frame` lazy-mints a one-shot UUID:

```python
turn_id = self._turn_id or str(uuid.uuid4())
```

Every LLM invocation is traceable, even ones not triggered by a user utterance.

### Function-call follow-up traceability

When the LLM makes a tool call, the assistant aggregator pushes an upstream `LLMContextFrame` so the pipeline can run the follow-up LLM inference. `TurnAwareAssistantAggregator._get_context_frame` stamps `self._turn_id` onto that frame, so the follow-up response carries the same `turn_id` as the original user turn.

## Usage

```python
from voqalcloud import TurnAwareContextAggregatorPair
from voqalcloud.services import TurnAwareLLMMixin, TurnAwareTTSMixin

# Compose your provider classes with the mixins (mixin FIRST in MRO)
class MyLLM(TurnAwareLLMMixin, OpenAILLMService):
    pass

class MyTTS(TurnAwareTTSMixin, ElevenLabsTTSService):
    pass

# Create aggregator pair
aggs = TurnAwareContextAggregatorPair(llm_context)

# Wire pipeline
pipeline = Pipeline([
    transport.input(),
    stt,
    aggs.user(),
    MyLLM(...),
    MyTTS(...),
    transport.output(),
    aggs.assistant(),
])

# Handle completed turns via event handler
@aggs.assistant().event_handler("on_assistant_turn_stopped")
async def on_turn(agg, message):
    print(f"turn={message.turn_id} text={message.content!r} interrupted={message.interrupted}")

# Or observe BotTurnCompletedFrame in a pipeline observer
```

## Package Layout

```
voqalcloud/
├── __init__.py                   TurnAwareContextAggregatorPair (drop-in for
│                                 LLMContextAggregatorPair)
├── frames/frames.py              UserTurnStartedFrame, BotTurnCompletedFrame,
│                                 get_turn_id / set_turn_id helpers
├── aggregators/
│   ├── user_aggregator.py        TurnAwareUserAggregator — generates turn_id
│   └── assistant_aggregator.py  TurnAwareAssistantAggregator — closes the loop
└── services/
    ├── llm_service.py            TurnAwareLLMMixin — stamps all 6 LLM frame types
    └── tts_service.py            TurnAwareTTSMixin — bridges turn_id into TTS
                                  audio context ID
```

## Frames emitted by voqalcloud

| Frame | Direction | When |
|-------|-----------|------|
| `UserTurnStartedFrame(turn_id, timestamp)` | Downstream | User turn begins |
| `BotTurnCompletedFrame(turn_id, text, interrupted, timestamp)` | Downstream | Bot response committed to context |

`BotTurnCompletedFrame.text` holds exactly what was saved to the LLM context — the full response if uninterrupted, or the partial prefix if the turn was cut short.

## Known Limitations

- **`dataclasses.asdict()` drops `turn_id`**: `AssistantTurnStoppedMessage` is a plain `@dataclass` and `turn_id` is attached as a dynamic attribute. `asdict()` only serializes declared fields, so it silently omits `turn_id`. Consumers that need to serialize the turn ID should use `BotTurnCompletedFrame` instead.

- **`on_assistant_thought` event has no `turn_id`**: The `AssistantThoughtMessage` emitted by `on_assistant_thought` has no extension point for `turn_id`. The underlying `LLMThoughtTextFrame` and `LLMThoughtStartFrame`/`LLMThoughtEndFrame` frames do carry `turn_id` (stamped by `TurnAwareLLMMixin`), so pipeline observers can correlate thought frames — but the aggregator's thought event cannot.

## What is NOT changed

- Zero modifications to any pipecat source file.
- All existing pipecat frame types are used as-is; `turn_id` is added as a dynamic attribute on unfrozen dataclass instances via `setattr`.
- Provider-specific LLM and TTS services are unchanged; the mixins compose via Python MRO.
