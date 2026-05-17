// bot.go — Gato session entry point
//
// This is the Go equivalent of a pipecat bot.py. It shows the full pipeline for
// one voice session and makes explicit every package that must be built.
// It is a design sketch, not compilable code — function signatures are illustrative.

package main

import (
	"context"
	"os"
	"time"

	// ── Gato framework — must build ──────────────────────────────────────────
	//
	// These packages have no pipecat equivalent we can copy; each must be
	// implemented from scratch in Go, modelled on the pipecat Python source.

	// Core frame types (~25 of pipecat's 236; the rest are LLM/video/telephony)
	"github.com/voqalcloud/gato/frames"
	// Provides: Frame, SystemFrame, DataFrame, UninterruptibleFrame
	//           InputAudioRawFrame, OutputAudioRawFrame, TTSAudioRawFrame
	//           TranscriptionFrame, InterimTranscriptionFrame, TTSTextFrame
	//           InterruptionFrame, StartFrame, EndFrame, CancelFrame, StopFrame
	//           UserStartedSpeakingFrame, UserStoppedSpeakingFrame
	//           BotStartedSpeakingFrame, BotStoppedSpeakingFrame, BotSpeakingFrame
	//           TTSStartedFrame, TTSStoppedFrame, ErrorFrame, HeartbeatFrame
	//           OutputTransportReadyFrame

	// FrameProcessor base class + FrameDirection + FrameProcessorQueue
	// Python source: processors/frame_processor.py (1074 lines)
	// Key: priority queue (system > data), two goroutines per processor,
	//      prev/next links, observer hooks, TaskManager integration.
	"github.com/voqalcloud/gato/pipeline"

	// FrameQueue: channel-like queue that tracks UninterruptibleFrame count
	// and exposes Reset() to drain interruptible frames while keeping others.
	// Python source: utils/frame_queue.py (94 lines)
	// Critical for interrupt correctness in BaseOutputTransport.
	"github.com/voqalcloud/gato/pipeline/queue"

	// PipelineTask: wraps Pipeline with PipelineSource + PipelineSink,
	// injects StartFrame, runs heartbeat goroutine, idle timeout, observer fan-out.
	// Python source: pipeline/task.py (1044 lines)
	"github.com/voqalcloud/gato/pipeline/task"

	// BaseObserver interface + VoqalCloud observer (telemetry)
	// Python source: observers/base_observer.py
	"github.com/voqalcloud/gato/observers"

	// BaseInputTransport: audio input queue + filter + passthrough flag.
	// BaseOutputTransport + MediaSender: THE HARD PART.
	//   - audio chunk normalization (incoming → 10 ms chunks)
	//   - streaming sample-rate resampler (TTS rate → RTP track rate)
	//   - FrameQueue-backed audio task goroutine
	//   - PriorityQueue-backed clock task goroutine (PTS-based frame delivery)
	//   - BotStartedSpeaking / BotStoppedSpeaking state machine
	//   - Interruption handling: Reset() or cancel+recreate audio task
	// Python source: transports/base_output.py (995 lines), base_input.py (265 lines)
	"github.com/voqalcloud/gato/transport"

	// PionWebRTCTransport: concrete transport wrapping a Pion PeerConnection.
	//   Input side:  audio RTP → 20 ms InputAudioRawFrame (16 kHz, mono)
	//   Output side: TTSAudioRawFrame → resample → 10 ms chunks → Pion WriteSample
	//                with real-time pacing (sleep per chunk to bound interrupt latency)
	// Python analogue: transports/smallwebrtc/transport.py + RawAudioTrack
	"github.com/voqalcloud/gato/transport/pion"

	// SileroVAD: CGO wrapper around onnxruntime. Runs every 20 ms on InputAudioRawFrame.
	// Goroutine pool sized to runtime.NumCPU() shares one ONNX session.
	// Pushes VADSpeechStartFrame / VADSpeechEndFrame.
	// Python analogue: audio/vad/silero.py (used as analyzer inside user aggregator)
	"github.com/voqalcloud/gato/audio/vad"

	// TurnDetectorV3: CGO wrapper around onnxruntime. Runs only on VADSpeechEnd.
	// Classifier answers "is this turn complete?" to distinguish pause vs end-of-turn.
	// Pushes UserStoppedSpeakingFrame when turn is judged complete.
	// Python analogue: livekit-agents endpointing.py DynamicEndpointing
	"github.com/voqalcloud/gato/audio/turn"

	// UserAggregator: accumulates TranscriptionFrames after UserStartedSpeakingFrame.
	// On UserStoppedSpeakingFrame → sends TurnStarted + TranscriptFinal to IPC.
	// On InterruptionFrame → pushes TurnInterrupted to IPC.
	// Simpler than pipecat's LLMUserAggregator (no LLM context, no tool calls).
	// Python analogue: processors/aggregators/llm_response_universal.py (user side)
	//
	// AssistantAggregator: accumulates TextChunkFrames from IPC.
	// On EndOfTurn from IPC → emits LLMStoppedFrame to allow TTS to flush.
	// Tracks TTS text for [HEARD] calculation on interruption.
	// Simpler than pipecat's LLMAssistantAggregator.
	"github.com/voqalcloud/gato/processors/agg"

	// ProtobufIPCBridge: bidirectional WebSocket frame boundary.
	//   Downstream: TurnStarted, TranscriptFinal, TurnInterrupted(heard_text) → protobuf → WS
	//   Upstream:   protobuf → WS → TextChunkFrame, EndOfTurnFrame, InterruptFrame
	// Proto schema follows pipecat's Frame { oneof payload } canvas.
	"github.com/voqalcloud/gato/processors/ipc"

	// ── Provider implementations — also must build, lower risk ───────────────

	// Google Cloud STT: gRPC bidirectional streaming.
	// InputAudioRawFrame → StreamingRecognize → TranscriptionFrame (interim + final)
	// Must handle: stream lifecycle, reconnect on silence/timeout, config message.
	// Python analogue: services/deepgram/stt.py (different provider, same frame contract)
	"github.com/voqalcloud/gato/services/stt/google"

	// Google Cloud TTS: HTTP/2 streaming synthesis.
	// TTSTextFrame → SynthesizeSpeech (streaming) → TTSAudioRawFrame chunks → TTSStoppedFrame
	// Python analogue: services/cartesia/tts.py (different provider, same frame contract)
	"github.com/voqalcloud/gato/services/tts/google"

	// ── External dependencies — already exist ────────────────────────────────
	"github.com/pion/webrtc/v3"
)

