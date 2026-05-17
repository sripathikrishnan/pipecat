#!/usr/bin/env python3
"""
EXP-010: End-to-End WebRTC test client using aiortc.

Connects to the Gato EXP-008 Go server, sends pre-recorded audio, and
records the TTS response for manual verification.

Usage:
    python e2e_client.py \
        --audio ../testdata/test_audio.wav \
        --output ../output/received.wav \
        --server http://localhost:8080 \
        --timeout 120

The client:
1. Creates a WebRTC PeerConnection with an audio send track (from WAV file).
2. Posts an SDP offer to the server's /offer endpoint.
3. Applies the SDP answer and connects.
4. Sends the audio at real-time rate via aiortc MediaPlayer.
5. Records the incoming audio track to ../output/received.wav.
6. Exits after --timeout seconds (or when the audio finishes + 30s buffer).
"""

import argparse
import asyncio
import json
import logging
import os
import sys
import time
from pathlib import Path

import aiohttp
from aiortc import RTCPeerConnection, RTCSessionDescription
from aiortc.contrib.media import MediaPlayer, MediaRecorder

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger("e2e_client")


async def run(server_url: str, audio_path: str, output_path: str, timeout: float):
    pc = RTCPeerConnection()
    recorder = MediaRecorder(output_path)

    # Track whether we have received any audio from the server.
    received_track_event = asyncio.Event()
    connection_event = asyncio.Event()
    connection_failed = asyncio.Event()

    @pc.on("connectionstatechange")
    async def on_connection_state():
        state = pc.connectionState
        log.info(f"Connection state: {state}")
        if state == "connected":
            connection_event.set()
        elif state in ("failed", "closed", "disconnected"):
            connection_failed.set()

    @pc.on("track")
    async def on_track(track):
        log.info(f"Received track from server: kind={track.kind}")
        if track.kind == "audio":
            recorder.addTrack(track)
            received_track_event.set()

    # Load audio from file and add as send track.
    # MediaPlayer reads the WAV and sends at real-time rate.
    log.info(f"Loading audio: {audio_path}")
    player = MediaPlayer(audio_path)
    if player.audio:
        pc.addTrack(player.audio)
        log.info("Audio send track added")
    else:
        log.error("No audio track in file — aborting")
        await pc.close()
        return 1

    # Create offer.
    offer = await pc.createOffer()
    await pc.setLocalDescription(offer)

    # Wait for ICE gathering to complete (Pion side waits too).
    await asyncio.sleep(1.0)

    offer_payload = {
        "sdp": pc.localDescription.sdp,
        "type": pc.localDescription.type,
    }

    log.info(f"Posting SDP offer to {server_url}/offer")
    try:
        async with aiohttp.ClientSession() as session:
            async with session.post(
                f"{server_url}/offer",
                json=offer_payload,
                timeout=aiohttp.ClientTimeout(total=30),
            ) as resp:
                if resp.status != 200:
                    body = await resp.text()
                    log.error(f"Server returned {resp.status}: {body}")
                    await pc.close()
                    return 1
                answer_data = await resp.json(content_type=None)
    except Exception as e:
        log.error(f"Failed to exchange SDP: {e}")
        await pc.close()
        return 1

    log.info("Received SDP answer from server")
    answer = RTCSessionDescription(
        sdp=answer_data["sdp"], type=answer_data["type"]
    )
    await pc.setRemoteDescription(answer)

    # Wait for connection.
    log.info("Waiting for ICE connection...")
    try:
        await asyncio.wait_for(connection_event.wait(), timeout=30.0)
    except asyncio.TimeoutError:
        log.error("ICE connection timed out after 30s")
        await pc.close()
        return 1

    log.info("Connected! Starting recorder...")
    await recorder.start()

    # Compute how long to run.
    import wave
    try:
        with wave.open(audio_path) as wf:
            audio_duration_s = wf.getnframes() / wf.getframerate()
    except Exception:
        audio_duration_s = 90.0  # fallback

    # Stay connected for audio duration + 30s buffer (for TTS response).
    run_duration = audio_duration_s + 30.0
    if timeout > 0:
        run_duration = min(run_duration, timeout)

    log.info(
        f"Audio duration: {audio_duration_s:.1f}s, "
        f"running for {run_duration:.1f}s total"
    )

    start = time.monotonic()
    while time.monotonic() - start < run_duration:
        if connection_failed.is_set():
            log.warning("Connection failed/closed early")
            break
        await asyncio.sleep(1.0)
        elapsed = time.monotonic() - start
        log.info(f"  ... {elapsed:.0f}s / {run_duration:.0f}s elapsed")

    log.info("Test duration complete — stopping recorder")
    await recorder.stop()
    await pc.close()

    log.info(f"Recorded output saved to: {output_path}")
    log.info("Manual verification: listen to the output file and confirm:")
    log.info("  - Audio is audible")
    log.info('  - TTS says "Okay, I heard: [first 10 words of speech]"')
    log.info("  - Playback matches the expected script")
    return 0


def main():
    parser = argparse.ArgumentParser(description="EXP-010 aiortc E2E test client")
    parser.add_argument(
        "--audio",
        default="../testdata/test_audio.wav",
        help="Path to input WAV file to send",
    )
    parser.add_argument(
        "--output",
        default="../output/received.wav",
        help="Path to save recorded server response",
    )
    parser.add_argument(
        "--server",
        default="http://localhost:8080",
        help="Go server base URL",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=120.0,
        help="Maximum seconds to run (0 = auto from audio duration)",
    )
    args = parser.parse_args()

    # Ensure output directory exists.
    Path(args.output).parent.mkdir(parents=True, exist_ok=True)

    rc = asyncio.run(
        run(
            server_url=args.server,
            audio_path=args.audio,
            output_path=args.output,
            timeout=args.timeout,
        )
    )
    sys.exit(rc)


if __name__ == "__main__":
    main()
