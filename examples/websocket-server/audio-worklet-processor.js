// audio-worklet-processor.js
class AudioStreamProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = []; // Internal buffer to store audio data
    this.playhead = 0; // Current position in the buffer
    this.port.onmessage = (event) => this.handleMessage(event); // Handle messages from the main thread
  }

  // Handle messages from the main thread
  handleMessage(event) {
    if (event.data.type === 'enqueue') {
      this.enqueueAudio(event.data.data); // Enqueue new audio data
    }
  }

  // Enqueue audio data into the buffer
  enqueueAudio(data) {
    this.buffer.push(...data); // Append the new audio data to the buffer
  }

  // Process audio (called by the Web Audio API)
  process(inputs, outputs, parameters) {
    const output = outputs[0]; // Get the output channel
    const channel = output[0]; // Get the first channel (mono audio)

    // Fill the output buffer with data from the internal buffer
    for (let i = 0; i < channel.length; i++) {
      if (this.playhead < this.buffer.length) {
        channel[i] = this.buffer[this.playhead++]; // Play the next sample
      } else {
        channel[i] = 0; // Fill with silence if no data is available
      }
    }

    // Remove played audio data from the buffer to free memory
    if (this.playhead >= this.buffer.length) {
      this.buffer = []; // Clear the buffer
      this.playhead = 0; // Reset the playhead
    }

    return true; // Keep the processor alive
  }
}

// Register the AudioWorkletProcessor
registerProcessor('audio-stream-processor', AudioStreamProcessor);