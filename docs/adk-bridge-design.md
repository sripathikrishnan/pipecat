# Pipecat ↔ Google ADK Bridge Design

## Overview

This document describes the design for bridging Pipecat's frame-based pipeline architecture with Google ADK's agent and session model.

The bridge allows bot developers to drop a fully-configured Google ADK agent into a Pipecat pipeline. Pipecat handles real-time voice I/O, VAD, speech-to-text, text-to-speech, and turn management. ADK handles LLM orchestration, tool execution, state management, and session persistence.

**Key philosophy**: ADK owns the session and conversation history. Pipecat owns real-time voice turn management and frame routing. The bridge coordinates between them without either side needing to know the other's internals.

---

## Components

```
Bot Developer Provides:
  ├── LlmAgent     — configured with tools, before_model_callback, instructions
  └── Runner       — configured as resumable (required for async tools)

Bridge provides:
  ├── ADKLLMContext          — wraps Session; owns "heard" event appending
  ├── ADKContextFrame        — carrier frame from aggregators to LLM service
  ├── ADKUserAggregator      — collects speech transcriptions, appends user Events
  ├── ADKAssistantAggregator — tracks bot speaking state, appends "heard" Events
  └── ADKLLMService          — drives ADK runner, maps ADK events → Pipecat frames
```

---

## The Session Event Model

### Dual Events Per Turn

Every bot response produces **two** events in `session.events`:

| Event | Author | Appended by | Purpose |
|---|---|---|---|
| Generated | agent name | ADK runner (automatic) | Full LLM output — permanent record |
| Heard | agent name | ADKAssistantAggregator | Text actually spoken before any interruption |

```
session.events after one full turn:
  Event(author="user",  content=[Part(text="user question")])
  Event(author="agent", content=[Part(text="full response")])          ← generated
  Event(author="agent", content=[...], custom_metadata={               ← heard
      "pipecat_heard": True, "interrupted": False })
```

On normal completion, heard text == generated text. On interruption, the heard event contains only the fragment the user actually heard before cutting in. Both events remain in the session permanently as a historical record. The LLM only ever sees the heard version.

### Why Two Events

ADK's runner appends the generated event **before** tool execution and before the user hears anything. The heard event is appended **after** speaking, when we know what fraction was actually delivered. A reconciliation callback (see below) ensures the LLM always reasons over what was heard, not what was generated, while the generated event serves as an audit record.

---

## History Reconciliation: `before_model_callback`

Registered on the `LlmAgent`. Runs before every LLM inference. It receives `llm_request` (which contains the full message history built from `session.events`) and `callback_context` (which has access to `session.events` directly).

**Algorithm:**

1. Walk `session.events` to find all heard events (tagged `custom_metadata["pipecat_heard"] == True`)
2. For each heard event at position N, the generated event is at position N-1
3. In `llm_request.contents`, for the content at position N-1:
   - Replace its text parts with the heard event's text
   - Keep any `function_call` parts from the generated content (those happened, regardless of interruption)
4. Remove the heard event's content from `llm_request.contents` (it was a patch, not a message)
5. Return `None` to let the LLM call proceed with modified contents

```python
async def reconcile_heard_events(callback_context, llm_request):
    events = callback_context.session.events
    heard_positions = {
        i: event for i, event in enumerate(events)
        if event.custom_metadata and event.custom_metadata.get("pipecat_heard")
    }
    if not heard_positions:
        return None

    new_contents = []
    skip_next = False
    for i, content in enumerate(llm_request.contents):
        if skip_next:
            skip_next = False
            continue
        if (i + 1) in heard_positions:
            heard = heard_positions[i + 1]
            heard_text = _extract_text(heard.content)
            # Keep function_call parts, replace text parts with heard text
            parts = [p for p in content.parts if p.function_call]
            if heard_text:
                parts.insert(0, Part(text=heard_text))
            new_contents.append(Content(role=content.role, parts=parts))
            skip_next = True
        else:
            new_contents.append(content)

    llm_request.contents = new_contents
    return None
```

---

## ADKUserAggregator

Keeps all of Pipecat's existing turn detection machinery unchanged:
- VAD controller and speech frames
- User turn controller (start/stop strategies)
- Mute strategies
- Idle detection
- Interruption broadcasting
- All `on_user_turn_*` events

Only `push_aggregation` changes — it appends an ADK `Event` instead of an OpenAI dict:

