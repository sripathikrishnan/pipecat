#
# voqalcloud/aggregators/assistant_aggregator.py
#
# TurnAwareAssistantAggregator: the terminal layer that closes the turn loop.
#
# Reads turn_id from LLMFullResponseStartFrame (stamped by the LLM service
# mixin) and carries it as an instance variable until _trigger_assistant_turn_stopped.
# Instance variable is safe here — assistant turns are sequential within this
# processor: a new one cannot start until LLMFullResponseStartFrame arrives,
# which only happens after the previous turn has ended or been interrupted.
#
# This class surgically overrides _trigger_assistant_turn_stopped because it
# is a private method that constructs AssistantTurnStoppedMessage inline; we
# need turn_id in that message and in the BotTurnCompletedFrame we push.
#

from loguru import logger

from pipecat.frames.frames import LLMFullResponseStartFrame
from pipecat.processors.aggregators.llm_response_universal import (
    AssistantTurnStoppedMessage,
    LLMAssistantAggregator,
    LLMAssistantAggregatorParams,
)
from pipecat.processors.aggregators.llm_context import LLMContext

from voqalcloud.frames.frames import BotTurnCompletedFrame, get_turn_id, set_turn_id


class TurnAwareAssistantAggregator(LLMAssistantAggregator):
    """Assistant aggregator that captures turn_id from LLMFullResponseStartFrame
    and closes the traceable turn lifecycle by emitting BotTurnCompletedFrame.
    """

    def __init__(self, context: LLMContext, *, params: LLMAssistantAggregatorParams | None = None, **kwargs):
        super().__init__(context, params=params, **kwargs)
        self._turn_id: str | None = None

    # ------------------------------------------------------------------
    # Override: stamp current turn_id onto context frames pushed upstream
    # for function-call result follow-ups.  Without this, the upstream
    # LLMContextFrame would carry no turn_id and the follow-up LLM
    # response would be untraceable.
    # ------------------------------------------------------------------

    def _get_context_frame(self):
        frame = super()._get_context_frame()
        set_turn_id(frame, self._turn_id)
        return frame

    # ------------------------------------------------------------------
    # Override: capture turn_id the moment the LLM starts responding.
    # ------------------------------------------------------------------

    async def _handle_llm_start(self, frame: LLMFullResponseStartFrame):
        self._turn_id = get_turn_id(frame)
        logger.debug(f"{self}: Assistant turn started, turn_id={self._turn_id}")
        await super()._handle_llm_start(frame)

    # ------------------------------------------------------------------
    # Override: inject turn_id into the stopped message and emit
    # BotTurnCompletedFrame.
    #
    # SURGICAL OVERRIDE: we duplicate ~12 lines from the parent because
    # AssistantTurnStoppedMessage is constructed inline in the parent and
    # there is no hook to extend it without full replication.
    # Any changes to the parent method must be mirrored here.
    # ------------------------------------------------------------------

    async def _trigger_assistant_turn_stopped(self, *, interrupted: bool = False):
        if not self._assistant_turn_start_timestamp:
            return

        # Snapshot before push_aggregation() + reset() clears both.
        turn_id = self._turn_id
        timestamp = self._assistant_turn_start_timestamp

        aggregation = await self.push_aggregation()
        if aggregation:
            aggregation = self._maybe_strip_turn_completion_markers(aggregation)

        message = AssistantTurnStoppedMessage(
            content=aggregation,
            interrupted=interrupted,
            timestamp=timestamp,
        )
        # Attach turn_id for callers of on_assistant_turn_stopped who want it
        # without importing BotTurnCompletedFrame.
        message.turn_id = turn_id  # type: ignore[attr-defined]

        await self._call_event_handler("on_assistant_turn_stopped", message)
        self._assistant_turn_start_timestamp = ""
        self._turn_id = None

        # Emit the voqalcloud terminal frame. Consumers correlate this back to
        # UserTurnStartedFrame via turn_id — full pipeline traceability.
        if turn_id:
            logger.debug(
                f"{self}: BotTurnCompletedFrame turn_id={turn_id} "
                f"interrupted={interrupted} chars={len(aggregation)}"
            )
            await self.push_frame(
                BotTurnCompletedFrame(
                    turn_id=turn_id,
                    text=aggregation,
                    interrupted=interrupted,
                    timestamp=timestamp,
                )
            )
