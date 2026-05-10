#
# tests/test_voqalcloud.py
#
# Comprehensive test suite for the voqalcloud package — turn_id propagation
# end-to-end through the pipecat pipeline.
#

import asyncio
import unittest
import uuid

from pipecat.frames.frames import (
    ErrorFrame,
    FunctionCallCancelFrame,
    FunctionCallInProgressFrame,
    FunctionCallResultFrame,
    FunctionCallsStartedFrame,
    InterruptionFrame,
    LLMContextAssistantTimestampFrame,
    LLMContextFrame,
    LLMFullResponseEndFrame,
    LLMFullResponseStartFrame,
    LLMRunFrame,
    LLMTextFrame,
    LLMThoughtTextFrame,
    SpeechControlParamsFrame,
    TextFrame,
    TranscriptionFrame,
    VADUserStartedSpeakingFrame,
    VADUserStoppedSpeakingFrame,
)
from pipecat.pipeline.pipeline import Pipeline
from pipecat.processors.aggregators.llm_context import LLMContext
from pipecat.processors.aggregators.llm_response_universal import (
    AssistantTurnStoppedMessage,
    LLMAssistantAggregator,
    LLMUserAggregatorParams,
)
from pipecat.processors.frame_processor import FrameDirection, FrameProcessor
from pipecat.tests.utils import SleepFrame, run_test
from pipecat.turns.user_stop import SpeechTimeoutUserTurnStopStrategy
from pipecat.turns.user_turn_strategies import UserTurnStrategies

from voqalcloud.aggregators.assistant_aggregator import TurnAwareAssistantAggregator
from voqalcloud.aggregators.user_aggregator import TurnAwareUserAggregator
from voqalcloud.frames.frames import (
    BotTurnCompletedFrame,
    UserTurnStartedFrame,
    get_turn_id,
    set_turn_id,
)
from voqalcloud.services.llm_service import TurnAwareLLMMixin
from voqalcloud.services.tts_service import TurnAwareTTSMixin


TRANSCRIPTION_TIMEOUT = 0.1


# ---------------------------------------------------------------------------
# Helper processors
# ---------------------------------------------------------------------------


class FakeLLMService(FrameProcessor):
    """Simulates a real pipecat LLM service: processes LLMContextFrame INLINE
    (no create_task) — exactly as OpenAI/Anthropic/etc. do it.

    Real pattern (from OpenAI base_llm.py):
        if isinstance(frame, LLMContextFrame):
            await self.push_frame(LLMFullResponseStartFrame())
            await self._process_context(context)   # blocking stream
            await self.push_frame(LLMFullResponseEndFrame())
    """

    def __init__(self, frames_to_emit=None, **kwargs):
        super().__init__(**kwargs)
        self._frames_to_emit = frames_to_emit or [
            LLMFullResponseStartFrame(),
            LLMTextFrame(text="Hello"),
            LLMFullResponseEndFrame(),
        ]

    async def process_frame(self, frame, direction):
        await super().process_frame(frame, direction)
        if isinstance(frame, LLMContextFrame):
            # Inline — just like every real pipecat LLM service
            for f in self._frames_to_emit:
                await self.push_frame(f)
        else:
            await self.push_frame(frame, direction)


class TurnAwareFakeLLM(TurnAwareLLMMixin, FakeLLMService):
    pass


class FakeTTSService(FrameProcessor):
    """Simulates TTSService: on LLMFullResponseStartFrame, calls create_context_id()
    exactly as the real TTSService does and stores the result in _turn_context_id."""

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self._turn_context_id: str | None = None
        self._context_id_calls: list[str | None] = []

    async def process_frame(self, frame, direction):
        await super().process_frame(frame, direction)
        if isinstance(frame, LLMFullResponseStartFrame):
            self._turn_context_id = self.create_context_id()
            self._context_id_calls.append(self._turn_context_id)
        await self.push_frame(frame, direction)

    def create_context_id(self) -> str:
        return str(uuid.uuid4())


class TurnAwareFakeTTS(TurnAwareTTSMixin, FakeTTSService):
    pass


# ---------------------------------------------------------------------------
# 1. TestFrameHelpers
# ---------------------------------------------------------------------------


class TestFrameHelpers(unittest.IsolatedAsyncioTestCase):
    def test_get_turn_id_default_none(self):
        frame = LLMTextFrame(text="hello")
        self.assertIsNone(get_turn_id(frame))

    def test_set_and_get_turn_id(self):
        frame = LLMTextFrame(text="hello")
        set_turn_id(frame, "t1")
        self.assertEqual(get_turn_id(frame), "t1")

    def test_set_turn_id_overwrites(self):
        frame = LLMTextFrame(text="hello")
        set_turn_id(frame, "t1")
        set_turn_id(frame, "t2")
        self.assertEqual(get_turn_id(frame), "t2")

    def test_set_on_one_frame_does_not_affect_another(self):
        frame_a = LLMTextFrame(text="a")
        frame_b = LLMTextFrame(text="b")
        set_turn_id(frame_a, "t1")
        self.assertIsNone(get_turn_id(frame_b))

    def test_set_turn_id_to_none(self):
        frame = LLMTextFrame(text="hello")
        set_turn_id(frame, "t1")
        set_turn_id(frame, None)
        self.assertIsNone(get_turn_id(frame))


# ---------------------------------------------------------------------------
# 2. TestLLMMixinFrameStamping
# ---------------------------------------------------------------------------


