package requesttracer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// flushInterval bounds how long a written trace can sit in the bufio buffer
// before reaching the OS. Kept short so a process kill during a long experiment
// loses at most this window of traces (page-cache bytes survive a process kill).
const flushInterval = time.Second

// Sink consumes finalized traces. Emit must not block the request hot path.
type Sink interface {
	// Emit hands a finalized trace to the sink. It must return promptly; a sink
	// that cannot keep up drops rather than blocks.
	Emit(t *RequestTrace)
	// Close flushes and releases resources. Safe to call once.
	Close() error
}

// nopSink discards everything. Used when the plugin is configured with sink=nop.
type nopSink struct{}

func (nopSink) Emit(*RequestTrace) {}
func (nopSink) Close() error       { return nil }

// fileSink serializes traces to a JSONL file from a single background goroutine.
// The hot path only does a non-blocking channel send; when the buffer is full it
// drops the trace and increments a counter, so a slow disk never stalls request
// handling.
type fileSink struct {
	ch      chan *RequestTrace
	done    chan struct{}
	f       *os.File
	w       *bufio.Writer
	metrics *collectors

	closeOnce sync.Once
}

func newFileSink(path string, bufferSize int, metrics *collectors) (*fileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open trace file %q: %w", path, err)
	}
	s := &fileSink{
		ch:      make(chan *RequestTrace, bufferSize),
		done:    make(chan struct{}),
		f:       f,
		w:       bufio.NewWriter(f),
		metrics: metrics,
	}
	go s.run()
	return s, nil
}

// Emit is non-blocking: on a full buffer it drops and counts.
func (s *fileSink) Emit(t *RequestTrace) {
	select {
	case s.ch <- t:
	default:
		if s.metrics != nil {
			s.metrics.dropped.Inc()
		}
	}
}

func (s *fileSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	dirty := false
	for {
		select {
		case t, ok := <-s.ch:
			if !ok {
				_ = s.w.Flush()
				return
			}
			s.writeOne(t)
			dirty = true
		case <-ticker.C:
			if dirty {
				_ = s.w.Flush()
				dirty = false
			}
		}
	}
}

func (s *fileSink) writeOne(t *RequestTrace) {
	b, err := json.Marshal(t)
	if err != nil {
		if s.metrics != nil {
			s.metrics.writeErrors.Inc()
		}
		return
	}
	b = append(b, '\n')
	if _, err := s.w.Write(b); err != nil {
		if s.metrics != nil {
			s.metrics.writeErrors.Inc()
		}
		return
	}
	if s.metrics != nil {
		s.metrics.emitted.Inc()
	}
}

// Close stops the writer goroutine, flushes, and closes the file. Idempotent.
func (s *fileSink) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.ch)
		<-s.done
		err = s.f.Close()
	})
	return err
}
