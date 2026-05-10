#
# voqalcloud/services/tts_service.py
#
# TurnAwareTTSMixin: uses the turn_id that arrived on LLMFullResponseStartFrame
# as the TTS audio context ID, instead of generating a fresh UUID.
#
# WHY THIS WORKS WITHOUT INSTANCE-VARIABLE RISK
# -----------------------------------------------
# TTSService processes LLM turns sequentially: the audio context serialization
# queue ensures that one LLM turn's audio is fully synthesized before the next
# begins (or the current one is cancelled by an interruption and reset).
# Therefore _pending_turn_id as an instance variable is safe here:
#   - set  synchronously before  await super().process_frame(LLMFullResponseStartFrame)
#   - read synchronously inside  super().process_frame via create_context_id()
#   - cleared synchronously after super().process_frame returns
#
# No other frame can be processed in this processor between set and clear because
# asyncio tasks within a processor are sequential.
#
# HOW create_context_id() WIRES IN
# ----------------------------------
# TTSService.process_frame for LLMFullResponseStartFrame calls:
#
#     self._turn_context_id = self.create_context_id()
#
# By overriding create_context_id() we redirect that assignment to use the
# propagated turn_id.  Subsequent calls to create_context_id() for individual
# sentences within the same LLM turn fall through to super(), which reuses
# self._turn_context_id (now equal to the turn_id) when
# reuse_context_id_within_turn=True — so all sentences share the same turn_id
# as their audio context ID.
#

from loguru import logger

from pipecat.frames.frames import LLMFullResponseStartFrame

from voqalcloud.frames.frames import get_turn_id


class TurnAwareTTSMixin:
    """Mixin for any TTSService subclass.

    Uses the turn_id propagated by TurnAwareLLMMixin as the TTS audio context ID
    so that TTSAudioRawFrame, TTSTextFrame, and TTSStartedFrame/StoppedFrame
    all carry the same ID as the originating user turn.

    Place this FIRST in the MRO:

        class MyTTS(TurnAwareTTSMixin, ElevenLabsTTSService):
            pass
    """

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self._pending_turn_id: str | None = None

    # ------------------------------------------------------------------
    # Override: bracket super().process_frame with _pending_turn_id so
    # that create_context_id() sees it during the LLMFullResponseStartFrame
    # branch in TTSService.process_frame.
    # ------------------------------------------------------------------

    async def process_frame(self, frame, direction):
        if isinstance(frame, LLMFullResponseStartFrame):
            self._pending_turn_id = get_turn_id(frame)
            logger.debug(
                f"{self}: LLMFullResponseStartFrame received, "
                f"pending_turn_id={self._pending_turn_id}"
            )
            try:
                await super().process_frame(frame, direction)
            finally:
                # Always clear so an exception in the TTS provider doesn't
                # leave a stale ID that poisons the next turn.
                self._pending_turn_id = None
        else:
            await super().process_frame(frame, direction)

    # ------------------------------------------------------------------
    # Override: return turn_id as the context ID for the current LLM turn.
    #
    # Called by TTSService.process_frame during LLMFullResponseStartFrame:
    #     self._turn_context_id = self.create_context_id()
    #
    # Also called per-sentence in _push_tts_frames().  When _pending_turn_id
    # is None (i.e., we are not in the LLMFullResponseStartFrame branch),
    # super() reuses self._turn_context_id which was set to the turn_id on
    # the first call — so all sentences share the same ID automatically.
    # ------------------------------------------------------------------

    def create_context_id(self) -> str:
        if self._pending_turn_id:
            # First call: during LLMFullResponseStartFrame processing.
            # The audio context for this turn does not exist yet, so we do NOT
            # call _refresh_audio_context (nothing to refresh).
            return self._pending_turn_id
        # Subsequent calls (individual sentences): delegate to parent, which
        # reuses self._turn_context_id when reuse_context_id_within_turn=True.
        return super().create_context_id()