class TestLLMMixinFrameStamping(unittest.IsolatedAsyncioTestCase):
    async def test_all_frame_types_stamped(self):
        """All LLM downstream frame types carry the correct turn_id, including function call frames."""
        t1 = "stamp-t1"
        context = LLMContext()

        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(),
            LLMTextFrame(text="hi"),
            LLMThoughtTextFrame(text="thinking"),
            LLMFullResponseEndFrame(),
            FunctionCallsStartedFrame(function_calls=[]),
            FunctionCallInProgressFrame(function_name="search", tool_call_id="tc1", arguments={}),
            FunctionCallResultFrame(function_name="search", tool_call_id="tc1", arguments={}, result="ok"),
            FunctionCallCancelFrame(function_name="search", tool_call_id="tc1"),
        ])

        ctx_frame = LLMContextFrame(context=context)
        set_turn_id(ctx_frame, t1)

        expected_down = [
            LLMFullResponseStartFrame,
            LLMTextFrame,
            LLMThoughtTextFrame,
            LLMFullResponseEndFrame,
            FunctionCallsStartedFrame,
            FunctionCallInProgressFrame,
            FunctionCallResultFrame,
            FunctionCallCancelFrame,
        ]
        (down, _) = await run_test(
            llm,
            frames_to_send=[ctx_frame],
            expected_down_frames=expected_down,
        )

        for frame in down:
            self.assertEqual(
                get_turn_id(frame),
                t1,
                f"{frame.__class__.__name__} has turn_id={get_turn_id(frame)!r}, expected {t1!r}",
            )

    async def test_no_turn_id_on_context_means_no_stamp(self):
        """When LLMContextFrame has no turn_id, downstream frames get None."""
        context = LLMContext()
        ctx_frame = LLMContextFrame(context=context)  # no set_turn_id

        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(),
            LLMTextFrame(text="hi"),
            LLMFullResponseEndFrame(),
        ])

        expected_down = [LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame]
        (down, _) = await run_test(llm, frames_to_send=[ctx_frame], expected_down_frames=expected_down)

        for frame in down:
            self.assertIsNone(
                get_turn_id(frame),
                f"{frame.__class__.__name__} should have no turn_id",
            )

    async def test_non_listed_frame_types_not_stamped(self):
        """A TextFrame pushed by the LLM is NOT stamped — only _LLM_DOWNSTREAM_FRAMES are."""
        t1 = "stamp-gate"
        context = LLMContext()

        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(),
            TextFrame(text="plain"),  # not in _LLM_DOWNSTREAM_FRAMES
            LLMFullResponseEndFrame(),
        ])

        ctx_frame = LLMContextFrame(context=context)
        set_turn_id(ctx_frame, t1)

        (down, _) = await run_test(
            llm,
            frames_to_send=[ctx_frame],
            expected_down_frames=[LLMFullResponseStartFrame, TextFrame, LLMFullResponseEndFrame],
        )

        plain_texts = [f for f in down if type(f) is TextFrame]
        self.assertEqual(len(plain_texts), 1)
        self.assertIsNone(get_turn_id(plain_texts[0]), "TextFrame must not be stamped")

    async def test_upstream_frames_not_stamped(self):
        """ErrorFrame pushed UPSTREAM is not stamped even when _current_turn_id is set.

        Verifies the `direction == DOWNSTREAM` guard in push_frame.
        """
        t1 = "upstream-test"
        context = LLMContext()

        class ErrorPushingFakeLLM(TurnAwareLLMMixin, FrameProcessor):
            async def process_frame(self, frame, direction):
                await super().process_frame(frame, direction)
                if isinstance(frame, LLMContextFrame):
                    await self.push_frame(ErrorFrame(error="oops"), FrameDirection.UPSTREAM)
                    await self.push_frame(LLMFullResponseStartFrame())
                    await self.push_frame(LLMFullResponseEndFrame())
                else:
                    await self.push_frame(frame, direction)

        llm = ErrorPushingFakeLLM()
        ctx_frame = LLMContextFrame(context=context)
        set_turn_id(ctx_frame, t1)

        (down, up) = await run_test(
            llm,
            frames_to_send=[ctx_frame],
            expected_down_frames=[LLMFullResponseStartFrame, LLMFullResponseEndFrame],
            expected_up_frames=[ErrorFrame],
        )

        error_frames = [f for f in up if isinstance(f, ErrorFrame)]
        self.assertEqual(len(error_frames), 1)
        self.assertIsNone(
            get_turn_id(error_frames[0]),
            "ErrorFrame pushed upstream must not receive a turn_id",
        )

    async def test_stale_turn_id_not_propagated_to_next_turn(self):
        """After turn t1, a context with no turn_id produces frames with turn_id=None."""
        context = LLMContext()

        llm1 = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="t1"), LLMFullResponseEndFrame()
        ])
        ctx1 = LLMContextFrame(context=context)
        set_turn_id(ctx1, "stale")

        (down1, _) = await run_test(
            llm1, frames_to_send=[ctx1],
            expected_down_frames=[LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame],
        )
        for f in down1:
            self.assertEqual(get_turn_id(f), "stale")

        llm2 = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="t2"), LLMFullResponseEndFrame()
        ])
        ctx2 = LLMContextFrame(context=context)
        # no set_turn_id

        (down2, _) = await run_test(
            llm2, frames_to_send=[ctx2],
            expected_down_frames=[LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame],
        )
        for f in down2:
            self.assertIsNone(get_turn_id(f), f"{f.__class__.__name__} must not carry stale ID")

    async def test_two_sequential_turns_on_same_instance(self):
        """Two back-to-back LLMContextFrames on the same instance each get the right turn_id.

        This is the primary correctness test for the sequential execution model:
        _current_turn_id is set before process_frame calls push_frame.
        """
        context = LLMContext()

        class MultiTurnFakeLLM(TurnAwareLLMMixin, FrameProcessor):
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                self._turn_count = 0

            async def process_frame(self, frame, direction):
                await super().process_frame(frame, direction)
                if isinstance(frame, LLMContextFrame):
                    self._turn_count += 1
                    await self.push_frame(LLMFullResponseStartFrame())
                    await self.push_frame(LLMTextFrame(text=f"response-{self._turn_count}"))
                    await self.push_frame(LLMFullResponseEndFrame())
                else:
                    await self.push_frame(frame, direction)

        llm = MultiTurnFakeLLM()
        ctx1 = LLMContextFrame(context=context)
        set_turn_id(ctx1, "seq-t1")
        ctx2 = LLMContextFrame(context=context)
        set_turn_id(ctx2, "seq-t2")

        expected = [
            LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame,
            LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame,
        ]
        (down, _) = await run_test(llm, frames_to_send=[ctx1, ctx2], expected_down_frames=expected)

        for f in down[:3]:
            self.assertEqual(get_turn_id(f), "seq-t1", f"{f.__class__.__name__} should carry seq-t1")
        for f in down[3:]:
            self.assertEqual(get_turn_id(f), "seq-t2", f"{f.__class__.__name__} should carry seq-t2")

    async def test_two_instances_do_not_share_state(self):
        """Two TurnAwareLLMMixin instances are independent — no shared module-level state."""
        context = LLMContext()

        llm_a = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="a"), LLMFullResponseEndFrame()
        ])
        llm_b = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="b"), LLMFullResponseEndFrame()
        ])

        ctx_a = LLMContextFrame(context=context)
        set_turn_id(ctx_a, "instance-A")
        ctx_b = LLMContextFrame(context=context)
        set_turn_id(ctx_b, "instance-B")

        expected = [LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame]
        (down_a, _) = await run_test(llm_a, frames_to_send=[ctx_a], expected_down_frames=expected)
        (down_b, _) = await run_test(llm_b, frames_to_send=[ctx_b], expected_down_frames=expected)

        for f in down_a:
            self.assertEqual(get_turn_id(f), "instance-A")
        for f in down_b:
            self.assertEqual(get_turn_id(f), "instance-B")


