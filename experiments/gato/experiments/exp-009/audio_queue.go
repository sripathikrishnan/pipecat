package main

import "context"

// AudioQueue is a simple FIFO queue for audio chunks backed by a buffered channel.
// Put is non-blocking (returns false if full), Get is blocking.
type AudioQueue struct {
	ch chan []byte
}

// newAudioQueue creates an AudioQueue with the given capacity (number of chunks).
func newAudioQueue(cap int) *AudioQueue {
	return &AudioQueue{ch: make(chan []byte, cap)}
}

// Put enqueues a chunk. Returns false if the queue is full (non-blocking).
func (q *AudioQueue) Put(chunk []byte) bool {
	select {
	case q.ch <- chunk:
		return true
	default:
		return false
	}
}

// Get dequeues a chunk, blocking until one is available or ctx is cancelled.
func (q *AudioQueue) Get(ctx context.Context) ([]byte, error) {
	select {
	case chunk := <-q.ch:
		return chunk, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Reset drains all pending chunks from the queue.
func (q *AudioQueue) Reset() {
	for {
		select {
		case <-q.ch:
		default:
			return
		}
	}
}

// Len returns the number of items currently in the queue.
func (q *AudioQueue) Len() int {
	return len(q.ch)
}