// runSession is the per-session entry point, one goroutine cluster per WebRTC connection.
// Python equivalent: run_bot() in bot.py.
func runSession(ctx context.Context, conn *webrtc.PeerConnection, sessionID string) error {

	// ── Provider services ────────────────────────────────────────────────────
	stt := google.NewSTTService(google.STTConfig{
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		SampleRate:      16000,
		LanguageCode:    "en-US",
		// InterimResults: true — needed for streaming transcription display
	})

	tts := google.NewTTSService(google.TTSConfig{
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		VoiceName:       "en-US-Chirp3-HD-Charon",
		// OutputSampleRate must match AudioOutSampleRate below to avoid resampling.
		OutputSampleRate: 24000,
	})

	// ── In-process audio processors (CGO / ONNX) ────────────────────────────
	vadProc := vad.NewSileroVAD(vad.Config{
		ModelPath:     "/models/silero_vad.onnx",
		SampleRate:    16000,
		ChunkDuration: 20 * time.Millisecond,
		Threshold:     0.5,
	})

	turnProc := turn.NewTurnDetectorV3(turn.Config{
		ModelPath:  "/models/turn_detector_v3.onnx",
		SampleRate: 16000,
	})

	// ── Aggregators ──────────────────────────────────────────────────────────
	userAgg := agg.NewUserAggregator(agg.UserConfig{SessionID: sessionID})
	assistantAgg := agg.NewAssistantAggregator(agg.AssistantConfig{SessionID: sessionID})

	// ── IPC bridge ───────────────────────────────────────────────────────────
	ipcBridge := ipc.NewProtobufBridge(ipc.Config{
		Endpoint:  os.Getenv("BUSINESS_LAYER_WS_ENDPOINT"),
		SessionID: sessionID,
	})

	// ── Transport ────────────────────────────────────────────────────────────
	// Wraps a Pion PeerConnection. Internally holds:
	//   Input:  RTP track reader → 20 ms chunks → push InputAudioRawFrame downstream
	//   Output: BaseOutputTransport with MediaSender (see transport/base_output.go)
	xport := pion.NewTransport(conn, pion.Params{
		AudioInSampleRate:  16000,
		AudioOutSampleRate: 24000, // must match TTS OutputSampleRate to skip resampler
		AudioOut10msChunks: 1,     // 10 ms per chunk → interrupt latency bound
		AudioOutChannels:   1,
	})

	// ── Pipeline ─────────────────────────────────────────────────────────────
	//
	// Downstream (left → right):
	//   RTP audio → VAD → TurnDetect → STT → UserAgg → IPC → AssistantAgg → TTS → RTP
	//
	// Upstream (right → left):
	//   InterruptionFrame floods back through all processors
	//   BotStartedSpeaking / BotStoppedSpeaking from OutputTransport → UserAgg
	//   TTSStoppedFrame from TTS → AssistantAgg
	//
	// IPC bridge is bidirectional:
	//   Downstream: TurnStarted, TranscriptFinal, TurnInterrupted → wire
	//   Upstream:   TextChunk, EndOfTurn, Interrupt ← wire
	p := pipeline.New([]pipeline.FrameProcessor{
		xport.Input(),  // BaseInputTransport
		vadProc,        // SileroVAD (CGO)
		turnProc,       // TurnDetectorV3 (CGO)
		stt,            // GoogleSTTService
		userAgg,        // UserAggregator
		ipcBridge,      // ProtobufIPCBridge (bidirectional)
		assistantAgg,   // AssistantAggregator
		tts,            // GoogleTTSService
		xport.Output(), // BaseOutputTransport + MediaSender
	})

	ptask := task.New(p, task.Params{
		EnableMetrics:    true,
		HeartbeatEnabled: true,
		IdleTimeoutSecs:  300,
		Observers: []observers.Observer{
			observers.NewVoqalCloudObserver(sessionID),
		},
	})

	xport.OnClientConnected(func() {
		// Kick off the pipeline. PipelineTask injects StartFrame first.
		ptask.QueueFrame(frames.NewStartFrame())
	})
	xport.OnClientDisconnected(func() {
		ptask.Cancel()
	})

	return ptask.Run(ctx)
}

// ── What bot.go does NOT contain (vs a full pipecat bot.py) ──────────────────
//
//   LLMService          — lives in the business layer, connected via ipcBridge
//   LLMContext          — lives in the business layer
//   Tool call handlers  — lives in the business layer
//   RTVI processor      — phase 2
//   Recording           — phase 2
//   Video               — not planned
//   Multiple transports — Gato always uses Pion WebRTC