# ---------------------------------------------------------------------------
# 3. TestAssistantAggregator
# ---------------------------------------------------------------------------


class TestAssistantAggregator(unittest.IsolatedAsyncioTestCase):
    async def test_handle_llm_start_captures_turn_id(self):
        """_handle_llm_start stores turn_id; BotTurnCompletedFrame confirms it."""
        t1 = "t-capture"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        expected_down = [LLMContextFrame, LLMContextAssistantTimestampFrame, BotTurnCompletedFrame]
        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="hi"), LLMFullResponseEndFrame()],
            expected_down_frames=expected_down,
        )

        bot_frame = next(f for f in down if isinstance(f, BotTurnCompletedFrame))
        self.assertEqual(bot_frame.turn_id, t1)
        self.assertIsNone(agg._turn_id)

    async def test_bot_turn_completed_content(self):
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, "t-content")

        expected_down = [LLMContextFrame, LLMContextAssistantTimestampFrame, BotTurnCompletedFrame]
        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="Hello there"), LLMFullResponseEndFrame()],
            expected_down_frames=expected_down,
        )

        bot_frame = next(f for f in down if isinstance(f, BotTurnCompletedFrame))
        self.assertEqual(bot_frame.text, "Hello there")

    async def test_event_handler_receives_turn_id_on_message(self):
        """on_assistant_turn_stopped message carries turn_id as dynamic attribute."""
        t1 = "event-t1"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        messages_received: list = []

        @agg.event_handler("on_assistant_turn_stopped")
        async def on_stopped(aggregator, message):
            messages_received.append(message)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="hi"), LLMFullResponseEndFrame()],
        )

        self.assertEqual(len(messages_received), 1)
        msg = messages_received[0]
        self.assertTrue(hasattr(msg, "turn_id"), "message must have turn_id dynamic attr")
        self.assertEqual(msg.turn_id, t1)

    async def test_turn_id_reset_after_turn(self):
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, "t-reset")

        await run_test(agg, frames_to_send=[start_frame, LLMFullResponseEndFrame()])
        self.assertIsNone(agg._turn_id)

    async def test_interrupted_turn_carries_interrupted_true(self):
        t1 = "t-interrupt"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="partial"), SleepFrame(), InterruptionFrame()],
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 1)
        self.assertTrue(bot_frames[0].interrupted)
        self.assertEqual(bot_frames[0].turn_id, t1)

    async def test_no_bot_turn_completed_without_turn_id(self):
        """The `if turn_id:` guard prevents BotTurnCompletedFrame when turn_id is None."""
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()  # no set_turn_id

        expected_down = [LLMContextFrame, LLMContextAssistantTimestampFrame]
        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="hello"), LLMFullResponseEndFrame()],
            expected_down_frames=expected_down,
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 0)

    async def test_three_sequential_turns_unique_ids(self):
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        t_ids = ["seq-t1", "seq-t2", "seq-t3"]
        frames_to_send = []
        for t_id in t_ids:
            start_frame = LLMFullResponseStartFrame()
            set_turn_id(start_frame, t_id)
            frames_to_send.extend([
                start_frame, LLMTextFrame(text=f"resp-{t_id}"), LLMFullResponseEndFrame()
            ])

        (down, _) = await run_test(agg, frames_to_send=frames_to_send)

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 3)
        self.assertEqual([f.turn_id for f in bot_frames], t_ids)

    async def test_both_event_and_frame_carry_same_turn_id(self):
        t1 = "t-ordering"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        event_received: list = []

        @agg.event_handler("on_assistant_turn_stopped")
        async def on_stopped(aggregator, message):
            event_received.append(getattr(message, "turn_id", None))

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        expected_down = [LLMContextFrame, LLMContextAssistantTimestampFrame, BotTurnCompletedFrame]
        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="test"), LLMFullResponseEndFrame()],
            expected_down_frames=expected_down,
        )

        await asyncio.sleep(0.05)  # let background event tasks run

        self.assertEqual(len(event_received), 1)
        self.assertEqual(event_received[0], t1)
        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(bot_frames[0].turn_id, t1)


