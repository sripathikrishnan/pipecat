# Pipecat ↔ OpenAI Agents SDK Bridge Design

## 1. Overview

This document describes the architecture for bridging Pipecat's real-time voice pipeline with the OpenAI Agents SDK. The goal is to allow bot developers to write their tool logic using the OpenAI Agents SDK (`@function_tool`, `Agent`, `Runner`) while Pipecat handles all the real-time voice machinery: VAD, STT, TTS, interruption handling, and turn management.

### Design philosophy

The OpenAI Agents SDK has a fundamentally different model from Google ADK:

- **Stateless history model**: The SDK operates on a plain `list[TResponseInputItem]` — a list of OpenAI Responses-API message dicts. There is no event graph, no `session.events` list, no rich `Event` wrapper. History is just a Python list of dicts.
- **No pause/resume for async tools**: The SDK does not have a `LongRunningFunctionTool` equivalent. Every `FunctionTool` executes to completion before the runner proceeds. There is no built-in mechanism to pause an invocation and resume it later with an externally produced result.
- **The runner is a multi-turn loop**: `Runner.run_streamed(agent, input_history)` takes the entire history as input, calls the LLM, executes tools, and runs multiple model turns until the agent produces a final output. The Pipecat bridge bypasses the runner's multi-turn loop by managing the history list directly and calling the runner with `max_turns=1` per bot turn.
- **History is caller-managed**: The SDK itself does not auto-append history to a session. The caller (Pipecat's aggregators) is responsible for maintaining the `input_history` list and passing it on each call to `Runner.run_streamed`. This is already how the `SingleAgentVoiceWorkflow` works.

### Key insight: Pipecat manages the history list

Because the SDK's history is just a `list[TResponseInputItem]`, the bridge can own that list and apply the same "dual event" pattern from the ADK bridge — tracking both what was generated and what the user actually heard — without any special SDK hooks. The bridge maintains two views:

1. **Model-facing history** (`_input_history`): What gets sent to the LLM each turn.
2. **Assistant aggregator tracking**: The assistant aggregator knows what text was spoken before any interruption and writes back the truncated "heard" version to the history list before the next LLM call.

There is no need for a `before_model_callback` hook because the bridge directly edits `_input_history` before calling `Runner.run_streamed`.

---

## 2. Components

### 2.1 `OpenAIAgentsLLMService`

Replaces Pipecat's standard `LLMService`. On receiving `LLMContextFrame`, it:

1. Converts the Pipecat `LLMContext` into the SDK's `list[TResponseInputItem]` format (or, in the stateless mode described below, uses the bridge's own history list directly).
2. Calls `Runner.run_streamed(agent, input_history, max_turns=1)`.
3. Streams `RawResponsesStreamEvent` → `TextFrame` and `LLMFullResponseStartFrame` / `LLMFullResponseEndFrame`.
4. Detects tool calls in `RunItemStreamEvent(name="tool_called")` and pushes `FunctionCallInProgressFrame`.
5. Executes tools inline (sync path) or defers to the async queue path.
6. On completion, extracts `result.to_input_list()` and passes it back to the assistant aggregator.

### 2.2 `OpenAIAgentsUserAggregator`

Subclass of Pipecat's `LLMUserAggregator`. Inherits all Pipecat turn detection machinery unchanged. The only customization is `push_aggregation`: instead of pushing an `LLMContextFrame` built from a Pipecat `LLMContext`, it appends the user message to the bridge's `_input_history` list, then pushes an `LLMContextFrame` (or a custom frame type) that carries the updated history list.

### 2.3 `OpenAIAgentsAssistantAggregator`

Lightweight lifecycle coordinator, analogous to the ADK bridge's `ADKAssistantAggregator`. Its responsibilities:

1. Track the text chunks spoken to the user (accumulate `TextFrame`s).
2. On `LLMFullResponseEndFrame` (no interruption): append the full assistant message to `_input_history`.
3. On `InterruptionFrame`: append only the spoken fragment to `_input_history`, and mark it with metadata so the reconciliation step (see §4) can fix the tool-call accounting.

Unlike the ADK bridge, this aggregator does **not** need to wrap anything in an event object — it directly writes a `{"role": "assistant", "content": "<heard text>"}` item into the history list.

