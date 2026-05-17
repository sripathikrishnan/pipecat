package main

import (
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// SileroVAD wraps the Silero VAD v5 ONNX model.
// Strategy B: one ONNX session shared across all streams;
// each logical stream carries its own LSTM hidden state (state tensor).
//
// The onnxruntime session is thread-safe for concurrent inference calls.
// LSTM state is passed as input tensors and received as output tensors,
// so each call is fully independent — no shared mutable state in the session.
//
// Silero VAD v5 model I/O (as inspected from the ONNX graph):
//
//	Inputs:  input [1, 512] float32
//	         state [2, 1, 128] float32  (combined h+c)
//	         sr    []  int64   (scalar)
//	Outputs: output  [1, 1] float32
//	         stateN  [2, 1, 128] float32

// StreamState holds the per-stream LSTM hidden state for Silero VAD v5.
// The state tensor is [2, 1, 128]: two layers, batch=1, 128 features (h and c concatenated).
// Callers maintain one StreamState per concurrent audio stream.
// Passing by value ensures complete isolation between streams.
type StreamState struct {
	State [2][1][128]float32 // Combined LSTM state (h+c)
}

// SileroVAD wraps a single shared ONNX session for the Silero VAD v5 model.
// The onnxruntime C API is thread-safe for concurrent Run() calls on the same session.
// No mutex is required: all per-call state is in local tensors, not session fields.
type SileroVAD struct {
	session    *ort.DynamicAdvancedSession
	errorInject bool // for failure injection testing
}

var ortOnce sync.Once
var ortInitErr error

// initORT initialises the ONNX Runtime environment exactly once.
func initORT() error {
	ortOnce.Do(func() {
		ort.SetSharedLibraryPath("/opt/homebrew/lib/libonnxruntime.dylib")
		ortInitErr = ort.InitializeEnvironment()
	})
	return ortInitErr
}

// NewSileroVAD loads the Silero VAD v5 ONNX model from modelPath and returns
// a shared SileroVAD that can be called concurrently from multiple goroutines.
func NewSileroVAD(modelPath string) (*SileroVAD, error) {
	if err := initORT(); err != nil {
		return nil, fmt.Errorf("onnxruntime init: %w", err)
	}

	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input", "state", "sr"},
		[]string{"output", "stateN"},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	return &SileroVAD{session: session}, nil
}

// Close releases the ONNX session resources.
func (v *SileroVAD) Close() {
	v.session.Destroy()
}

// InjectError sets a flag that causes all subsequent Infer() calls to return an error.
// Used by failure injection tests.
func (v *SileroVAD) InjectError(inject bool) {
	v.errorInject = inject
}

// Infer runs one VAD inference step on a 512-sample audio chunk.
//
// audio must be exactly 512 float32 samples (32ms at 16kHz).
// sr is the sample rate (should be 16000).
// state is the caller-maintained LSTM hidden state for this stream.
//
// Returns the speech probability in [0, 1], the updated LSTM state,
// and any error. Thread-safe: multiple goroutines can call concurrently
// with their own distinct state instances.
func (v *SileroVAD) Infer(audio []float32, sr int64, state StreamState) (prob float32, newState StreamState, err error) {
	if v.errorInject {
		return 0, state, fmt.Errorf("injected VAD error")
	}

	if len(audio) != 512 {
		return 0, state, fmt.Errorf("audio must be 512 samples, got %d", len(audio))
	}

	// --- Build input tensors ---

	// input: [1, 512]
	inputShape := ort.NewShape(1, 512)
	inputTensor, e := ort.NewTensor(inputShape, audio)
	if e != nil {
		return 0, state, fmt.Errorf("create input tensor: %w", e)
	}
	defer inputTensor.Destroy()

	// state: [2, 1, 128]
	stateShape := ort.NewShape(2, 1, 128)
	stateFlat := flattenState(state.State)
	stateTensor, e := ort.NewTensor(stateShape, stateFlat)
	if e != nil {
		return 0, state, fmt.Errorf("create state tensor: %w", e)
	}
	defer stateTensor.Destroy()

	// sr: scalar int64 — pass as shape [1] since DynamicAdvancedSession needs a slice
	srShape := ort.NewShape(1)
	srTensor, e := ort.NewTensor(srShape, []int64{sr})
	if e != nil {
		return 0, state, fmt.Errorf("create sr tensor: %w", e)
	}
	defer srTensor.Destroy()

	// --- Build output tensors ---

	// output: [1, 1]
	outShape := ort.NewShape(1, 1)
	outData := make([]float32, 1)
	outTensor, e := ort.NewTensor(outShape, outData)
	if e != nil {
		return 0, state, fmt.Errorf("create output tensor: %w", e)
	}
	defer outTensor.Destroy()

	// stateN: [2, 1, 128]
	stateNShape := ort.NewShape(2, 1, 128)
	stateNData := make([]float32, 2*1*128)
	stateNTensor, e := ort.NewTensor(stateNShape, stateNData)
	if e != nil {
		return 0, state, fmt.Errorf("create stateN tensor: %w", e)
	}
	defer stateNTensor.Destroy()

	// --- Run inference ---
	// onnxruntime sessions are thread-safe for concurrent Run() calls.
	// Each call allocates its own input/output tensors on the stack, so there
	// is no shared mutable state between concurrent goroutines.
	e = v.session.Run(
		[]ort.ArbitraryTensor{inputTensor, stateTensor, srTensor},
		[]ort.ArbitraryTensor{outTensor, stateNTensor},
	)
	if e != nil {
		return 0, state, fmt.Errorf("session run: %w", e)
	}

	// --- Extract results ---
	prob = outTensor.GetData()[0]
	newState.State = unflattenState(stateNTensor.GetData())

	return prob, newState, nil
}

// flattenState converts [2][1][128]float32 → []float32 (row-major).
func flattenState(s [2][1][128]float32) []float32 {
	out := make([]float32, 2*128)
	for i := 0; i < 2; i++ {
		for k := 0; k < 128; k++ {
			out[i*128+k] = s[i][0][k]
		}
	}
	return out
}

// unflattenState converts []float32 (len=256) → [2][1][128]float32.
func unflattenState(flat []float32) [2][1][128]float32 {
	var s [2][1][128]float32
	for i := 0; i < 2; i++ {
		for k := 0; k < 128; k++ {
			s[i][0][k] = flat[i*128+k]
		}
	}
	return s
}