# ---------------------------------------------------------------------------
# 4. TestAssistantAggregatorBehavioralParity
# ---------------------------------------------------------------------------


class TestAssistantAggregatorBehavioralParity(unittest.IsolatedAsyncioTestCase):
    async def test_non_voqalcloud_frames_identical_to_parent(self):
        """TurnAwareAssistantAggregator produces identical non-voqalcloud frames as parent.

        Catches silent drift when pipecat changes _trigger_assistant_turn_stopped.
        """
        context_parent = LLMContext()
        context_child = LLMContext()

        parent_agg = LLMAssistantAggregator(context_parent)
        child_agg = TurnAwareAssistantAggregator(context_child)

        frames_to_send = [
            LLMFullResponseStartFrame(),
            LLMTextFrame(text="Hello world"),
            LLMFullResponseEndFrame(),
        ]

        (down_parent, _) = await run_test(parent_agg, frames_to_send=list(frames_to_send))
        (down_child, _) = await run_test(child_agg, frames_to_send=list(frames_to_send))

        child_without_voqalcloud = [f for f in down_child if not isinstance(f, BotTurnCompletedFrame)]

        parent_types = [type(f) for f in down_parent]
        child_types = [type(f) for f in child_without_voqalcloud]

        self.assertEqual(
            parent_types,
            child_types,
            f"Frame types diverged.\nParent: {parent_types}\nChild:  {child_types}",
        )


# ---------------------------------------------------------------------------
# 5. TestUserAggregator
# ---------------------------------------------------------------------------


class TestUserAggregator(unittest.IsolatedAsyncioTestCase):
    def _make_user_agg(self, context):
        return TurnAwareUserAggregator(
            context,
            params=LLMUserAggregatorParams(
                user_turn_strategies=UserTurnStrategies(
                    stop=[SpeechTimeoutUserTurnStopStrategy(user_speech_timeout=TRANSCRIPTION_TIMEOUT)],
                ),
            ),
        )

    async def test_user_turn_started_frame_matches_llm_context_frame(self):
        """UserTurnStartedFrame and LLMContextFrame carry the same turn_id."""
        context = LLMContext()
        user_agg = self._make_user_agg(context)

        frames_to_send = [
            VADUserStartedSpeakingFrame(),
            TranscriptionFrame(text="Hello bot", user_id="", timestamp="now"),
            SleepFrame(),
            VADUserStoppedSpeakingFrame(),
            SleepFrame(sleep=TRANSCRIPTION_TIMEOUT + 0.1),
        ]

        (down, _) = await run_test(user_agg, frames_to_send=frames_to_send)

        user_started = [f for f in down if isinstance(f, UserTurnStartedFrame)]
        context_frames = [f for f in down if isinstance(f, LLMContextFrame)]

        self.assertEqual(len(user_started), 1)
        self.assertEqual(len(context_frames), 1)

        user_id = user_started[0].turn_id
        ctx_id = get_turn_id(context_frames[0])

        self.assertIsNotNone(user_id)
        self.assertIsNotNone(ctx_id)
        self.assertEqual(user_id, ctx_id)

    async def test_each_turn_gets_unique_uuid4(self):
        """Two successive turns produce distinct valid UUID4 turn_ids."""
        context = LLMContext()
        user_agg = self._make_user_agg(context)

        turn_delay = TRANSCRIPTION_TIMEOUT + 0.15
        frames_to_send = []
        for i in range(2):
            frames_to_send.extend([
                VADUserStartedSpeakingFrame(),
                TranscriptionFrame(text=f"turn {i}", user_id="", timestamp="now"),
                SleepFrame(),
                VADUserStoppedSpeakingFrame(),
                SleepFrame(sleep=turn_delay),
            ])

        (down, _) = await run_test(user_agg, frames_to_send=frames_to_send)

        ids = [f.turn_id for f in down if isinstance(f, UserTurnStartedFrame)]
        self.assertEqual(len(ids), 2)
        self.assertNotEqual(ids[0], ids[1])

        for t_id in ids:
            try:
                parsed = uuid.UUID(t_id, version=4)
                self.assertEqual(str(parsed), t_id)
            except (ValueError, AttributeError) as e:
                self.fail(f"turn_id {t_id!r} is not a valid UUID4: {e}")


