package main

import (
	"context"
	"encoding/binary"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// Output constants: 48kHz mono s16le, 10ms chunks.
	outSampleRate  = 48000
	outChunkMs     = 10
	outBytesPerSmp = 2
	outChannels    = 1
	// outChunkBytes = 48000 samples/s × 0.01 s × 1 ch × 2 bytes = 960 bytes
	outChunkBytes = outSampleRate / (1000 / outChunkMs) * outChannels * outBytesPerSmp

	// Input/VAD constants: 16kHz mono s16le.
	vadSampleRate = 16000
	// VAD processes 512 samples = 32ms at 16kHz per chunk.
	// Plus 64 context samples (last 64 from the previous call) prepended,
	// giving InferSize=576 total samples passed to Infer().
	vadSamplesPerChunk = 512
	vadBytesPerChunk   = vadSamplesPerChunk * 2

	// Turn detection thresholds.
	speechThreshold  = 0.5
	speechStartCount = 3  // 3 × 32ms = ~96ms speech to start turn
	speechEndCount   = 25 // 25 × 32ms = 800ms silence to end turn
)

// StatusEvent is used to push UI status updates to connected clients via SSE.
type StatusEvent struct {
	Kind string // "vad", "stt", "bot", "interrupt"
	Text string
}

// Session holds the full pipeline for one connected browser client.
type Session struct {
	pc          *webrtc.PeerConnection
	outputTrack *webrtc.TrackLocalStaticSample

	vad      *SileroVAD
	vadState StreamState

	stt     *STTClient
	tts     *GoogleTTS
	encoder *opus.Encoder // Opus encoder for output track

	audioQueue *AudioQueue
	resampler  *LinearResampler

	// heard text: text of last complete TTS segment played before interruption.
	heardMu   sync.Mutex
	heardText string

	// interrupted is set to 1 when the user starts speaking mid-bot-turn.
	interrupted atomic.Int32

	// statusCh delivers UI events (optional; nil means no SSE client).
	statusCh chan StatusEvent

	// cancel shuts down the session.
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSession allocates a Session. vad and tts are shared resources from the server.
// stt is per-session (created inside). Call Run(ctx) to start goroutines.
func NewSession(
	pc *webrtc.PeerConnection,
	outputTrack *webrtc.TrackLocalStaticSample,
	vad *SileroVAD,
	tts *GoogleTTS,
	statusCh chan StatusEvent,
) *Session {
	enc, err := opus.NewEncoder(outSampleRate, outChannels, opus.AppVoIP)
	if err != nil {
		log.Panicf("opus.NewEncoder: %v", err)
	}
	return &Session{
		pc:          pc,
		outputTrack: outputTrack,
		vad:         vad,
		tts:         tts,
		encoder:     enc,
		audioQueue:  NewAudioQueue(),
		resampler:   &LinearResampler{},
		statusCh:    statusCh,
	}
}

// Run starts the session goroutines with a child context derived from ctx.
func (s *Session) Run(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)

	// Start wakeOnCancel for the output queue.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		wakeOnCancel(ctx, s.audioQueue)
	}()

	// Start the audio output (playback) goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.audioTaskRun(ctx)
	}()
}

// Close shuts the session down gracefully.
func (s *Session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.audioQueue.Close()
	s.wg.Wait()
	if s.stt != nil {
		s.stt.Close()
	}
}

// emit sends a status event non-blocking.
func (s *Session) emit(kind, text string) {
	if s.statusCh == nil {
		return
	}
	select {
	case s.statusCh <- StatusEvent{Kind: kind, Text: text}:
	default:
	}
}

