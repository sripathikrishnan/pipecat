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

// STTClient is a thin Google STT streaming client.
// It wraps the gRPC StreamingRecognize API with transparent reconnect and a
// 500ms replay buffer so no audio is lost at stream boundaries.
type STTClient struct {
	speechClient *speech.Client
	config       *speechpb.StreamingRecognitionConfig

	results chan string   // final transcripts
	audio   chan []byte   // raw PCM from caller
	done    chan struct{}

	replayBuf *sttReplayBuffer
	wg        sync.WaitGroup
}

// NewSTTClient creates an STTClient using Application Default Credentials.
// Call Start(ctx) before SendAudio().
func NewSTTClient(ctx context.Context) (*STTClient, error) {
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
		InterimResults: false, // final only, simpler turn logic
	}

	return &STTClient{
		speechClient: client,
		config:       cfg,
		results:      make(chan string, 64),
		audio:        make(chan []byte, 256),
		done:         make(chan struct{}),
		replayBuf:    newSTTReplayBuffer(500 * time.Millisecond),
	}, nil
}

// Start begins the streaming loop. Must be called once before SendAudio.
func (c *STTClient) Start(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(c.results)
		c.runLoop(ctx)
	}()
}

// SendAudio enqueues raw 16kHz mono int16 PCM for streaming to Google STT.
// Non-blocking: returns an error only if the client has been closed.
func (c *STTClient) SendAudio(pcm []byte) error {
	select {
	case c.audio <- pcm:
		return nil
	case <-c.done:
		return errors.New("stt client closed")
	}
}

// Results returns a channel that receives final transcripts.
// The channel is closed when the client stops.
func (c *STTClient) Results() <-chan string {
	return c.results
}

// Close terminates the stream and waits for all goroutines to exit.
func (c *STTClient) Close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	c.wg.Wait()
	c.speechClient.Close()
}

// --- internal ---

func (c *STTClient) runLoop(ctx context.Context) {
	for {
		err := c.runStream(ctx)
		if err == nil {
			return
		}
		if sttIsRestartable(err) {
			log.Printf("[stt] stream closed (%v) — restarting", err)
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return
			case <-c.done:
				return
			}
			continue
		}
		log.Printf("[stt] fatal error: %v", err)
		return
	}
}

func (c *STTClient) runStream(ctx context.Context) error {
	stream, err := c.speechClient.StreamingRecognize(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: c.config,
		},
	}); err != nil {
		return fmt.Errorf("send config: %w", err)
	}

	// Replay buffered audio on reconnect.
	for _, chunk := range c.replayBuf.snapshot() {
		if err := stream.Send(&speechpb.StreamingRecognizeRequest{
			StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
				AudioContent: chunk,
			},
		}); err != nil {
			return fmt.Errorf("replay: %w", err)
		}
	}

	recvErrCh := make(chan error, 1)
	go func() {
		recvErrCh <- c.receiveResults(stream)
	}()

	senderErr := c.sendAudio(ctx, stream)
	_ = stream.CloseSend()
	recvErr := <-recvErrCh

	if recvErr != nil && !errors.Is(recvErr, io.EOF) {
		return recvErr
	}
	return senderErr
}

func (c *STTClient) sendAudio(ctx context.Context, stream speechpb.Speech_StreamingRecognizeClient) error {
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

func (c *STTClient) receiveResults(stream speechpb.Speech_StreamingRecognizeClient) error {
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		for _, result := range resp.Results {
			if result.IsFinal {
				for _, alt := range result.Alternatives {
					select {
					case c.results <- alt.Transcript:
					case <-c.done:
						return nil
					}
				}
			}
		}
	}
}

func sttIsRestartable(err error) bool {
	if errors.Is(err, io.EOF) {
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

// sttReplayBuffer keeps the last maxDuration of audio for stream reconnects.
type sttReplayBuffer struct {
	mu          sync.Mutex
	chunks      [][]byte
	maxDuration time.Duration
}

func newSTTReplayBuffer(max time.Duration) *sttReplayBuffer {
	return &sttReplayBuffer{maxDuration: max}
}

func (r *sttReplayBuffer) push(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chunks = append(r.chunks, chunk)
	for len(r.chunks) > 1 && r.totalDuration() > r.maxDuration {
		r.chunks = r.chunks[1:]
	}
}

func (r *sttReplayBuffer) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.chunks) == 0 {
		return nil
	}
	out := make([][]byte, len(r.chunks))
	copy(out, r.chunks)
	return out
}

func (r *sttReplayBuffer) totalDuration() time.Duration {
	var total int
	for _, c := range r.chunks {
		total += len(c)
	}
	// 16kHz mono int16: 2 bytes/sample
	samples := total / 2
	return time.Duration(float64(samples) / 16000 * float64(time.Second))
}
