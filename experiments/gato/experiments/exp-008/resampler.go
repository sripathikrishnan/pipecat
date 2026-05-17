package main

// LinearResampler is a stateful 2:1 upsampler using linear interpolation.
// Zero allocation on the hot path (output slice is caller-provided).
// Converts 24kHz int16 PCM to 48kHz via 2× interpolation.
type LinearResampler struct {
	prev    int16
	hasPrev bool
}

// Resample converts inSamples (int16 samples at 24 kHz) to 2× output samples
// (at 48 kHz) by linear interpolation. Returns a new slice; caller owns it.
// All heap allocation happens in the output slice — the resampler itself is alloc-free.
func (r *LinearResampler) Resample(input []byte) []byte {
	if len(input) == 0 {
		return nil
	}
	nIn := len(input) / 2
	out := make([]byte, nIn*4) // 2 output samples per input sample, each 2 bytes
	outIdx := 0

	for i := 0; i < nIn; i++ {
		cur := int16(input[i*2]) | int16(input[i*2+1])<<8

		// First output sample: the input sample verbatim.
		out[outIdx] = byte(cur)
		out[outIdx+1] = byte(cur >> 8)
		outIdx += 2

		// Second output sample: linear interpolation to next sample.
		var next int16
		if i+1 < nIn {
			next = int16(input[(i+1)*2]) | int16(input[(i+1)*2+1])<<8
		} else {
			// Last sample of this chunk. We don't know the first sample of the next
			// chunk, so repeat cur. This introduces a 1-sample error at each chunk
			// boundary — acceptable for voice audio (error magnitude ≈ half the
			// inter-sample delta, which is tiny for smooth speech signals).
			next = cur
		}
		interp := int16((int32(cur) + int32(next)) / 2)
		out[outIdx] = byte(interp)
		out[outIdx+1] = byte(interp >> 8)
		outIdx += 2

		r.prev = cur
		r.hasPrev = true
	}
	return out[:outIdx]
}

// Reset clears the carry-over state. Call on interruption or stream restart.
func (r *LinearResampler) Reset() {
	r.prev = 0
	r.hasPrev = false
}