// handleInputTrack reads RTP from the browser mic, decodes Opus, decimates
// 48→16kHz, runs VAD, and sends speech segments to STT.
func (s *Session) handleInputTrack(ctx context.Context, track *webrtc.TrackRemote) {
	decoder, err := opus.NewDecoder(48000, 1)
	if err != nil {
		log.Printf("[session] opus.NewDecoder: %v", err)
		return
	}

	pcm48 := make([]int16, 960) // 20ms at 48kHz

	// VAD accumulation buffer: accumulate 512 samples at 16kHz per chunk.
	// Each decoded Opus frame gives 320 samples at 16kHz (960 samples @ 48kHz, decimated 3:1).
	var vadBuf []float32

	// Context buffer: last 64 samples passed to the previous VAD call.
	// Silero VAD v5's outer model prepends these to each 512-sample chunk before
	// calling the inner model. Without this context, all probabilities are ~0.
	vadContext := make([]float32, ContextSize)

	// closeTurn tears down the active STT stream. Safe to call when no turn is active.
	closeTurn := func() {
		if s.stt != nil {
			s.stt.Close()
			s.stt = nil
		}
	}
	defer closeTurn()

	// Turn state machine.
	var (
		speechConsecutive  int
		silenceConsecutive int
		inTurn             bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pkt, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("[session] ReadRTP: %v", err)
			return
		}

		// Decode Opus → 48kHz PCM.
		n, err := decoder.Decode(pkt.Payload, pcm48)
		if err != nil {
			log.Printf("[session] opus decode: %v", err)
			continue
		}

		// Decimate 3:1 — take every 3rd sample to get 16kHz.
		nOut := n / 3
		pcm16 := make([]int16, nOut)
		for i := range pcm16 {
			pcm16[i] = pcm48[i*3]
		}

		// Convert int16 to float32 for VAD.
		for _, s16 := range pcm16 {
			vadBuf = append(vadBuf, float32(s16)/32768.0)
		}

		// Process full 512-sample VAD chunks.
		for len(vadBuf) >= vadSamplesPerChunk {
			chunk := vadBuf[:vadSamplesPerChunk]
			vadBuf = vadBuf[vadSamplesPerChunk:]

			// Prepend context (64 samples) to form a 576-sample input.
			input := make([]float32, InferSize)
			copy(input[:ContextSize], vadContext)
			copy(input[ContextSize:], chunk)

			prob, newState, err := s.vad.Infer(input, vadSampleRate, s.vadState)
			if err != nil {
				log.Printf("[session] vad infer: %v", err)
				continue
			}
			s.vadState = newState
			// Advance context: keep the last 64 samples of the current chunk.
			copy(vadContext, input[vadSamplesPerChunk:])

			if prob > speechThreshold {
				speechConsecutive++
				silenceConsecutive = 0
			} else {
				silenceConsecutive++
				speechConsecutive = 0
			}

			// Turn start: 60ms consecutive speech.
			if !inTurn && speechConsecutive >= speechStartCount {
				inTurn = true
				speechConsecutive = 0
				log.Println("[vad] turn START")
				s.emit("vad", "speaking")

				// Handle interruption if bot is playing audio.
				if s.interrupted.CompareAndSwap(0, 1) {
					s.handleInterrupt()
				}

				// Start STT. Use the session context; STT.Close() stops it.
				var sttErr error
				s.stt, sttErr = NewSTTClient(ctx)
				if sttErr != nil {
					log.Printf("[session] NewSTTClient: %v", sttErr)
					inTurn = false
					continue
				}
				s.stt.Start(ctx)

				// Start goroutine to read STT results.
				s.wg.Add(1)
				sttRef := s.stt
				go func() {
					defer s.wg.Done()
					for transcript := range sttRef.Results() {
						if transcript != "" {
							log.Printf("[stt] %q", transcript)
							s.emit("stt", transcript)
							go s.handleSTTResult(ctx, transcript)
						}
					}
				}()
			}

			// Turn end: 500ms consecutive silence after being in a turn.
			if inTurn && silenceConsecutive >= speechEndCount {
				inTurn = false
				silenceConsecutive = 0
				log.Println("[vad] turn END")
				s.emit("vad", "silent")
				closeTurn()
			}

			// Feed audio to STT while in turn.
			if inTurn && s.stt != nil {
				// Convert pcm16 chunk (32ms = 512 samples) to bytes.
				pcmBytes := int16SliceToBytes(chunk)
				if err := s.stt.SendAudio(pcmBytes); err != nil {
					log.Printf("[session] SendAudio: %v", err)
				}
			}
		}
	}
}

