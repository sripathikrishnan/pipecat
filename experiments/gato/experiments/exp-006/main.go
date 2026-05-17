// Package main demonstrates real-time Google Cloud STT streaming (EXP-006).
//
// Usage:
//
//	go run . [--file testdata/speech.raw]
//
// Application Default Credentials (ADC) are used automatically.
// Run `gcloud auth application-default login` if you do not have a service account.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"time"
)

const (
	testdataShort = "testdata/speech.raw"
	testdataLong  = "testdata/long_speech.raw"

	genSampleRate = 16000
	genFrequency  = 440.0
	genAmplitude  = 16000
)

func main() {
	audioFile := flag.String("file", testdataShort, "raw PCM file to stream (16 kHz, mono, int16 LE)")
	flag.Parse()

	// Generate test data if it doesn't exist.
	ensureTestData()

	ctx := context.Background()

	log.Printf("[exp-006] opening STT stream for file: %s", *audioFile)

	client, err := NewStreamingSTTClient(ctx)
	if err != nil {
		log.Fatalf("NewStreamingSTTClient: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Printf("Close: %v", err)
		}
	}()

	client.Start(ctx)

	// Collect results in the background.
	resultsDone := make(chan struct{})
	go func() {
		defer close(resultsDone)
		for r := range client.Results() {
			kind := "interim"
			if r.IsFinal {
				kind = "FINAL"
			}
			fmt.Printf("[%s] %s\n", kind, r.Text)
		}
	}()

	// Stream audio.
	startTime := time.Now()
	if err := streamFile(client, *audioFile); err != nil {
		log.Fatalf("streamFile: %v", err)
	}

	elapsed := time.Since(startTime)
	log.Printf("[exp-006] finished streaming in %s; closing stream", elapsed.Round(time.Millisecond))

	// Close the client so the results channel drains.
	if err := client.Close(); err != nil {
		log.Printf("Close: %v", err)
	}

	<-resultsDone
	log.Println("[exp-006] done")
}

// streamFile reads the given raw PCM file and sends it to the STT client in 100 ms chunks.
func streamFile(client *StreamingSTTClient, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// 100 ms chunk: 16000 samples/s * 0.1 s * 2 bytes/sample = 3200 bytes
	const chunkBytes = 16000 * 2 * 100 / 1000

	for i := 0; i < len(data); i += chunkBytes {
		end := i + chunkBytes
		if end > len(data) {
			end = len(data)
		}
		if err := client.SendAudio(data[i:end]); err != nil {
			return fmt.Errorf("SendAudio: %w", err)
		}
	}
	return nil
}

// ensureTestData generates synthetic test audio files if they don't already exist.
func ensureTestData() {
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		log.Fatalf("mkdir testdata: %v", err)
	}

	if _, err := os.Stat(testdataShort); os.IsNotExist(err) {
		log.Printf("generating %s ...", testdataShort)
		if err := writeSineWave(testdataShort, 30); err != nil {
			log.Fatalf("generate %s: %v", testdataShort, err)
		}
	}

	if _, err := os.Stat(testdataLong); os.IsNotExist(err) {
		log.Printf("generating %s (this may take a moment) ...", testdataLong)
		if err := writeSineWave(testdataLong, 360); err != nil {
			log.Fatalf("generate %s: %v", testdataLong, err)
		}
	}
}

// writeSineWave writes durationSecs seconds of a 440 Hz sine wave to path
// in raw int16 little-endian PCM at 16 kHz.
func writeSineWave(path string, durationSecs int) error {
	numSamples := genSampleRate * durationSecs
	buf := make([]byte, numSamples*2)

	for i := 0; i < numSamples; i++ {
		t := float64(i) / float64(genSampleRate)
		sample := int16(float64(genAmplitude) * math.Sin(2*math.Pi*genFrequency*t))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}

	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s (%d bytes = %.0f seconds)", path, len(buf), float64(len(buf))/(genSampleRate*2))
	return nil
}
