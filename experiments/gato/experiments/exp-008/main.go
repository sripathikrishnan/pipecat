package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/pion/webrtc/v4"
)

// server holds shared resources for all sessions.
type server struct {
	vad *SileroVAD
	tts *GoogleTTS

	mu       sync.Mutex
	sessions map[*Session]struct{}
}

func newServer(ctx context.Context) (*server, error) {
	vad, err := NewSileroVAD("testdata/silero_vad.onnx")
	if err != nil {
		return nil, err
	}

	tts, err := NewGoogleTTS(ctx)
	if err != nil {
		vad.Close()
		return nil, err
	}

	return &server{
		vad:      vad,
		tts:      tts,
		sessions: make(map[*Session]struct{}),
	}, nil
}

func (srv *server) close() {
	srv.mu.Lock()
	for s := range srv.sessions {
		s.Close()
	}
	srv.mu.Unlock()
	srv.vad.Close()
	srv.tts.Close()
}

func (srv *server) addSession(s *Session) {
	srv.mu.Lock()
	srv.sessions[s] = struct{}{}
	srv.mu.Unlock()
}

func (srv *server) removeSession(s *Session) {
	srv.mu.Lock()
	delete(srv.sessions, s)
	srv.mu.Unlock()
}

func (srv *server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create PeerConnection.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Output track (bot → browser).
	outputTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "gato-exp008",
	)
	if err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err = pc.AddTrack(outputTrack); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set remote description, create answer.
	if err = pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherDone

	// Respond with answer before starting goroutines.
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		log.Printf("[offer] encode answer: %v", err)
	}

	// Create and start session.
	statusCh := make(chan StatusEvent, 32)
	sess := NewSession(pc, outputTrack, srv.vad, srv.tts, statusCh)
	srv.addSession(sess)

	ctx, cancel := context.WithCancel(context.Background())

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[pion] state: %s", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			log.Println("[session] Connected — pipeline running")
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			cancel()
			sess.Close()
			srv.removeSession(sess)
		}
	})

	// Handle incoming audio track from browser.
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			log.Println("[session] Received audio track from browser")
			go sess.handleInputTrack(ctx, track)
		}
	})

	sess.Run(ctx)
}

func main() {
	ctx := context.Background()

	srv, err := newServer(ctx)
	if err != nil {
		log.Fatalf("[main] server init: %v", err)
	}
	defer srv.close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	mux.HandleFunc("/offer", srv.handleOffer)

	log.Println("Gato EXP-008 Hello World Session")
	log.Println("Open http://localhost:8080 in Chrome/Firefox")

	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("[main] ListenAndServe: %v", err)
	}
}
