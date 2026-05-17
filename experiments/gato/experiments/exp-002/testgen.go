// +build ignore

// testgen generates testdata files for EXP-002.
// Run: go run testgen.go
package main

import (
	"encoding/binary"
	"math"
	"os"
)

func main() {
	// sine_24k.raw: 440 Hz, 24 kHz, s16le, 10 seconds.
	writeSine("testdata/sine_24k.raw", 440, 24000, 10)
	// speech_24k.raw: multi-frequency speech-like signal, 24 kHz, 30 seconds.
	writeSpeech("testdata/speech_24k.raw", 24000, 30)
}

func writeSine(path string, freqHz, sampleRate, durationSec int) {
	n := sampleRate * durationSec
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(math.Sin(2*math.Pi*float64(freqHz)*float64(i)/float64(sampleRate)) * 28000)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	os.WriteFile(path, buf, 0644)
}

func writeSpeech(path string, sampleRate, durationSec int) {
	// Multi-tone signal: 200 Hz + 800 Hz + 2000 Hz — approximates speech spectrum.
	n := sampleRate * durationSec
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		v := int16((math.Sin(2*math.Pi*200*t)*0.5 +
			math.Sin(2*math.Pi*800*t)*0.3 +
			math.Sin(2*math.Pi*2000*t)*0.2) * 16000)
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	os.WriteFile(path, buf, 0644)
}