```python
async def push_aggregation(self) -> str:
    if not self._aggregation:
        return ""
    text = concatenate_aggregated_text(self._aggregation)
    await self.reset()
    self._context.session.events.append(Event(
        author="user",
        content=Content(role="user", parts=[Part(text=text)])
    ))
    await self.push_context_frame()   # pushes ADKContextFrame upstream
    return text
```

**Dropped from the standard LLMUserAggregator:**
- `LLMSetToolsFrame` / `LLMSetToolChoiceFrame` handling (tools live on the ADK agent, not the context)
- `LLMMessagesAppendFrame` / `LLMMessagesUpdateFrame` / `LLMMessagesTransformFrame` (no OpenAI-format message list to manipulate)
- Tool change messages (ADK doesn't need mid-conversation tool diffs injected as prompts)

---

## ADKAssistantAggregator

A lightweight lifecycle coordinator. ADK owns history management; the aggregator's only history responsibility is appending the heard event.

**Kept from the standard LLMAssistantAggregator:**
- `on_assistant_turn_started` / `on_assistant_turn_stopped` events
- Bot/user speaking state tracking (`_bot_speaking`, `_user_speaking`)
- `_resume_when_bot_stops` flag (deferred async tool resumption — replaces `_push_context_on_bot_stopped_speaking`)
- `LLMFullResponseStartFrame` / `LLMFullResponseEndFrame` handling for turn lifecycle
- `InterruptionFrame` handling

**Dropped:**
- All `context.add_message(...)` calls — ADK's runner appends generated events
- `FunctionCallInProgressFrame` / `FunctionCallResultFrame` / `FunctionCallCancelFrame` handling — owned by `ADKLLMService`
- `_function_calls_in_progress` tracking
- `_update_function_call_result`
- Context summarization (`LLMContextSummarizer`) — ADK has `EventCompaction`
- Thought aggregation writing to context

**The heard event path:**

```python
# Normal completion (LLMFullResponseEndFrame received):
async def push_aggregation(self) -> str:
    text = concatenate_aggregated_text(self._aggregation)
    if text:
        self._context.session.events.append(Event(
            author=self._agent_name,
            content=Content(role="model", parts=[Part(text=text)]),
            custom_metadata={"pipecat_heard": True, "interrupted": False}
        ))
    await self.reset()
    return text

# Interruption:
async def _handle_interruptions(self, frame: InterruptionFrame):
    text = concatenate_aggregated_text(self._aggregation)
    if text:
        self._context.session.events.append(Event(
            author=self._agent_name,
            content=Content(role="model", parts=[Part(text=text)]),
            custom_metadata={"pipecat_heard": True, "interrupted": True}
        ))
    await self.reset()
    await self._trigger_assistant_turn_stopped(interrupted=True)
    # Signal ADKLLMService to cancel its runner task
```

---

## ADKLLMService

Receives `ADKContextFrame`. Before each run, injects a completion queue into `session.state["_bridge_queue"]` for async tools to signal back.

### Tool Flavors

#### Sync Tool (`FunctionTool`, `is_long_running=False`)

ADK executes the tool internally and blocks until it completes before re-running the LLM. The bridge pushes Pipecat function call frames for pipeline observability (RTVI clients, event handlers) but does not control execution.

```
ADK yields: model event with function_call parts (no long_running_tool_ids)
  → push FunctionCallInProgressFrame(cancel_on_interruption=True) per call

ADK executes tool internally (bridge is blocked)

ADK yields: function_response event
  → push FunctionCallResultFrame

ADK yields: model text event
  → push TextFrame(s), LLMFullResponseStartFrame / EndFrame
```

**Interruption — sync tool running:**
1. Cancel the ADK runner task via `runner_task.cancel()`
2. For each in-flight sync call: manually append a CANCELLED function response Event to `session.events` (keeps session consistent for next turn)
3. Push `FunctionCallCancelFrame` per cancelled call
4. ADKAssistantAggregator appends heard event with partial spoken text

#### Async Tool (`LongRunningFunctionTool`, `is_long_running=True`)

The tool function returns quickly with an initial status. ADK detects `is_long_running=True`, sets `event.long_running_tool_ids`, and **pauses the invocation** (runner yields the event and stops). The bridge drives resumption.

```
ADK yields: model event with long_running_tool_ids set
  → push FunctionCallInProgressFrame(cancel_on_interruption=False) per async call
  → record in _suspended_invocations

ADK runner ends (invocation paused — no more events until resumed)

Bridge monitors: session.state["_bridge_queue"]

When (tool_call_id, result) arrives in queue:
  → push FunctionCallResultFrame to Pipecat pipeline
  → accumulate result in _suspended_invocations
  → if all pending calls for this invocation are done:
      if _bot_speaking: set _resume_when_bot_stops = True
      else: call runner.run_async(new_message=Content(role="user",
                parts=[FunctionResponse(id=call_id, name=fn_name, response=result)...]))
            ADK auto-resolves the invocation_id from the function_call_id

On BotStoppedSpeakingFrame + _resume_when_bot_stops:
  → call runner.run_async(new_message=accumulated_results)

Resumed runner yields: model text event
  → push TextFrame(s), LLMFullResponseStartFrame / EndFrame
```

**Interruption — async tool pending:**
The invocation is already paused. Do nothing — the monitoring loop stays active. When the tool result arrives and the user has stopped speaking, resume normally (Option A: always resume regardless of how many turns have elapsed).

### Suspended Invocation Tracking

```python
@dataclass
class SuspendedInvocation:
    invocation_id: str
    pending_calls: dict[str, str]  # tool_call_id → function_name
    results: list[Part]            # accumulated FunctionResponse parts

_suspended: dict[str, SuspendedInvocation]   # invocation_id → state
_tool_to_invocation: dict[str, str]          # tool_call_id → invocation_id
```

Multiple invocations can be suspended simultaneously (e.g., inv1 and inv2 each have a long-running tool). They are tracked independently and resumed independently when their tools complete. When inv1 eventually resumes, the LLM sees the full chronological session history including inv2's and inv3's events — this is ADK's native behavior and is correct. The LLM has full context of everything that transpired and can weave the late-arriving result into the conversation naturally.

---

## Async Tool Contract

Long-running tool functions access the bridge queue via `tool_context.state["_bridge_queue"]` and push `(tool_call_id, result)` when the background work completes. This is the only contract. Sync tool authors ignore it entirely.

```python
class BookRestaurantTool(LongRunningFunctionTool):
    async def run_async(self, *, args, tool_context):
        queue = tool_context.state["_bridge_queue"]
        asyncio.create_task(
            self._book_and_signal(args, tool_context.function_call_id, queue)
        )
        return {"status": "started", "message": "Booking in progress..."}

    async def _book_and_signal(self, args, call_id, queue):
        result = await actually_book_restaurant(args)
        await queue.put((call_id, result))
```

The queue is created fresh by `ADKLLMService` before each invocation and injected into `session.state`. It persists across the pause/resume cycle since `session.state` is preserved.

---

## Multiple Async Invocations

When inv1 is suspended (waiting for a long-running tool) and inv2, inv3 happen before inv1's tool completes:

- ADK includes **all session events chronologically** when building LLM history (filtered by branch, not invocation_id). This means when inv1 resumes, the LLM sees inv2's and inv3's exchanges as well.
- This is the **correct behavior** — the LLM has full context and can naturally acknowledge the late result: *"By the way, while we were discussing X, your booking at Chez Pierre just came through!"*
- The bridge tracks all suspended invocations independently. Each resumes when its own tools complete, regardless of what other turns have happened in between.
- When an invocation has multiple parallel long-running tools, the bridge waits for **all** of them to complete before resuming — one LLM re-run for the whole batch (avoids N intermediate responses for N parallel tools).

---

## What the Bot Developer Sees

```python
agent = LlmAgent(
    model="gemini-2.0-flash",
    instruction="You are a helpful assistant.",
    tools=[
        FunctionTool(get_weather),                 # sync — blocks until result
        BookRestaurantTool(),                      # async — continues while running
    ],
    before_model_callback=bridge.reconcile_heard_events,  # provided by bridge
)

runner = Runner(
    agent=agent,
    session_service=InMemorySessionService(),
    resumability_config=ResumabilityConfig(is_resumable=True),  # required for async tools
)

bridge = ADKLLMService(agent=agent, runner=runner)
```

**Tool flavor is expressed through ADK tool type alone.** No Pipecat-specific registration API. No `cancel_on_interruption` parameter. The bridge infers sync vs async from `tool.is_long_running`.

The `before_model_callback` is provided by the bridge and must be registered on the agent. It is the mechanism that ensures the LLM sees heard text rather than generated text.

The `ResumabilityConfig` is required if any async (`LongRunningFunctionTool`) tools are used. Without it, `should_pause_invocation()` always returns False and long-running tools behave as sync tools (ADK awaits them).
