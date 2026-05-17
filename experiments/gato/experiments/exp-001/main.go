package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// pionWriter wraps a Pion TrackLocalStaticSample to satisfy AudioWriter.
type pionWriter struct {
	track *webrtc.TrackLocalStaticSample
}

func (p *pionWriter) WriteAudio(pcm []byte) error {
	return p.track.WriteSample(media.Sample{
		Data:     pcm,
		Duration: chunkDuration,
	})
}

// logObserver logs bot-speaking events and records timing for latency measurement.
type logObserver struct {
	mu            sync.Mutex
	startedAt     time.Time
	interruptAt   time.Time // set just before HandleInterruption() in scenarios
	interruptSent bool
	nStopped      int
}

func (o *logObserver) OnBotStartedSpeaking() {
	log.Println("[observer] BotStartedSpeaking ↑↓")
}

func (o *logObserver) OnBotStoppedSpeaking() {
	o.mu.Lock()
	o.nStopped++
	it := o.interruptAt
	sent := o.interruptSent
	o.mu.Unlock()

	if sent && !it.IsZero() {
		latency := time.Since(it)
		log.Printf("[observer] BotStoppedSpeaking ↑↓ (interrupt latency: %v)", latency)
	} else {
		log.Println("[observer] BotStoppedSpeaking ↑↓")
	}
}

func (o *logObserver) OnEndFrame() {
	log.Println("[observer] EndFrame received — session shutting down")
}

func (o *logObserver) setInterruptTime() {
	o.mu.Lock()
	o.interruptAt = time.Now()
	o.interruptSent = true
	o.mu.Unlock()
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/offer", handleOffer)
	http.HandleFunc("/interrupt", handleInterruptGlobal)

	log.Printf("EXP-001 server listening on %s", *addr)
	log.Printf("Open http://localhost%s in a browser to run Scenario 1 & 2", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}

// globalTransport holds the active transport for /interrupt endpoint.
// Single-session only — this is an experiment, not production code.
var (
	globalMu        sync.Mutex
	globalTransport *OutputTransport
	globalObs       *logObserver
)

func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create Pion PeerConnection.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Add audio track.
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "gato-exp001",
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err = pc.AddTrack(audioTrack); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set remote description, create answer.
	if err = pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherDone

	// Respond with answer.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())

	// Build output transport + observer, start playing.
	obs := &logObserver{}
	ot := NewOutputTransport(&pionWriter{track: audioTrack}, obs)

	globalMu.Lock()
	if globalTransport != nil {
		globalTransport.Close()
	}
	globalTransport = ot
	globalObs = obs
	globalMu.Unlock()

	// Scenario 1 + 2: play a 2-second TTS blob then loop.
	go func() {
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			log.Printf("[pion] Connection state: %s", s)
			if s == webrtc.PeerConnectionStateConnected {
				log.Println("[pion] Connected — starting audio playback scenario")
				runScenario(ot, obs)
			}
		})
	}()
}

// runScenario plays the 2-second audio scenario (Scenarios 1 & 2 from EXP-001).
func runScenario(ot *OutputTransport, obs *logObserver) {
	// Generate 2000 ms of synthetic speech (440 Hz sine wave, s16le, 48 kHz mono).
	audio := generateSine(440, 2000)
	log.Printf("[scenario] Feeding %d bytes of 2000 ms audio", len(audio))

	startTime := time.Now()
	ot.HandleAudioFrame(&TTSAudioFrame{
		AudioFrame: AudioFrame{Audio: audio, SampleRate: sampleRate},
		Text:       "Hello from EXP-001",
	})
	ot.HandleTTSStopped()

	// Wait for playback to complete (with 5-second max).
	for i := 0; i < 500; i++ {
		time.Sleep(10 * time.Millisecond)
		if obs.stoppedCount() > 0 {
			break
		}
	}
	elapsed := time.Since(startTime)
	log.Printf("[scenario] Playback complete in %v (expected ~2s)", elapsed)
}

func handleInterruptGlobal(w http.ResponseWriter, r *http.Request) {
	globalMu.Lock()
	ot := globalTransport
	obs := globalObs
	globalMu.Unlock()

	if ot == nil {
		http.Error(w, "no active session", http.StatusNotFound)
		return
	}

	obs.setInterruptTime()
	ot.HandleInterruption()
	fmt.Fprintln(w, "interrupt sent")
}

// generateSine produces n milliseconds of 440 Hz sine at 48 kHz s16le mono.
func generateSine(freqHz float64, durationMs int) []byte {
	samples := sampleRate * durationMs / 1000
	buf := make([]byte, samples*bytesPerSample)
	for i := 0; i < samples; i++ {
		val := int16(math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)) * 16000)
		buf[i*2] = byte(val)
		buf[i*2+1] = byte(val >> 8)
	}
	return buf
}

// stoppedCount returns how many times BotStoppedSpeaking has fired.
func (o *logObserver) stoppedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.nStopped
}

// Ensure main package compiles even when run with `go test`.
var _ = os.Getenv