# ---------------------------------------------------------------------------
# 6. TestTTSMixin
# ---------------------------------------------------------------------------


class TestTTSMixin(unittest.IsolatedAsyncioTestCase):
    async def test_create_context_id_returns_turn_id_during_start_frame(self):
        t1 = "tts-t1"
        tts = TurnAwareFakeTTS()

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])
        self.assertEqual(tts._turn_context_id, t1)

    async def test_pending_turn_id_cleared_after_normal_processing(self):
        t1 = "tts-clear"
        tts = TurnAwareFakeTTS()

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])
        self.assertIsNone(tts._pending_turn_id)

    async def test_fallthrough_to_super_when_no_turn_id(self):
        """When LLMFullResponseStartFrame has no turn_id, create_context_id falls through to super()."""
        tts = TurnAwareFakeTTS()
        start_frame = LLMFullResponseStartFrame()  # no set_turn_id

        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])

        self.assertIsNotNone(tts._turn_context_id)
        try:
            uuid.UUID(tts._turn_context_id)
        except (ValueError, TypeError):
            self.fail(f"Expected UUID from super(), got {tts._turn_context_id!r}")

    async def test_pending_turn_id_cleared_on_exception(self):
        """If super().process_frame raises, try/finally clears _pending_turn_id.

        This was previously xfail. The try/finally fix makes it pass.
        """
        class ExceptionBase(FrameProcessor):
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                self._turn_context_id = None

            async def process_frame(self, frame, direction):
                await super().process_frame(frame, direction)
                if isinstance(frame, LLMFullResponseStartFrame):
                    raise RuntimeError("simulated TTS failure")
                await self.push_frame(frame, direction)

            def create_context_id(self) -> str:
                return "uuid-from-base"

        class TurnAwareExceptionTTS(TurnAwareTTSMixin, ExceptionBase):
            pass

        tts = TurnAwareExceptionTTS()
        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, "t-exc")

        try:
            await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[])
        except Exception:
            pass

        self.assertIsNone(
            tts._pending_turn_id,
            "_pending_turn_id must be None after exception (try/finally ensures cleanup)",
        )

    async def test_no_turn_id_means_pending_stays_none(self):
        """When turn_id is absent, _pending_turn_id is set to None (falsy → falls through to super)."""
        tts = TurnAwareFakeTTS()
        start_frame = LLMFullResponseStartFrame()  # no set_turn_id

        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])

        self.assertIsNone(tts._pending_turn_id)
        self.assertIsNotNone(tts._turn_context_id)  # super() UUID fallback ran

    async def test_create_context_id_outside_bracket_defers_to_super(self):
        """Outside the LLMFullResponseStartFrame bracket, create_context_id defers to super().

        After the start frame is processed, _pending_turn_id is None — a second
        call (as if for a per-sentence context) falls through to the base class.
        """
        t1 = "per-sentence"
        tts = TurnAwareFakeTTS()

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)
        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])

        self.assertEqual(tts._turn_context_id, t1)
        self.assertIsNone(tts._pending_turn_id)

        # Call create_context_id() as if for a second sentence (outside the bracket)
        second_id = tts.create_context_id()
        # _pending_turn_id is None → defers to FakeTTSService.create_context_id() → new UUID
        self.assertIsNotNone(second_id)
        # The mixin did not return a stale pending id
        self.assertIsNone(tts._pending_turn_id)

    async def test_interrupted_tts_pending_cleared(self):
        """After an interruption, _pending_turn_id is not stuck on a stale value."""
        t1 = "tts-interrupt"
        tts = TurnAwareFakeTTS()

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        await run_test(tts, frames_to_send=[start_frame], expected_down_frames=[LLMFullResponseStartFrame])
        self.assertEqual(tts._turn_context_id, t1)
        self.assertIsNone(tts._pending_turn_id)

        # Send an interruption; verify no stale pending state
        await run_test(tts, frames_to_send=[InterruptionFrame()])
        self.assertIsNone(tts._pending_turn_id)


# ---------------------------------------------------------------------------
# 7. TestEndToEnd
# ---------------------------------------------------------------------------


