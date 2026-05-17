//go:build ignore

// testdata_gen.go generates raw PCM test audio files for EXP-009.
//
// Run with: go run testdata_gen.go
//
// Generates:
//   - testdata/user_speech.raw  — 10s of 440 Hz at 16 kHz mono int16 (160000 bytes)
//   - testdata/tts_response.raw — 5s of 880 Hz at 48 kHz mono int16  (480000 bytes)
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

func main() {
	if err := generateSine("testdata/user_speech.raw", 440.0, 16000, 10); err != nil {
		fmt.Fprintf(os.Stderr, "user_speech: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Generated testdata/user_speech.raw (10s 440Hz 16kHz mono int16, 160000 bytes)")

	if err := generateSine("testdata/tts_response.raw", 880.0, 48000, 5); err != nil {
		fmt.Fprintf(os.Stderr, "tts_response: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Generated testdata/tts_response.raw (5s 880Hz 48kHz mono int16, 480000 bytes)")
}

// generateSine writes a raw mono int16 PCM sine wave to path.
// freq: frequency in Hz, sampleRate: samples per second, durationSec: length in seconds.
func generateSine(path string, freq float64, sampleRate int, durationSec int) error {
	nSamples := sampleRate * durationSec
	buf := make([]byte, nSamples*2) // int16 = 2 bytes per sample

	for i := 0; i < nSamples; i++ {
		t := float64(i) / float64(sampleRate)
		sample := math.Sin(2.0 * math.Pi * freq * t)
		// Scale to int16 range (80% amplitude to avoid clipping).
		s16 := int16(sample * 26214.0)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s16))
	}

	if err := os.MkdirAll("testdata", 0755); err != nil {
		return fmt.Errorf("mkdir testdata: %w", err)
	}
	return os.WriteFile(path, buf, 0644)
}
