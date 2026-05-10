#
# voqalcloud/frames/frames.py
#
# New frame types and helpers for turn-id propagation.
# Turn-id is threaded through existing pipecat frames as a dynamic attribute
# using get_turn_id / set_turn_id. These new frames are voqalcloud-native and
# carry the completed traceability payload for downstream consumers.
#

from dataclasses import dataclass

from pipecat.frames.frames import Frame

# Dynamic attribute name used on existing pipecat frames (LLMContextFrame,
# LLMFullResponseStartFrame, LLMFullResponseEndFrame).
TURN_ID_ATTR = "turn_id"


def get_turn_id(frame) -> str | None:
    """Read the turn_id off any frame (returns None if absent)."""
    return getattr(frame, TURN_ID_ATTR, None)


def set_turn_id(frame, turn_id: str | None) -> None:
    """Stamp a turn_id onto any unfrozen dataclass frame."""
    setattr(frame, TURN_ID_ATTR, turn_id)


@dataclass
class UserTurnStartedFrame(Frame):
    """Emitted downstream when a user turn begins.

    Carries the freshly-generated turn_id so pipeline observers can
    associate subsequent frames with this specific user utterance.
    """

    turn_id: str
    timestamp: str


@dataclass
class BotTurnCompletedFrame(Frame):
    """Emitted downstream when a bot turn is committed to the LLM context.

    Carries the full traceability payload: which user turn triggered this
    response, the exact text saved to context (possibly a subset of what was
    generated if the turn was interrupted), and whether it was cut short.

    This is the terminal event in the turn lifecycle. Consumers can correlate
    it back to UserTurnStartedFrame via turn_id.
    """

    turn_id: str
    text: str        # what was actually saved to context
    interrupted: bool
    timestamp: str   # when the assistant turn started (ISO 8601)
