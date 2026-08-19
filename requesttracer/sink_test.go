package requesttracer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestFileSinkWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	m := newCollectors()
	s, err := newFileSink(path, 8, m)
	if err != nil {
		t.Fatalf("newFileSink: %v", err)
	}
	s.Emit(&RequestTrace{RequestID: "r1"})
	s.Emit(&RequestTrace{RequestID: "r2"})
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"requestID":"r1"`) {
		t.Fatalf("line 0 unexpected: %s", lines[0])
	}
	if got := counterValue(t, m.emitted); got != 2 {
		t.Fatalf("emitted counter = %v, want 2", got)
	}
}

// blockingSink stand-in: a fileSink pointed at a full channel to force drops.
func TestFileSinkDropsOnFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	m := newCollectors()
	s, err := newFileSink(path, 1, m)
	if err != nil {
		t.Fatalf("newFileSink: %v", err)
	}
	// Stall the writer so the buffer cannot drain, then overfill it.
	// The single buffer slot + the in-flight item the writer may hold means a
	// burst well past capacity guarantees at least one drop.
	for i := 0; i < 1000; i++ {
		s.Emit(&RequestTrace{RequestID: "x"})
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := counterValue(t, m.dropped); got == 0 {
		t.Fatalf("expected drops under a 1000-item burst into a depth-1 buffer, got 0")
	}
}

func TestNopSink(t *testing.T) {
	var s Sink = nopSink{}
	s.Emit(&RequestTrace{RequestID: "r1"})
	if err := s.Close(); err != nil {
		t.Fatalf("nop close: %v", err)
	}
}
