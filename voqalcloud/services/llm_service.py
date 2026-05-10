#
# voqalcloud/services/llm_service.py
#
# TurnAwareLLMMixin: threads turn_id through the LLM service.
#
# WHY AN INSTANCE VARIABLE IS CORRECT HERE
# -----------------------------------------
# Every standard pipecat LLM service (OpenAI, Anthropic, etc.) processes
# LLMContextFrame fully inline inside process_frame:
#
#     if isinstance(frame, LLMContextFrame):
#         await self.push_frame(LLMFullResponseStartFrame())
#         await self._process_context(frame.context)   # blocks until streaming done
#         await self.push_frame(LLMFullResponseEndFrame())
#
# The processor's input queue loop awaits process_frame before consuming the
# next frame, so a second LLMContextFrame cannot arrive until the first call
# has returned.  There is no concurrent LLM invocation, and an instance
# variable is perfectly safe.
#
# All push_frame calls (for Start/Text/End frames) happen from within the
# same blocking process_frame call on the same asyncio task, so they always
# read the correct _current_turn_id.
#
# DIRECTION GUARD ON CONTEXT FRAME
# ----------------------------------
# Only DOWNSTREAM-flowing LLMContextFrames update _current_turn_id.
# UPSTREAM-flowing LLMContextFrames (emitted by the assistant aggregator for
# function-call result follow-ups) must not overwrite the current turn_id with
# stale state — those frames carry the turn_id stamped by the assistant side,
# which may lag behind or belong to a different invocation.
#
# USAGE
# -----
# TurnAwareLLMMixin must be the FIRST parent in the MRO so its process_frame
# and push_frame run before the real LLM service:
#
#     class MyLLM(TurnAwareLLMMixin, OpenAILLMService):
#         pass
#
#     class MyLLM(TurnAwareLLMMixin, AnthropicLLMService):
#         pass
#
# No additional overrides are needed. The mixin is provider-agnostic.
#

from loguru import logger

from pipecat.frames.frames import (
    FunctionCallCancelFrame,
    FunctionCallInProgressFrame,
    FunctionCallResultFrame,
    FunctionCallsStartedFrame,
    LLMContextFrame,
    LLMFullResponseEndFrame,
    LLMFullResponseStartFrame,
    LLMTextFrame,
    LLMThoughtEndFrame,
    LLMThoughtStartFrame,
    LLMThoughtTextFrame,
)
from pipecat.processors.frame_processor import FrameDirection

from voqalcloud.frames.frames import get_turn_id, set_turn_id

# All LLM-originated downstream frame types that must carry turn_id.
# Thought start/end are included so observers can bracket the full thought
# lifecycle with a consistent turn_id, not just the text tokens.
# Function call frames are included so observers can correlate tool execution
# back to the originating user turn — broadcast_frame pushes both downstream
# and upstream copies; the mixin stamps only the downstream copy.
_LLM_DOWNSTREAM_FRAMES = (
    LLMFullResponseStartFrame,
    LLMFullResponseEndFrame,
    LLMTextFrame,
    LLMThoughtTextFrame,
    LLMThoughtStartFrame,
    LLMThoughtEndFrame,
    FunctionCallsStartedFrame,
    FunctionCallInProgressFrame,
    FunctionCallResultFrame,
    FunctionCallCancelFrame,
)


class TurnAwareLLMMixin:
    """Mixin that propagates turn_id from LLMContextFrame to all LLM output frames.

    Place this FIRST in the MRO:

        class MyLLM(TurnAwareLLMMixin, SomeLLMService):
            pass
    """

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self._current_turn_id: str | None = None

    async def process_frame(self, frame, direction):
        # Only downstream context frames carry a fresh turn_id.
        # Upstream context frames (function-call result follow-ups from the
        # assistant aggregator) must not overwrite the current turn_id.
        if isinstance(frame, LLMContextFrame) and direction == FrameDirection.DOWNSTREAM:
            self._current_turn_id = get_turn_id(frame)
            logger.debug(f"{self}: LLMContextFrame received, turn_id={self._current_turn_id}")

        await super().process_frame(frame, direction)

    async def push_frame(self, frame, direction=FrameDirection.DOWNSTREAM):
        if direction == FrameDirection.DOWNSTREAM and isinstance(frame, _LLM_DOWNSTREAM_FRAMES):
            if self._current_turn_id:
                set_turn_id(frame, self._current_turn_id)
                logger.debug(f"{self}: Stamped turn_id={self._current_turn_id} onto {frame.__class__.__name__}")

        await super().push_frame(frame, direction)


# ---------------------------------------------------------------------------
# STUB — replace with your concrete LLM service class.
# ---------------------------------------------------------------------------


class TurnAwareLLMServiceStub(TurnAwareLLMMixin):
    """
    STUB: Shows how to compose TurnAwareLLMMixin with a real LLM service.

    Replace this class with:

        from pipecat.services.openai import OpenAILLMService   # or your provider
        class VoqalLLMService(TurnAwareLLMMixin, OpenAILLMService):
            pass

    The mixin handles all turn_id propagation automatically — no additional
    overrides are required in the concrete class.

    ------------------------------------------------------------------
    WHAT THE REAL IMPLEMENTATION MUST DO (pseudocode)
    ------------------------------------------------------------------

    class VoqalLLMService(TurnAwareLLMMixin, BaseLLMService):

        async def process_frame(self, frame, direction):
            # [MIXIN] sets _current_turn_id from LLMContextFrame (DOWNSTREAM only)
            # [REAL]  receives LLMContextFrame, runs the LLM call inline:
            #
            #   await self.push_frame(LLMFullResponseStartFrame())  # mixin stamps turn_id
            #   async for token in your_llm_api.stream(...):
            #       await self.push_frame(LLMTextFrame(token))       # mixin stamps turn_id
            #   await self.push_frame(LLMFullResponseEndFrame())     # mixin stamps turn_id
            #
            # All push_frame calls happen WITHIN this process_frame call,
            # so _current_turn_id is always correct.
            await super().process_frame(frame, direction)

    ------------------------------------------------------------------
    IMPORTANT ORDERING CONSTRAINT
    ------------------------------------------------------------------
    TurnAwareLLMMixin MUST appear before the real service class in the MRO
    so that its process_frame() sets _current_turn_id BEFORE the service
    processes the context frame and pushes response frames.

    Correct:   class MyLLM(TurnAwareLLMMixin, OpenAILLMService)
    Incorrect: class MyLLM(OpenAILLMService, TurnAwareLLMMixin)
    """

    pass
