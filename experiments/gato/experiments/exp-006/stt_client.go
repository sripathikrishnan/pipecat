package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	"cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TranscriptionResult holds a single transcription result from Google STT.
type TranscriptionResult struct {
	Text    string
	IsFinal bool
	// Stability is only set for interim results (0.0–1.0).
	Stability float32
}

// StreamingSTTClient wraps Google Cloud STT StreamingRecognize.
//
// Key behaviors:
//   - Opens a gRPC stream and sends a config message first.
//   - Sends audio in configurable chunk sizes (default 100 ms).
//   - Reads results in a background goroutine and delivers them on Results().
//   - Maintains a 500 ms circular replay buffer of recently-sent audio.
//   - On any gRPC stream error (including ResourceExhausted / OutOfRange from
//     Google's 5-minute hard limit), transparently restarts the stream and
//     replays the buffer so no audio is lost at the boundary.
type StreamingSTTClient struct {
	speechClient *speech.Client
	config       *speechpb.StreamingRecognitionConfig

	// results delivers transcription events to the caller.
	results chan TranscriptionResult

	// audio is the channel through which the caller pushes raw PCM bytes.
	audio chan []byte

	// done signals that Close() was called.
	done chan struct{}

	// replayBuf is the circular replay buffer (last 500 ms of sent audio).
	replayBuf *replayBuffer

	// wg tracks background goroutines.
	wg sync.WaitGroup

	// chunkDuration controls how often we flush an audio chunk to Google.
	chunkDuration time.Duration

	// sampleRate is the PCM sample rate in Hz (int16 samples expected).
	sampleRate int

	// mu protects stream-level state used by the reader goroutine.
	mu sync.Mutex
}

// NewStreamingSTTClient creates a StreamingSTTClient using Application Default Credentials.
//
// The caller is responsible for calling Close() when done.
func NewStreamingSTTClient(ctx context.Context) (*StreamingSTTClient, error) {
	client, err := speech.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("speech.NewClient: %w", err)
	}

	cfg := &speechpb.StreamingRecognitionConfig{
		Config: &speechpb.RecognitionConfig{
			Encoding:        speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz: 16000,
			LanguageCode:    "en-US",
		},
		InterimResults: true,
	}

	c := &StreamingSTTClient{
		speechClient:  client,
		config:        cfg,
		results:       make(chan TranscriptionResult, 64),
		audio:         make(chan []byte, 64),
		done:          make(chan struct{}),
		replayBuf:     newReplayBuffer(500 * time.Millisecond),
		chunkDuration: 100 * time.Millisecond,
		sampleRate:    16000,
	}
	return c, nil
}

// Results returns the channel on which transcription results are delivered.
// The channel is closed when the client stops (after Close() returns).
func (c *StreamingSTTClient) Results() <-chan TranscriptionResult {
	return c.results
}

// Start begins streaming. It must be called once before SendAudio.
// The provided context governs the entire streaming session; cancel it to stop.
func (c *StreamingSTTClient) Start(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(c.results)
		c.runLoop(ctx)
	}()
}

// SendAudio enqueues raw PCM audio for sending to Google STT.
// The slice must contain int16 little-endian samples at 16 kHz.
// Returns an error if the client has been closed.
func (c *StreamingSTTClient) SendAudio(pcm []byte) error {
	select {
	case c.audio <- pcm:
		return nil
	case <-c.done:
		return errors.New("client closed")
	}
}

// Close stops the streaming client and waits for all goroutines to exit.
func (c *StreamingSTTClient) Close() error {
	select {
	case <-c.done:
		// already closed
	default:
		close(c.done)
	}
	c.wg.Wait()
	return c.speechClient.Close()
}

// -------------------------------------------------------------------
// Internal implementation
// -------------------------------------------------------------------