---

## 3. Session / History Model

### 3.1 History representation

The OpenAI Agents SDK uses `list[TResponseInputItem]` as its history format. `TResponseInputItem` is `openai.types.responses.ResponseInputItemParam` — a TypedDict with a `"role"` key and various content fields. Examples:

```python
# User message
{"role": "user", "content": "What's the weather?"}

# Assistant message
{"role": "assistant", "content": "Let me check."}

# Tool call emitted by model
{"type": "function_call", "name": "get_weather", "arguments": '{"city": "Paris"}', "call_id": "call_abc"}

# Tool result
{"type": "function_call_output", "call_id": "call_abc", "output": "Sunny, 22°C"}
```

`RunResult.to_input_list()` returns the original input items plus all items generated during the run (model messages, tool calls, tool outputs) — ready to be passed as `input_history` to the next `Runner.run_streamed` call.

### 3.2 Stateless mode (recommended for Pipecat)

The bridge maintains a plain Python list:

```python
self._input_history: list[TResponseInputItem] = []
```

Each turn:
1. User aggregator appends the user message.
2. `OpenAIAgentsLLMService` calls `Runner.run_streamed(agent, self._input_history, max_turns=1)`.
3. After streaming completes, `result.to_input_list()` replaces `_input_history`.
4. The assistant aggregator may edit the last assistant message in `_input_history` to reflect what was actually heard (if interrupted).

### 3.3 Session persistence (optional)

