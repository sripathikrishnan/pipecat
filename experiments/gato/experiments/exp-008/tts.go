package main

import (
	"context"
	"fmt"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
)

// GoogleTTS synthesizes text to 24kHz mono int16 PCM using Google Cloud TTS.
// Uses Application Default Credentials (no explicit key file required).
type GoogleTTS struct {
	client *texttospeech.Client
}

// NewGoogleTTS creates a GoogleTTS client using Application Default Credentials.
// The caller must call Close() when done.
func NewGoogleTTS(ctx context.Context) (*GoogleTTS, error) {
	client, err := texttospeech.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("texttospeech.NewClient: %w", err)
	}
	return &GoogleTTS{client: client}, nil
}

// Close releases TTS client resources.
func (t *GoogleTTS) Close() {
	t.client.Close()
}

// Synthesize converts text to raw 24kHz mono LINEAR16 PCM bytes.
// The returned bytes are int16 little-endian samples at 24000 Hz, mono.
func (t *GoogleTTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{
				Text: text,
			},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: "en-US",
			Name:         "en-US-Neural2-F",
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			AudioEncoding:   texttospeechpb.AudioEncoding_LINEAR16,
			SampleRateHertz: 24000,
		},
	}

	resp, err := t.client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("SynthesizeSpeech: %w", err)
	}

	return resp.AudioContent, nil
}
