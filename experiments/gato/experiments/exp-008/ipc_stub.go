package main

import "strings"

// stubLLM echoes the first 10 words of the transcript.
// In a real pipeline this would call an LLM API and return the response text.
func stubLLM(transcript string) string {
	words := strings.Fields(transcript)
	if len(words) > 10 {
		words = words[:10]
	}
	return "Okay, I heard: " + strings.Join(words, " ")
}
