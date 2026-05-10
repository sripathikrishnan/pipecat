#
# voqalcloud/aggregators/user_aggregator.py
#
# TurnAwareUserAggregator: the origin of every turn_id.
#
# This is the TOPMOST layer where a single instance variable (_turn_id) is safe
# because the user aggregator processes turns sequentially — a new user turn
# cannot start until the previous one has definitionally ended (or been
# interrupted and reset). Generating the turn_id here and stamping it onto the
# outgoing LLMContextFrame is the only place where an instance variable is used
# for turn tracking.
#

import uuid

from loguru import logger

from pipecat.frames.frames import LLMContextFrame
from pipecat.processors.aggregators.llm_response_universal import (
    LLMAssistantAggregatorParams,
    LLMUserAggregator,
    LLMUserAggregatorParams,
)
from pipecat.processors.aggregators.llm_context import LLMContext
from pipecat.turns.user_start import BaseUserTurnStartStrategy, UserTurnStartedParams
from pipecat.turns.user_turn_controller import UserTurnController
from pipecat.utils.time import time_now_iso8601

from voqalcloud.frames.frames import UserTurnStartedFrame, set_turn_id


class TurnAwareUserAggregator(LLMUserAggregator):
    """User aggregator that generates a turn_id at the start of each user turn.

    The turn_id is stored as an instance variable (safe here — sequential) and
    stamped onto the outgoing LLMContextFrame as a dynamic attribute.  All
    downstream layers read it from the frame, never from shared state.
    """

    def __init__(self, context: LLMContext, *, params: LLMUserAggregatorParams | None = None, **kwargs):
        super().__init__(context, params=params, **kwargs)
        self._turn_id: str | None = None

    # ------------------------------------------------------------------
    # Override: generate turn_id at the moment a user turn starts.
    # Instance variable is safe: the aggregator is sequential.
    # ------------------------------------------------------------------

    async def _on_user_turn_started(
        self,
        controller: UserTurnController,
        strategy: BaseUserTurnStartStrategy,
        params: UserTurnStartedParams,
    ):
        self._turn_id = str(uuid.uuid4())
        logger.debug(f"{self}: User turn started, turn_id={self._turn_id}")

        # Let the parent handle interruptions, speaking frames, and events.
        await super()._on_user_turn_started(controller, strategy, params)

        # Emit a voqalcloud frame so any pipeline observer can pick up the new turn.
        await self.push_frame(
            UserTurnStartedFrame(turn_id=self._turn_id, timestamp=time_now_iso8601())
        )

    # ------------------------------------------------------------------
    # Override: stamp turn_id onto every LLMContextFrame we create.
    # From this point forward the ID travels on frame objects only.
    # ------------------------------------------------------------------

    def _get_context_frame(self) -> LLMContextFrame:
        frame = super()._get_context_frame()
        # For VAD-driven turns, _turn_id is set by _on_user_turn_started.
        # For non-VAD paths (LLMRunFrame, LLMMessagesAppendFrame(run_llm=True), etc.),
        # _turn_id is None — generate a one-shot UUID so every LLM invocation
        # is traceable. We don't store it back; the next non-VAD call gets its own.
        turn_id = self._turn_id or str(uuid.uuid4())
        set_turn_id(frame, turn_id)
        return frame
