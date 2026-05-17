#!/usr/bin/env bash
# EXP-010: End-to-End WebRTC test runner.
#
# Usage:
#   ./run_e2e_test.sh          # run full E2E test
#   ./run_e2e_test.sh --help
#
# Prerequisites:
#   - Go (with onnxruntime + opus installed via brew)
#   - Python 3 with aiortc installed (pip install -r client/requirements.txt)
#   - GCP Application Default Credentials (gcloud auth application-default login)
#   - testdata/test_audio.wav exists (run: make_testdata.sh or ffmpeg command)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXP008_DIR="$SCRIPT_DIR/../exp-008"
SERVER_PORT=8080
SERVER_URL="http://localhost:$SERVER_PORT"
SERVER_PID=""
LOG_DIR="$SCRIPT_DIR/logs"
OUTPUT_DIR="$SCRIPT_DIR/output"
AUDIO_FILE="$SCRIPT_DIR/testdata/test_audio.wav"
OUTPUT_FILE="$OUTPUT_DIR/received_$(date +%Y%m%d_%H%M%S).wav"

mkdir -p "$LOG_DIR" "$OUTPUT_DIR"

cleanup() {
    if [ -n "$SERVER_PID" ]; then
        echo "[runner] Stopping Go server (PID $SERVER_PID)..."
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# ── 1. Build the Go server ────────────────────────────────────────────────────
echo "[runner] Building EXP-008 Go server..."
cd "$EXP008_DIR"
CGO_LDFLAGS="-L/opt/homebrew/lib" \
CGO_CFLAGS="-I/opt/homebrew/include/onnxruntime" \
go build -o /tmp/gato-exp008 . 2>&1
echo "[runner] Build OK"
cd "$SCRIPT_DIR"

# ── 2. Start the Go server ────────────────────────────────────────────────────
SERVER_LOG="$LOG_DIR/server_$(date +%Y%m%d_%H%M%S).log"
echo "[runner] Starting Go server on :$SERVER_PORT (log: $SERVER_LOG)"
cd "$EXP008_DIR"
DYLD_LIBRARY_PATH="/opt/homebrew/lib:${DYLD_LIBRARY_PATH:-}" \
    /tmp/gato-exp008 >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
cd "$SCRIPT_DIR"

# Wait for server to be ready.
echo -n "[runner] Waiting for server..."
for i in $(seq 1 30); do
    if curl -sf "$SERVER_URL/" -o /dev/null 2>/dev/null; then
        echo " ready (${i}s)"
        break
    fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
        echo ""
        echo "[runner] ERROR: Server exited early. Check $SERVER_LOG"
        cat "$SERVER_LOG"
        exit 1
    fi
    sleep 1
    echo -n "."
done

# ── 3. Install Python deps if needed ─────────────────────────────────────────
if ! python3 -c "import aiortc" 2>/dev/null; then
    echo "[runner] Installing Python dependencies..."
    pip3 install -q -r "$SCRIPT_DIR/client/requirements.txt"
fi

# ── 4. Run the aiortc client ──────────────────────────────────────────────────
echo ""
echo "[runner] ═══════════════════════════════════════════════════════════"
echo "[runner] Starting E2E test"
echo "[runner] Input audio:  $AUDIO_FILE"
echo "[runner] Output audio: $OUTPUT_FILE"
echo "[runner] Server log:   $SERVER_LOG"
echo "[runner] ═══════════════════════════════════════════════════════════"
echo ""

python3 "$SCRIPT_DIR/client/e2e_client.py" \
    --audio "$AUDIO_FILE" \
    --output "$OUTPUT_FILE" \
    --server "$SERVER_URL" \
    --timeout 120

CLIENT_RC=$?

# ── 5. Print server log tail ──────────────────────────────────────────────────
echo ""
echo "[runner] ═══════════════════════════════════════════════════════════"
echo "[runner] Go server log (last 40 lines):"
echo "[runner] ═══════════════════════════════════════════════════════════"
tail -40 "$SERVER_LOG"

# ── 6. Print summary ──────────────────────────────────────────────────────────
echo ""
echo "[runner] ═══════════════════════════════════════════════════════════"
echo "[runner] TEST COMPLETE"
echo "[runner] Client exit code: $CLIENT_RC"
echo "[runner] Output WAV: $OUTPUT_FILE"
echo ""

if [ -f "$OUTPUT_FILE" ]; then
    SIZE=$(du -h "$OUTPUT_FILE" | cut -f1)
    DURATION=$(ffprobe -v error -show_entries format=duration \
        -of default=noprint_wrappers=1:nokey=1 "$OUTPUT_FILE" 2>/dev/null || echo "?")
    echo "[runner] Recorded audio: $SIZE, ${DURATION}s"
    echo ""
    echo "[runner] MANUAL VERIFICATION:"
    echo "[runner]   Play the output file and verify:"
    echo "[runner]   1. Audio is audible (not silence)"
    echo "[runner]   2. TTS says 'Okay, I heard: [first 10 words of input]'"
    echo "[runner]   3. Content makes sense"
    echo ""
    echo "[runner]   To play: afplay '$OUTPUT_FILE'"
    echo "[runner]   Or open: open '$OUTPUT_FILE'"
else
    echo "[runner] WARNING: Output file not found — recording may have failed"
fi

echo "[runner] ═══════════════════════════════════════════════════════════"

exit $CLIENT_RC