class TestEndToEnd(unittest.IsolatedAsyncioTestCase):
    async def test_single_turn_user_id_matches_bot_id(self):
        """UserTurnStartedFrame and BotTurnCompletedFrame carry matching turn_ids."""
        context = LLMContext()
        user_agg = TurnAwareUserAggregator(
            context,
            params=LLMUserAggregatorParams(
                user_turn_strategies=UserTurnStrategies(
                    stop=[SpeechTimeoutUserTurnStopStrategy(user_speech_timeout=TRANSCRIPTION_TIMEOUT)],
                ),
            ),
        )
        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="Hi"), LLMFullResponseEndFrame()
        ])
        assistant_agg = TurnAwareAssistantAggregator(context)
        pipeline = Pipeline([user_agg, llm, assistant_agg])

        frames_to_send = [
            VADUserStartedSpeakingFrame(),
            TranscriptionFrame(text="Hello bot", user_id="", timestamp="now"),
            SleepFrame(),
            VADUserStoppedSpeakingFrame(),
            SleepFrame(sleep=TRANSCRIPTION_TIMEOUT + 0.15),
        ]

        (down, _) = await run_test(pipeline, frames_to_send=frames_to_send)

        user_frames = [f for f in down if isinstance(f, UserTurnStartedFrame)]
        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]

        self.assertEqual(len(user_frames), 1)
        self.assertEqual(len(bot_frames), 1)
        self.assertIsNotNone(user_frames[0].turn_id)
        self.assertEqual(user_frames[0].turn_id, bot_frames[0].turn_id)

    async def test_three_sequential_turns_unique_paired_ids(self):
        """Three turns produce three unique, correctly paired user/bot turn_ids."""
        context = LLMContext()

        class MultiResponseFakeLLM(TurnAwareLLMMixin, FrameProcessor):
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                self._turn_index = 0

            async def process_frame(self, frame, direction):
                await super().process_frame(frame, direction)
                if isinstance(frame, LLMContextFrame):
                    i = self._turn_index
                    self._turn_index += 1
                    await self.push_frame(LLMFullResponseStartFrame())
                    await self.push_frame(LLMTextFrame(text=f"response-{i}"))
                    await self.push_frame(LLMFullResponseEndFrame())
                else:
                    await self.push_frame(frame, direction)

        user_agg = TurnAwareUserAggregator(
            context,
            params=LLMUserAggregatorParams(
                user_turn_strategies=UserTurnStrategies(
                    stop=[SpeechTimeoutUserTurnStopStrategy(user_speech_timeout=TRANSCRIPTION_TIMEOUT)],
                ),
            ),
        )
        llm = MultiResponseFakeLLM()
        assistant_agg = TurnAwareAssistantAggregator(context)
        pipeline = Pipeline([user_agg, llm, assistant_agg])

        turn_delay = TRANSCRIPTION_TIMEOUT + 0.15
        frames_to_send = []
        for i in range(3):
            frames_to_send.extend([
                VADUserStartedSpeakingFrame(),
                TranscriptionFrame(text=f"turn {i}", user_id="", timestamp="now"),
                SleepFrame(),
                VADUserStoppedSpeakingFrame(),
                SleepFrame(sleep=turn_delay),
            ])

        (down, _) = await run_test(pipeline, frames_to_send=frames_to_send)

        user_ids = [f.turn_id for f in down if isinstance(f, UserTurnStartedFrame)]
        bot_ids = [f.turn_id for f in down if isinstance(f, BotTurnCompletedFrame)]

        self.assertEqual(len(user_ids), 3)
        self.assertEqual(len(bot_ids), 3)
        self.assertEqual(len(set(user_ids)), 3)
        self.assertEqual(len(set(bot_ids)), 3)

        for i, (uid, bid) in enumerate(zip(user_ids, bot_ids)):
            self.assertEqual(uid, bid, f"Turn {i}: user={uid!r} != bot={bid!r}")

    async def test_full_pipeline_including_tts_context_id(self):
        """Full chain: UserTurnStartedFrame.turn_id == TTS context_id == BotTurnCompletedFrame.turn_id."""
        context = LLMContext()

        user_agg = TurnAwareUserAggregator(
            context,
            params=LLMUserAggregatorParams(
                user_turn_strategies=UserTurnStrategies(
                    stop=[SpeechTimeoutUserTurnStopStrategy(user_speech_timeout=TRANSCRIPTION_TIMEOUT)],
                ),
            ),
        )
        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(), LLMTextFrame(text="Hi"), LLMFullResponseEndFrame()
        ])
        tts = TurnAwareFakeTTS()
        assistant_agg = TurnAwareAssistantAggregator(context)
        pipeline = Pipeline([user_agg, llm, tts, assistant_agg])

        frames_to_send = [
            VADUserStartedSpeakingFrame(),
            TranscriptionFrame(text="Hello", user_id="", timestamp="now"),
            SleepFrame(),
            VADUserStoppedSpeakingFrame(),
            SleepFrame(sleep=TRANSCRIPTION_TIMEOUT + 0.15),
        ]

        (down, _) = await run_test(pipeline, frames_to_send=frames_to_send)

        user_frames = [f for f in down if isinstance(f, UserTurnStartedFrame)]
        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]

        self.assertEqual(len(user_frames), 1)
        self.assertEqual(len(bot_frames), 1)

        user_turn_id = user_frames[0].turn_id
        bot_turn_id = bot_frames[0].turn_id
        tts_context_id = tts._turn_context_id

        self.assertIsNotNone(user_turn_id)
        self.assertEqual(user_turn_id, bot_turn_id, "user_id must equal bot_id")
        self.assertEqual(user_turn_id, tts_context_id, "user_id must equal TTS context_id")


# ---------------------------------------------------------------------------
# 8. TestEdgeCases — issues surfaced by adversarial review
# ---------------------------------------------------------------------------