If the bot developer wants cross-session persistence, they can pass a `Session` object to `Runner.run_streamed` (the SDK's `sqlite_session`, `redis_session`, etc.). The bridge does not block this. However, Pipecat's voice pipeline restarts every time, so the bridge should call `session.get_items()` at startup to restore history, then hand the session to the runner.

### 3.4 No server-managed conversation (`conversation_id`)

The bridge should NOT use `conversation_id` or `previous_response_id`. Those features route history management to OpenAI's server side and break Pipecat's ability to edit history for interruption handling. Use client-side history exclusively.

---

## 4. History Reconciliation

### 4.1 The problem

When the user interrupts mid-response, the model generated text "The weather in Paris is sunny and 22 degrees Celsius with light winds from the..." but the user only heard "The weather in Paris is sunny". We must not let the LLM believe it said the full sentence.

### 4.2 The solution (direct list edit)

Because the bridge owns `_input_history` directly, no callback hook is needed. The assistant aggregator simply edits the last assistant message in place before the next `Runner.run_streamed` call.

Steps:
1. During streaming: accumulate spoken text in `_heard_text`.
2. On interruption: the assistant aggregator keeps `_heard_text` but does NOT yet write to history (the streaming coroutine may still be running).
3. Once streaming is cancelled: append `{"role": "assistant", "content": _heard_text}` to `_input_history`.
4. If any tool calls were in-flight at interruption time: see §6.

### 4.3 Tool calls during interruption

If the model produced a `function_call` item but the user interrupted before the tool output was returned, the `_input_history` list will contain an orphaned tool call with no matching `function_call_output`. The OpenAI API requires that every `function_call` has a corresponding `function_call_output`.

**Resolution**: The bridge must scan `_input_history` for unmatched `function_call` entries and insert synthetic `function_call_output` items with `"output": "CANCELLED"` before the next LLM call. This is analogous to the ADK bridge's "manually append CANCELLED response" step.

---

## 5. Tool Flavors

The OpenAI Agents SDK has one tool execution model: every `FunctionTool` runs to completion inside `Runner.run_streamed`. There is no pause/resume primitive. The bridge must simulate async tool behavior on top of this by:

- For **sync tools** (`cancel_on_interruption=True` in Pipecat terms): let the runner execute the tool normally. Push `FunctionCallInProgressFrame(cancel_on_interruption=True)` for observability.
- For **async tools** (`cancel_on_interruption=False`): the runner still runs the tool, but the bridge's tool implementation returns immediately with a placeholder, then delivers the real result via an asyncio queue that the `OpenAIAgentsLLMService` checks before the next turn.

### 5.1 Sync tools

**Bot developer writes:**

```python
@function_tool
async def lookup_account(customer_id: str) -> str:
    """Look up account details."""
    return await db.get_account(customer_id)

agent = Agent(name="Support", tools=[lookup_account])
```

**Bridge behavior:**
1. Runner calls `lookup_account` normally.
2. Bridge pushes `FunctionCallInProgressFrame(cancel_on_interruption=True)` so Pipecat observers see the in-progress call.
3. If interrupted: the runner is cancelled (by cancelling its asyncio task), and any orphaned tool calls get synthetic CANCELLED results in history.
4. If completed normally: runner continues to the next model turn, bridge streams the text.

**Implementation note**: The bridge wraps the tool so it can intercept the call and push frames. The original function is called through unchanged.

### 5.2 Async tools

Async tools are tools that take significant time (e.g., file I/O, external API with SLA of seconds) and should not cancel when the user interrupts. Pipecat calls these `cancel_on_interruption=False` tools.

The SDK has no native support for this. The bridge uses a workaround:

**Bot developer writes:**

```python
@function_tool
async def run_long_analysis(data: str) -> str:
    """Run a slow analysis job."""
    return await slow_service.analyze(data)
```

The bot developer marks tools as async in the bridge registration:

```python
openai_bridge = OpenAIAgentsBridge(
    agent=agent,
    async_tools=["run_long_analysis"],   # tool names that should be async
)
```

**Bridge behavior:**

1. The bridge wraps `run_long_analysis` with a shim that:
   - Records the call (tool name, call_id, arguments) in `self._pending_async_calls`.
   - Starts the real function in a background `asyncio.Task` (outside the runner's control).
   - Returns immediately with a placeholder string `"__ASYNC_PENDING__"`.
2. Runner receives the placeholder, generates the next model turn which likely says "I've started the analysis, I'll let you know when it's done."
3. When the background task completes, it puts `(call_id, result)` onto `self._async_results_queue`.
4. On the next user turn (or proactively if the bot supports injecting messages), the bridge pops from `_async_results_queue`, finds the matching placeholder in `_input_history`, replaces `"__ASYNC_PENDING__"` with the real result, and triggers a new LLM call with a synthetic user prompt like `"The analysis is complete. Please tell the user."` — or uses a more elegant mechanism (see §7).

**Limitation**: This is a significant departure from the ADK bridge's first-class `LongRunningFunctionTool` support. The OpenAI Agents SDK has no pause/resume concept, so the bridge cannot cleanly interleave the tool result into the original turn's conversation flow. The result will always appear in a subsequent turn.

---

## 6. Interruption Handling

### 6.1 Sync tool interrupted

1. Pipecat pipeline receives `UserStartedSpeakingFrame` (or equivalent VAD signal).
2. Bridge cancels the `Runner.run_streamed` task via `asyncio.CancelledError`.
3. Bridge scans `_input_history` for all `function_call` items added in the cancelled turn that have no matching `function_call_output`.
4. For each orphaned call, appends `{"type": "function_call_output", "call_id": ..., "output": "CANCELLED"}`.
5. Appends heard text as the assistant message (if any text was spoken before the tool call).
6. Appends user's new message (from the interruption) to `_input_history`.
7. Starts a new `Runner.run_streamed` call.

### 6.2 Async tool interrupted

1. The background task is NOT cancelled (it was launched outside the runner's control).
2. The runner task IS cancelled, but the background async task continues.
3. The placeholder `"__ASYNC_PENDING__"` remains in `_input_history` for the async call.
4. Do NOT insert CANCELLED for async calls — they will deliver results later.
5. However, Pipecat MUST track which async calls are pending so they don't accumulate indefinitely if the user ends the session.

### 6.3 Interruption during text generation (no tools)

1. Runner task is cancelled mid-stream.
2. Assistant aggregator has accumulated `_heard_text = "The weather in Paris is"`.
3. Bridge appends `{"role": "assistant", "content": "The weather in Paris is"}` to `_input_history`.
4. No tool housekeeping needed.
5. New user turn starts.

---

## 7. Multiple Async Invocations

Since the SDK has no native async tool concept, the bridge must handle the case where multiple async tools are in flight simultaneously, possibly across multiple Pipecat turns.

### 7.1 Tracking pending calls

The bridge maintains:

```python
_pending_async_calls: dict[str, asyncio.Task] = {}
# key: call_id, value: background task
```

The `_input_history` list contains placeholder entries for in-flight async calls. The bridge knows which `call_id` values are still pending.

### 7.2 Delivering results

Option A (reactive, recommended for most bots): When a background task completes, push a `FunctionCallResultFrame` into the Pipecat pipeline. The `OpenAIAgentsLLMService` catches this, patches `_input_history`, and triggers a new bot turn. The user may be in the middle of speaking, so the bridge buffers incoming results and delivers them between user turns.

Option B (polled): Between turns, the `OpenAIAgentsLLMService` polls `_async_results_queue`. If results are waiting, it patches history and runs a synthetic LLM turn before waiting for the next user message. This is simpler to implement but may create awkward timing.

### 7.3 Session end cleanup

On `EndFrame`, the bridge cancels all tasks in `_pending_async_calls`. It does not need to clean up `_input_history` since the session is over.

### 7.4 Multiple pending calls from one turn

If the LLM called three async tools in one turn:
- Three tasks in `_pending_async_calls`.
- Three placeholder entries in `_input_history`.
- Results arrive independently.
- Each result delivery patches only its own placeholder.
- A new LLM turn only triggers when ALL three are resolved, OR the bot developer opts for partial delivery.

Recommendation: implement a "trigger after all resolved" policy first (simpler), add partial delivery as a later optimization.

---

## 8. Streaming

The SDK yields two event types relevant to the bridge:

### 8.1 `RawResponsesStreamEvent`

Wraps OpenAI Responses API stream events. The bridge watches for:

- `response.output_text.delta` → push `TextFrame(text=event.data.delta)`.
- `response.output_item.done` where `item.type == "function_call"` → push `FunctionCallInProgressFrame`.
- `response.completed` → push `LLMFullResponseEndFrame`.

### 8.2 `RunItemStreamEvent`

Higher-level events emitted by the runner:

- `name="message_output_created"` → `LLMFullResponseStartFrame`.
- `name="tool_called"` → tool is about to execute; push `FunctionCallInProgressFrame`.
- `name="tool_output"` → tool completed; push `FunctionCallResultFrame`.

The bridge should primarily use `RawResponsesStreamEvent` for text streaming (lower latency) and `RunItemStreamEvent` for tool lifecycle events (cleaner semantic signal).

### 8.3 Cancellation

`Runner.run_streamed` returns a `RunResultStreaming` whose `.run_loop_task` is an asyncio task. To cancel:

```python
streamed_result.run_loop_task.cancel()
await asyncio.gather(streamed_result.run_loop_task, return_exceptions=True)
```

The task raises `CancelledError`, which the bridge catches to trigger the interruption housekeeping described in §6.

---

## 9. History Management: What the SDK Owns vs. What the Bridge Owns

| Concern | SDK owns | Bridge owns |
|---|---|---|
| Message list storage | No — stateless | Yes — `_input_history: list[TResponseInputItem]` |
| Appending user messages | No | Yes — `OpenAIAgentsUserAggregator` |
| Appending assistant messages | No (only inside one run) | Yes — `OpenAIAgentsAssistantAggregator` |
| Appending tool call + output pairs | Yes — inside `Runner.run_streamed` | Bridge inspects and may patch post-run |
| Interruption truncation | No | Yes — assistant aggregator edits last message |
| Orphaned tool call cleanup | No | Yes — bridge inserts CANCELLED outputs |
| Session persistence | Optional — via `Session` protocol | Bridge wires `session=...` to runner |
| Multi-turn loop | Yes — but bridge uses `max_turns=1` | Bridge drives turns externally |

---

## 10. What the Bot Developer Sees

### 10.1 Minimal setup

```python
from agents import Agent, function_tool
from pipecat.services.openai_agents import OpenAIAgentsBridge, OpenAIAgentsLLMService
from pipecat.processors.aggregators.openai_agents import (
    OpenAIAgentsUserAggregator,
    OpenAIAgentsAssistantAggregator,
)

@function_tool
async def get_weather(city: str) -> str:
    """Get the weather for a city."""
    return await weather_api.get(city)

agent = Agent(
    name="Weather Bot",
    instructions="You help users with weather queries.",
    tools=[get_weather],
)

bridge = OpenAIAgentsBridge(agent=agent)
llm_service = OpenAIAgentsLLMService(bridge=bridge)

# Standard Pipecat pipeline wiring
context_aggregators = bridge.create_context_aggregators()
pipeline = Pipeline([
    transport.input(),
    stt,
    context_aggregators.user(),
    llm_service,
    tts,
    context_aggregators.assistant(),
    transport.output(),
])
```

### 10.2 Marking tools as async

```python
bridge = OpenAIAgentsBridge(
    agent=agent,
    async_tools=["run_long_analysis", "send_report"],
)
```

Async tools will continue running even if the user interrupts. Results arrive in a subsequent bot turn.

### 10.3 Context access in tools

Tools can access Pipecat-specific state via the `ToolContext` / `RunContextWrapper` first parameter:

```python
@function_tool
async def book_appointment(ctx: RunContextWrapper, date: str, time: str) -> str:
    """Book an appointment."""
    user_id = ctx.context.user_id   # set by the bot developer on context
    return await calendar.book(user_id, date, time)
```

The bridge passes a `context` object that the bot developer populates. The bridge itself does not need to be on the `RunContextWrapper` — it operates at a higher level.

---

## 11. Differences from the ADK Bridge

| Dimension | ADK Bridge | OpenAI Agents Bridge |
|---|---|---|
| History representation | ADK `Event` objects in `session.events` | Plain `list[TResponseInputItem]` |
| Dual event pattern | Two explicit events per turn (generated + heard) | Direct list edit — replace last message in-place |
| History reconciliation hook | `before_model_callback` | None needed — bridge edits list directly before calling runner |
| Async tools | First-class `LongRunningFunctionTool` with pause/resume | Workaround: background task + placeholder + deferred delivery |
| Tool suspension tracking | `session.state["_bridge_queue"]` | `_pending_async_calls: dict[str, Task]` on bridge |
| Multiple suspended invocations | ADK runner handles chronological ordering natively | Bridge must order result delivery manually |
| Streaming | ADK yields `Event` objects | SDK yields `RawResponsesStreamEvent` and `RunItemStreamEvent` |
| Runner control | ADK runner has explicit pause/resume | Bridge uses `max_turns=1` and cancellation |

The most significant architectural difference is the async tool story. The ADK bridge elegantly pauses and resumes the runner's invocation using ADK's first-class `LongRunningFunctionTool` + `session.state` queue. The OpenAI Agents bridge must simulate this with background tasks and deferred turn injection — a materially weaker pattern. If async tools are a critical use case, the bot developer should consider using the ADK bridge instead, or wait for the OpenAI Agents SDK to add a pause/resume primitive.

---

## 12. Open Questions and Gaps

1. **Proactive result injection**: When an async tool completes while the user is speaking, how does the bridge queue the result notification? The bridge needs a mechanism to inject a bot turn between user turns without blocking. This is not yet designed in detail.

2. **Partial async result delivery**: If the LLM called three async tools and one returns early, should the bridge trigger a partial LLM turn? The default "wait for all" policy may be too coarse for some use cases.

3. **Tool call ordering in history**: The OpenAI Responses API requires tool calls and their outputs to appear in sequence. If async results arrive out of order, the bridge must sort them correctly before appending to `_input_history`.

4. **Error handling in async tools**: If a background async task raises an exception, the bridge needs to insert an error string as the `function_call_output` and trigger a bot turn explaining the failure.

5. **`max_turns=1` assumption**: The bridge assumes one model turn per Pipecat bot turn. If the bot developer's agent uses handoffs or complex tool chains that need multiple turns to resolve, this assumption breaks. A configurable `max_turns` on the bridge may be needed.

6. **ChatCompletions vs. Responses API**: The bridge design above assumes the OpenAI Responses API (the SDK default). If the bot developer uses LiteLLM or another provider via `OpenAIChatCompletionsModel`, the item format changes slightly. The bridge should detect which model backend is in use and adapt history item serialization accordingly.

7. **Session `Session` protocol integration**: Pipecat persists conversation history differently from the SDK's `Session` protocol. The bridge should document clearly which persistence mechanism wins, and not activate the SDK's session unless the bot developer explicitly passes one.