// handleSTTResult receives a final transcript, calls the stub LLM, calls TTS,
// and enqueues audio for playback.
func (s *Session) handleSTTResult(ctx context.Context, transcript string) {
	response := stubLLM(transcript)
	log.Printf("[llm] %q → %q", transcript, response)
	s.emit("bot", response)

	// Reset interruption flag for new bot turn.
	s.interrupted.Store(0)
	s.resampler.Reset()

	// Synthesize.
	pcm24, err := s.tts.Synthesize(ctx, response)
	if err != nil {
		log.Printf("[session] TTS: %v", err)
		return
	}

	// Upsample 24kHz → 48kHz.
	pcm48 := s.resampler.Resample(pcm24)

	// Chunk into 10ms pieces and enqueue.
	for i := 0; i < len(pcm48); i += outChunkBytes {
		end := i + outChunkBytes
		if end > len(pcm48) {
			end = len(pcm48)
		}
		isLast := end >= len(pcm48)
		// Pad last chunk to a full Opus frame (outChunkBytes) with silence.
		chunk := make([]byte, outChunkBytes)
		copy(chunk, pcm48[i:end])
		segText := ""
		if isLast {
			segText = response
		}
		s.audioQueue.Put(&AudioFrame{
			Data:        chunk,
			SegmentText: segText,
			IsLastChunk: isLast,
		})
	}
	s.audioQueue.Put(&StopAudioFrame{})
}

// handleInterrupt stops bot audio and logs heard text.
func (s *Session) handleInterrupt() {
	s.audioQueue.Reset()
	s.resampler.Reset()

	s.heardMu.Lock()
	heard := s.heardText
	s.heardMu.Unlock()

	log.Printf("[interrupt] heard: %q", heard)
	s.emit("interrupt", heard)
}

// audioTaskRun is the real-time audio playback loop.
// It dequeues 10ms chunks and writes them to the Pion output track using
// monotonic-clock targeting to avoid cumulative drift.
func (s *Session) audioTaskRun(ctx context.Context) {
	target := time.Now()
	const chunkDur = 10 * time.Millisecond

	for {
		frame, err := s.audioQueue.Get(ctx)
		if err != nil {
			return // context cancelled
		}
		if frame == nil {
			return // queue closed and empty
		}

		switch f := frame.(type) {
		case *AudioFrame:
			// Monotonic clock targeting — fixes cumulative drift.
			target = target.Add(chunkDur)
			if sleep := time.Until(target); sleep > 0 {
				time.Sleep(sleep)
			}

			if s.outputTrack != nil {
				// Convert s16le bytes → int16 slice for Opus encoder.
				pcm := make([]int16, len(f.Data)/2)
				for i := range pcm {
					pcm[i] = int16(binary.LittleEndian.Uint16(f.Data[i*2:]))
				}
				// Encode PCM → Opus. Buffer sized conservatively.
				opusBuf := make([]byte, 1000)
				n, encErr := s.encoder.Encode(pcm, opusBuf)
				if encErr != nil {
					log.Printf("[audio] opus encode: %v", encErr)
					break
				}
				err := s.outputTrack.WriteSample(media.Sample{
					Data:     opusBuf[:n],
					Duration: chunkDur,
				})
				if err != nil {
					log.Printf("[audio] WriteSample: %v", err)
				}
			}

			// Update heardText when the last chunk of a segment plays.
			if f.IsLastChunk && f.SegmentText != "" {
				s.heardMu.Lock()
				s.heardText = f.SegmentText
				s.heardMu.Unlock()
			}

		case *StopAudioFrame:
			// Bot utterance complete — reset target so next utterance starts fresh.
			target = time.Now()

		case *EndAudioFrame:
			return
		}
	}
}

// int16SliceToBytes converts []float32 VAD chunk back to int16 PCM bytes.
// We receive float32 from VAD processing; STT needs int16 bytes.
func int16SliceToBytes(f32 []float32) []byte {
	out := make([]byte, len(f32)*2)
	for i, v := range f32 {
		// Clamp to [-1, 1].
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		s16 := int16(v * math.MaxInt16)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s16))
	}
	return out
}
