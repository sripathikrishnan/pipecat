package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestVADProbe checks what VAD probability the 440Hz sine wave produces.
func TestVADProbe(t *testing.T) {
	if err := initORT(); err != nil {
		t.Fatalf("ONNX Runtime init: %v", err)
	}
	vad, err := NewSileroVAD("testdata/silero_vad.onnx")
	if err != nil {
		t.Fatalf("Load VAD: %v", err)
	}
	defer vad.Close()

	// Generate 10 × 512-sample windows of 440Hz sine at 16kHz.
	const sampleRate = 16000
	const freq = 440.0
	var state StreamState

	for chunk := 0; chunk < 10; chunk++ {
		audio := make([]float32, 512)
		for i := 0; i < 512; i++ {
			t0 := float64(chunk*512+i) / sampleRate
			audio[i] = float32(math.Sin(2.0 * math.Pi * freq * t0))
		}
		prob, newState, err := vad.Infer(audio, sampleRate, state)
		if err != nil {
			t.Fatalf("Infer: %v", err)
		}
		state = newState
		t.Logf("chunk %d: VAD prob=%.4f", chunk, prob)
	}

	// Generate 10 × 512-sample windows of silence.
	t.Log("--- Silence windows ---")
	state = StreamState{}
	for chunk := 0; chunk < 10; chunk++ {
		audio := make([]float32, 512)
		// silence = all zeros
		_ = audio
		prob, newState, err := vad.Infer(audio, sampleRate, state)
		if err != nil {
			t.Fatalf("Infer: %v", err)
		}
		state = newState
		t.Logf("silence chunk %d: VAD prob=%.4f", chunk, prob)
	}

	// Generate input matching user_speech.raw format (int16 → float32).
	t.Log("--- From raw file (first 10 windows) ---")
	rawData := make([]byte, 10*512*2)
	for i := 0; i < 10*512; i++ {
		t0 := float64(i) / sampleRate
		s := math.Sin(2.0 * math.Pi * freq * t0)
		s16 := int16(s * 26214.0)
		binary.LittleEndian.PutUint16(rawData[i*2:], uint16(s16))
	}
	state = StreamState{}
	for chunk := 0; chunk < 10; chunk++ {
		audio := make([]float32, 512)
		for i := 0; i < 512; i++ {
			s16 := int16(rawData[(chunk*512+i)*2]) | int16(rawData[(chunk*512+i)*2+1])<<8
			audio[i] = float32(s16) / 32768.0
		}
		prob, newState, err := vad.Infer(audio, sampleRate, state)
		if err != nil {
			t.Fatalf("Infer: %v", err)
		}
		state = newState
		t.Logf("raw chunk %d: VAD prob=%.4f", chunk, prob)
	}
}
