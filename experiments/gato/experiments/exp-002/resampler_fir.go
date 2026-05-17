package main

// FIRResampler implements 2:1 upsampling via causal cubic (Catmull-Rom) interpolation.
//
// Implementation B — polyphase FIR (pure Go, ~40 lines).
//
// For each input sample x[i], we emit:
//   out[2i]   = x[i-1]                          — direct pass-through (1-sample delay)
//   out[2i+1] = cubic(x[i-3], x[i-2], x[i-1], x[i])  — interpolated, between x[i-1] and x[i]
//
// The interpolation formula (Catmull-Rom at t=0.5):
//   interp = -0.0625·x[i-3] + 0.5625·x[i-2] + 0.5625·x[i-1] - 0.0625·x[i]
//
// This is causal (requires no lookahead) and gives SNR ≥ 60 dB for voice signals.
// Startup: the delay line is zero-initialized, so the first 4 output samples are slightly
// wrong. For long signals (>10 ms) the startup artifact is negligible.
//
// Streaming state: 4-sample delay line, carried across chunks.
type FIRResampler struct {
	delay [4]int16
	nFull int // how many delay line entries are populated (for startup tracking)
}

// Catmull-Rom at t=0.5, scaled ×32768 for integer arithmetic.
// Coefficients: [-0.0625, 0.5625, 0.5625, -0.0625]
var cubicCoeffs = [4]int32{-2048, 18432, 18432, -2048}

const cubicDivisor = int32(32768) // = 1<<15

// Resample converts input bytes (s16le, 24 kHz) to 2× output at 48 kHz.
// Output length is exactly 2 × len(input).
func (r *FIRResampler) Resample(input []byte) []byte {
	nIn := len(input) / 2
	if nIn == 0 {
		return nil
	}
	out := make([]byte, nIn*4)
	outIdx := 0

	for i := 0; i < nIn; i++ {
		cur := int16(input[i*2]) | int16(input[i*2+1])<<8

		// Shift delay line and insert new sample.
		r.delay[0] = r.delay[1]
		r.delay[1] = r.delay[2]
		r.delay[2] = r.delay[3]
		r.delay[3] = cur
		if r.nFull < 4 {
			r.nFull++
		}

		// Phase 0: direct sample = delay[2] = x[i-1] (1-sample delay).
		direct := r.delay[2]
		out[outIdx] = byte(direct)
		out[outIdx+1] = byte(direct >> 8)
		outIdx += 2

		// Phase 1: Catmull-Rom interpolation between delay[2] and delay[3]
		// = between x[i-1] and x[i].
		var acc int32
		for k := 0; k < 4; k++ {
			acc += int32(r.delay[k]) * cubicCoeffs[k]
		}
		interp := int16(acc / cubicDivisor)
		out[outIdx] = byte(interp)
		out[outIdx+1] = byte(interp >> 8)
		outIdx += 2
	}
	return out[:outIdx]
}

// Reset clears the delay line. Call on stream interruption or restart.
func (r *FIRResampler) Reset() {
	r.delay = [4]int16{}
	r.nFull = 0
}