class TestLLMMixinEdgeCases(unittest.IsolatedAsyncioTestCase):
    async def test_thought_lifecycle_frames_all_stamped(self):
        """LLMThoughtStartFrame and LLMThoughtEndFrame are stamped, not just LLMThoughtTextFrame."""
        from pipecat.frames.frames import LLMThoughtEndFrame, LLMThoughtStartFrame

        t1 = "thought-lifecycle"
        context = LLMContext()

        llm = TurnAwareFakeLLM(frames_to_emit=[
            LLMFullResponseStartFrame(),
            LLMThoughtStartFrame(),
            LLMThoughtTextFrame(text="thinking..."),
            LLMThoughtEndFrame(),
            LLMFullResponseEndFrame(),
        ])

        ctx_frame = LLMContextFrame(context=context)
        set_turn_id(ctx_frame, t1)

        expected_down = [
            LLMFullResponseStartFrame,
            LLMThoughtStartFrame,
            LLMThoughtTextFrame,
            LLMThoughtEndFrame,
            LLMFullResponseEndFrame,
        ]
        (down, _) = await run_test(llm, frames_to_send=[ctx_frame], expected_down_frames=expected_down)

        for frame in down:
            self.assertEqual(
                get_turn_id(frame),
                t1,
                f"{frame.__class__.__name__} has turn_id={get_turn_id(frame)!r}, expected {t1!r}",
            )

    async def test_upstream_context_frame_does_not_overwrite_current_turn_id(self):
        """An UPSTREAM-flowing LLMContextFrame (e.g., function-call follow-up) must NOT
        overwrite _current_turn_id so subsequent downstream response frames keep the
        correct id."""
        context = LLMContext()

        class AssistantSideContextPushLLM(TurnAwareLLMMixin, FrameProcessor):
            """LLM that, on seeing an upstream LLMContextFrame, processes it inline."""
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                self._responses: list[list] = []
                self._response_index = 0

            def set_responses(self, responses):
                self._responses = responses

            async def process_frame(self, frame, direction):
                await super().process_frame(frame, direction)
                if isinstance(frame, LLMContextFrame):
                    idx = self._response_index
                    self._response_index += 1
                    if idx < len(self._responses):
                        for f in self._responses[idx]:
                            await self.push_frame(f)
                else:
                    await self.push_frame(frame, direction)

        llm = AssistantSideContextPushLLM()
        llm.set_responses([
            # Turn 1: downstream context, produces frames
            [LLMFullResponseStartFrame(), LLMTextFrame(text="turn1"), LLMFullResponseEndFrame()],
            # Turn 2: this would be from an upstream context frame — but since we gate on
            # DOWNSTREAM, the upstream frame doesn't trigger _current_turn_id update
        ])

        # First: downstream context with t1
        ctx_down = LLMContextFrame(context=context)
        set_turn_id(ctx_down, "down-t1")

        # Second: upstream context with t2 (simulates function call follow-up)
        ctx_up = LLMContextFrame(context=context)
        set_turn_id(ctx_up, "up-t2")

        # Send downstream ctx first — this sets _current_turn_id=down-t1
        expected = [LLMFullResponseStartFrame, LLMTextFrame, LLMFullResponseEndFrame]
        (down, _) = await run_test(
            llm,
            frames_to_send=[ctx_down],
            expected_down_frames=expected,
        )
        for f in down:
            self.assertEqual(get_turn_id(f), "down-t1")

        # Verify _current_turn_id is still down-t1 after seeing an upstream context frame
        # We do this by directly testing the mixin's direction gate
        self.assertEqual(llm._current_turn_id, "down-t1")
        # Simulate upstream context frame arriving
        import asyncio
        # Create a minimal test: upstream frame must not overwrite
        # (We test via the property directly since run_test doesn't easily test upstream input)
        original_turn_id = llm._current_turn_id
        # Manually call process_frame with upstream direction
        await llm.process_frame(ctx_up, FrameDirection.UPSTREAM)
        self.assertEqual(
            llm._current_turn_id,
            original_turn_id,
            "Upstream LLMContextFrame must not overwrite _current_turn_id",
        )


class TestUserAggregatorEdgeCases(unittest.IsolatedAsyncioTestCase):
    async def test_llm_run_frame_generates_turn_id(self):
        """LLMRunFrame triggers a context frame with a non-None turn_id even without VAD.

        _get_context_frame lazy-mints a UUID when _turn_id is None so that
        non-VAD-triggered LLM invocations (e.g., greeting on startup) are traceable.
        """
        context = LLMContext()
        user_agg = TurnAwareUserAggregator(context)

        frames_to_send = [LLMRunFrame()]
        # LLMUserAggregator emits SpeechControlParamsFrame + LLMContextFrame on LLMRunFrame
        (down, _) = await run_test(user_agg, frames_to_send=frames_to_send)

        context_frames = [f for f in down if isinstance(f, LLMContextFrame)]
        self.assertEqual(len(context_frames), 1, f"Expected 1 LLMContextFrame, got {len(context_frames)}")

        turn_id = get_turn_id(context_frames[0])
        self.assertIsNotNone(turn_id, "LLMRunFrame-triggered context frame must have a turn_id")
        # Should be a valid UUID4
        uuid.UUID(turn_id, version=4)

    async def test_llm_messages_append_run_llm_generates_turn_id(self):
        """LLMMessagesAppendFrame(run_llm=True) also triggers a turn_id."""
        from pipecat.frames.frames import LLMMessagesAppendFrame

        context = LLMContext()
        user_agg = TurnAwareUserAggregator(context)

        frames_to_send = [LLMMessagesAppendFrame(messages=[{"role": "user", "content": "hi"}], run_llm=True)]
        (down, _) = await run_test(user_agg, frames_to_send=frames_to_send)

        context_frames = [f for f in down if isinstance(f, LLMContextFrame)]
        self.assertEqual(len(context_frames), 1)

        turn_id = get_turn_id(context_frames[0])
        self.assertIsNotNone(turn_id, "LLMMessagesAppendFrame(run_llm=True) context frame must have a turn_id")

    async def test_each_non_vad_llm_run_gets_unique_turn_id(self):
        """Two consecutive LLMRunFrames each generate a distinct turn_id.

        The lazy-mint must not reuse a stale ID from a prior non-VAD run.
        """
        context = LLMContext()
        user_agg = TurnAwareUserAggregator(context)

        frames_to_send = [LLMRunFrame(), LLMRunFrame()]
        (down, _) = await run_test(user_agg, frames_to_send=frames_to_send)

        context_frames = [f for f in down if isinstance(f, LLMContextFrame)]
        self.assertEqual(len(context_frames), 2)

        id1 = get_turn_id(context_frames[0])
        id2 = get_turn_id(context_frames[1])
        self.assertIsNotNone(id1)
        self.assertIsNotNone(id2)
        self.assertNotEqual(id1, id2, "Each non-VAD run must get a distinct turn_id")