// runLoop drives the stream lifecycle. It opens a stream, pumps audio,
// and restarts on error until the context is cancelled or done is closed.
func (c *StreamingSTTClient) runLoop(ctx context.Context) {
	for {
		err := c.runStream(ctx)
		if err == nil {
			// Normal shutdown (context cancelled / done closed).
			return
		}

		// Classify the error to decide whether to restart.
		if isStreamRestartable(err) {
			log.Printf("[stt] stream closed (%v) — restarting in 200ms", err)
			select {
			case <-time.After(200 * time.Millisecond):
				// continue to next iteration (restart)
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
			continue
		}

		// Fatal error — surface it as a result (no field for errors, so just log).
		log.Printf("[stt] fatal stream error: %v", err)
		return
	}
}

// runStream opens one gRPC stream, runs the sender and receiver, and returns
// when the stream ends. A nil error means graceful shutdown; a non-nil error
// may be retriable.
func (c *StreamingSTTClient) runStream(ctx context.Context) error {
	stream, err := c.speechClient.StreamingRecognize(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Send config first.
	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: c.config,
		},
	}); err != nil {
		return fmt.Errorf("send config: %w", err)
	}

	// Replay the buffer in case this is a restart.
	for _, chunk := range c.replayBuf.snapshot() {
		if err := stream.Send(&speechpb.StreamingRecognizeRequest{
			StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
				AudioContent: chunk,
			},
		}); err != nil {
			return fmt.Errorf("replay buffer: %w", err)
		}
	}

	// receiverDone signals that the receiver goroutine has exited.
	receiverErr := make(chan error, 1)

	go func() {
		receiverErr <- c.receiveResults(stream)
	}()

	// Pump audio chunks from the audio channel.
	senderErr := c.sendAudio(ctx, stream)

	// Close the send side so Google knows we're done sending.
	_ = stream.CloseSend()

	// Wait for the receiver to drain remaining results.
	recvErr := <-receiverErr

	// Prefer the receiver error (more informative).
	if recvErr != nil && !isEOF(recvErr) {
		return recvErr
	}
	if senderErr != nil {
		return senderErr
	}
	return nil
}

// sendAudio reads from c.audio, appends chunks to the replay buffer, and sends
// them to the stream. Returns when ctx is cancelled, done is closed, or the
// stream write fails.
func (c *StreamingSTTClient) sendAudio(ctx context.Context, stream speechpb.Speech_StreamingRecognizeClient) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.done:
			return nil
		case chunk, ok := <-c.audio:
			if !ok {
				return nil
			}
			c.replayBuf.push(chunk)
			if err := stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: chunk,
				},
			}); err != nil {
				return err
			}
		}
	}
}

// receiveResults reads from the stream and forwards results to c.results.
func (c *StreamingSTTClient) receiveResults(stream speechpb.Speech_StreamingRecognizeClient) error {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		for _, result := range resp.Results {
			for _, alt := range result.Alternatives {
				select {
				case c.results <- TranscriptionResult{
					Text:      alt.Transcript,
					IsFinal:   result.IsFinal,
					Stability: result.Stability,
				}:
				case <-c.done:
					return nil
				}
			}
		}
	}
}

// isStreamRestartable returns true for gRPC errors that indicate the stream
// closed due to Google's time limits (ResourceExhausted, OutOfRange) or
// normal EOF/stream-reset conditions.
func isStreamRestartable(err error) bool {
	if isEOF(err) {
		return true
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.ResourceExhausted, codes.OutOfRange, codes.Unavailable, codes.Internal:
		return true
	}
	return false
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// -------------------------------------------------------------------
// replayBuffer — circular buffer of recent audio chunks.
// -------------------------------------------------------------------

// replayBuffer keeps track of the last `maxDuration` of audio chunks,
// using the sample rate to estimate durations from byte counts.
//
// Thread-safe.
type replayBuffer struct {
	mu          sync.Mutex
	chunks      [][]byte
	maxDuration time.Duration
	sampleRate  int // Hz
}

func newReplayBuffer(maxDuration time.Duration) *replayBuffer {
	return &replayBuffer{
		maxDuration: maxDuration,
		sampleRate:  16000,
	}
}

// push adds a chunk and drops the oldest chunks that fall outside the window.
func (r *replayBuffer) push(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.chunks = append(r.chunks, chunk)

	// Prune leading chunks until total duration fits within maxDuration.
	for len(r.chunks) > 1 && r.totalDuration() > r.maxDuration {
		r.chunks = r.chunks[1:]
	}
}

// snapshot returns a copy of all buffered chunks (oldest first).
func (r *replayBuffer) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.chunks) == 0 {
		return nil
	}
	out := make([][]byte, len(r.chunks))
	copy(out, r.chunks)
	return out
}

// totalDuration computes the total duration of all buffered chunks.
// Must be called with r.mu held.
func (r *replayBuffer) totalDuration() time.Duration {
	var totalBytes int
	for _, c := range r.chunks {
		totalBytes += len(c)
	}
	// int16 mono: 2 bytes per sample
	samples := totalBytes / 2
	return time.Duration(float64(samples)/float64(r.sampleRate)*float64(time.Second))
}

// reset clears the buffer.
func (r *replayBuffer) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = r.chunks[:0]
}
