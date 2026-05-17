package main

// stubLLM is a placeholder LLM that echoes the user's transcript.
// In a real pipeline this would call an LLM API and return the response text.
func stubLLM(transcript string) string {
	return "You said: " + transcript
}