class TestAssistantAggregatorEdgeCases(unittest.IsolatedAsyncioTestCase):
    async def test_end_frame_emits_bot_turn_completed_not_interrupted(self):
        """EndFrame while bot turn is in progress emits BotTurnCompletedFrame(interrupted=False)."""
        from pipecat.frames.frames import EndFrame

        t1 = "t-end"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        # StartFrame is needed to initialise the pipeline sink; we just test the aggregator
        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="mid-response"), SleepFrame(), EndFrame()],
            send_end_frame=False,  # we send our own
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 1)
        self.assertFalse(bot_frames[0].interrupted, "EndFrame produces interrupted=False")
        self.assertEqual(bot_frames[0].turn_id, t1)

    async def test_cancel_frame_emits_bot_turn_completed_interrupted(self):
        """CancelFrame while bot turn is in progress emits BotTurnCompletedFrame(interrupted=True)."""
        from pipecat.frames.frames import CancelFrame

        t1 = "t-cancel"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, LLMTextFrame(text="mid"), SleepFrame(), CancelFrame()],
            send_end_frame=False,
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 1)
        self.assertTrue(bot_frames[0].interrupted, "CancelFrame produces interrupted=True")
        self.assertEqual(bot_frames[0].turn_id, t1)

    async def test_empty_turn_interrupted_before_first_token(self):
        """Bot interrupted before any token: BotTurnCompletedFrame(text='', interrupted=True)."""
        t1 = "t-empty-interrupt"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        start_frame = LLMFullResponseStartFrame()
        set_turn_id(start_frame, t1)

        (down, _) = await run_test(
            agg,
            frames_to_send=[start_frame, SleepFrame(), InterruptionFrame()],
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 1)
        self.assertTrue(bot_frames[0].interrupted)
        self.assertEqual(bot_frames[0].text, "", "No text was generated")
        self.assertEqual(bot_frames[0].turn_id, t1)

    async def test_function_call_context_frame_stamped_with_turn_id(self):
        """_get_context_frame on the assistant aggregator stamps _turn_id.

        When a function-call result triggers push_context_frame(UPSTREAM), that
        LLMContextFrame must carry the active turn_id so the follow-up LLM response
        is traceable.
        """
        t1 = "t-funcall"
        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        # Simulate active turn: set _turn_id directly (as _handle_llm_start would)
        agg._turn_id = t1
        agg._assistant_turn_start_timestamp = "2024-01-01T00:00:00"

        # Call _get_context_frame directly — this is what push_context_frame uses
        ctx_frame = agg._get_context_frame()
        self.assertEqual(
            get_turn_id(ctx_frame),
            t1,
            "Assistant aggregator's _get_context_frame must stamp _turn_id onto context frames",
        )

    async def test_assistant_turn_stopped_message_turn_id_not_in_asdict(self):
        """Dynamic turn_id attribute on AssistantTurnStoppedMessage is silently lost by asdict().

        This documents a known limitation: asdict() only serializes declared fields.
        Consumers needing turn_id for serialization MUST use BotTurnCompletedFrame instead.
        """
        import dataclasses

        from pipecat.processors.aggregators.llm_response_universal import AssistantTurnStoppedMessage

        msg = AssistantTurnStoppedMessage(content="hello", interrupted=False, timestamp="now")
        msg.turn_id = "t-asdict"  # type: ignore[attr-defined]

        d = dataclasses.asdict(msg)
        self.assertNotIn(
            "turn_id",
            d,
            "turn_id is a dynamic attribute and is NOT preserved by asdict() — use BotTurnCompletedFrame",
        )

    async def test_no_bot_turn_completed_when_no_active_turn_on_end_frame(self):
        """EndFrame with no active turn (no preceding LLMFullResponseStartFrame) emits nothing."""
        from pipecat.frames.frames import EndFrame

        context = LLMContext()
        agg = TurnAwareAssistantAggregator(context)

        (down, _) = await run_test(
            agg,
            frames_to_send=[EndFrame()],
            send_end_frame=False,
        )

        bot_frames = [f for f in down if isinstance(f, BotTurnCompletedFrame)]
        self.assertEqual(len(bot_frames), 0, "No active turn means no BotTurnCompletedFrame on EndFrame")


if __name__ == "__main__":
    unittest.main()
