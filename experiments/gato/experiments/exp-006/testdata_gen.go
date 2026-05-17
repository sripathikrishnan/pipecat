//go:build ignore

// testdata_gen.go generates synthetic PCM audio files for STT experiments.
// Run with: go run testdata_gen.go
//
// Generates:
//   - testdata/speech.raw    — 30 seconds of 440 Hz sine wave, 16 kHz mono int16
//   - testdata/long_speech.raw — 360 seconds (6 minutes) of the same, for stream-restart testing
package main

import (
	"encoding/binary"
	"log"
	"math"
	"os"
)

const (
	sampleRate  = 16000  // Hz
	frequency   = 440.0  // Hz — A4 tone (approximates voiced audio shape)
	amplitude   = 16000  // int16 amplitude (roughly half of max 32767)
	shortSecs   = 30     // seconds for speech.raw
	longSecs    = 360    // seconds for long_speech.raw (6 minutes)
)

func generateSine(durationSecs int) []byte {
	numSamples := sampleRate * durationSecs
	buf := make([]byte, numSamples*2) // int16 = 2 bytes per sample

	for i := 0; i < numSamples; i++ {
		// Sine wave: A * sin(2π * f * t)
		t := float64(i) / float64(sampleRate)
		sample := int16(float64(amplitude) * math.Sin(2*math.Pi*frequency*t))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}
	return buf
}

func writeFile(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Fatalf("failed to write %s: %v", path, err)
	}
	log.Printf("wrote %s (%d bytes, %.1f seconds)", path, len(data), float64(len(data))/(sampleRate*2))
}

func main() {
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		log.Fatalf("failed to create testdata dir: %v", err)
	}

	log.Printf("generating %d-second sine wave for testdata/speech.raw ...", shortSecs)
	writeFile("testdata/speech.raw", generateSine(shortSecs))

	log.Printf("generating %d-second sine wave for testdata/long_speech.raw ...", longSecs)
	writeFile("testdata/long_speech.raw", generateSine(longSecs))

	log.Println("done")
}
