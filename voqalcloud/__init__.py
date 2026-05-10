"""voqalcloud — turn-traceable extension layer for pipecat pipelines.

Provides TurnAware* wrappers that thread a stable turn_id from the moment a
user starts speaking through to the moment the bot's response is committed to
the LLM context.

Quick start::

    from voqalcloud import TurnAwareContextAggregatorPair
    from voqalcloud.services import TurnAwareLLMMixin, TurnAwareTTSMixin

    # 1. Create aggregators
    aggs = TurnAwareContextAggregatorPair(llm_context)

    # 2. Compose your LLM and TTS services
    class MyLLM(TurnAwareLLMMixin, OpenAILLMService):
        pass

    class MyTTS(TurnAwareTTSMixin, ElevenLabsTTSService):
        pass

    # 3. Register handler — turn_id is now on every message
    @aggs.assistant().event_handler("on_assistant_turn_stopped")
    async def on_turn(agg, message):
        print(message.turn_id, message.content, message.interrupted)
"""

from pipecat.processors.aggregators.llm_context import LLMContext
from pipecat.processors.aggregators.llm_response_universal import (
    LLMAssistantAggregatorParams,
    LLMUserAggregatorParams,
)

from voqalcloud.aggregators.assistant_aggregator import TurnAwareAssistantAggregator
from voqalcloud.aggregators.user_aggregator import TurnAwareUserAggregator
from voqalcloud.frames.frames import BotTurnCompletedFrame, UserTurnStartedFrame
from voqalcloud.services.llm_service import TurnAwareLLMMixin
from voqalcloud.services.tts_service import TurnAwareTTSMixin


class TurnAwareContextAggregatorPair:
    """Drop-in replacement for LLMContextAggregatorPair with turn traceability."""

    def __init__(
        self,
        context: LLMContext,
        *,
        user_params: LLMUserAggregatorParams | None = None,
        assistant_params: LLMAssistantAggregatorParams | None = None,
    ):
        self._user = TurnAwareUserAggregator(context, params=user_params)
        self._assistant = TurnAwareAssistantAggregator(context, params=assistant_params)

    def user(self) -> TurnAwareUserAggregator:
        return self._user

    def assistant(self) -> TurnAwareAssistantAggregator:
        return self._assistant

    def __iter__(self):
        return iter((self._user, self._assistant))


__all__ = [
    "TurnAwareContextAggregatorPair",
    "TurnAwareAssistantAggregator",
    "TurnAwareUserAggregator",
    "TurnAwareLLMMixin",
    "TurnAwareTTSMixin",
    "BotTurnCompletedFrame",
    "UserTurnStartedFrame",
]
