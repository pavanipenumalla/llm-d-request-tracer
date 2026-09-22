package requesttracer

import (
	"context"
	"sync"
	"testing"
	"time"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwkrc "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	requesthandling "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	"k8s.io/apimachinery/pkg/types"
)

// captureSink records emitted traces for assertions.
type captureSink struct {
	mu     sync.Mutex
	traces []*RequestTrace
}

func (c *captureSink) Emit(t *RequestTrace) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traces = append(c.traces, t)
}
func (c *captureSink) Close() error { return nil }
func (c *captureSink) all() []*RequestTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*RequestTrace(nil), c.traces...)
}

// fakeClock returns preset times in sequence, holding the last one.
type fakeClock struct {
	mu    sync.Mutex
	times []time.Time
	i     int
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.i < len(f.times) {
		t := f.times[f.i]
		f.i++
		return t
	}
	return f.times[len(f.times)-1]
}

func newTestPlugin(sink Sink, clock func() time.Time) *Plugin {
	return &Plugin{
		name:    "test",
		sink:    sink,
		store:   newStore(),
		metrics: newCollectors(),
		now:     clock,
	}
}

func resultWith(winner string, candidates ...fwksched.Endpoint) *fwksched.SchedulingResult {
	scored := make([]fwksched.ScoredEndpoint, len(candidates))
	var targets []fwksched.Endpoint
	for i, ep := range candidates {
		scored[i] = fwksched.ScoredEndpoint{Endpoint: ep, Score: float64(i) + 0.5}
		if podID(ep) == winner {
			targets = append(targets, ep)
		}
	}
	return &fwksched.SchedulingResult{
		PrimaryProfileName: "default",
		ProfileResults: map[string]*fwksched.ProfileRunResult{
			"default": {TargetEndpoints: targets, ScoredCandidates: scored},
		},
	}
}

func TestFullLifecycleEmitsOnceOrdered(t *testing.T) {
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{times: []time.Time{
		base,                       // RequestHeader (arrival)
		base.Add(50 * time.Millisecond),  // PreRequest (dispatch)
		base.Add(120 * time.Millisecond), // ResponseHeader (headers)
		base.Add(130 * time.Millisecond), // ResponseBody StartOfStream (first token)
		base.Add(400 * time.Millisecond), // ResponseBody EndOfStream (completion)
	}}
	sink := &captureSink{}
	p := newTestPlugin(sink, clock.now)

	epA := mkEndpoint("ns/pod-a", map[fwkplugin.DataKey]fwkdl.Cloneable{
		attrprefix.PrefixCacheMatchInfoDataKey: attrprefix.NewPrefixCacheMatchInfo(2, 8, 64),
	})
	epB := mkEndpoint("ns/pod-b", nil)

	req := &fwksched.InferenceRequest{RequestID: "req-1", TargetModel: "m", FairnessID: "sess-1"}

	if err := p.RequestHeader(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := p.PreRequest(context.Background(), req, resultWith("ns/pod-a", epA, epB)); err != nil {
		t.Fatal(err)
	}
	p.ResponseHeader(context.Background(), req, &fwkrc.Response{
		RequestID: "req-1",
		Headers:   map[string]string{":status": "200"},
	}, &fwkdl.EndpointMetadata{ID: types.NamespacedName{Namespace: "ns", Name: "pod-a"}})
	p.ResponseBody(context.Background(), req, &fwkrc.Response{RequestID: "req-1", StartOfStream: true}, nil)
	p.ResponseBody(context.Background(), req, &fwkrc.Response{
		RequestID:   "req-1",
		EndOfStream: true,
		Usage:       requesthandling.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil)

	traces := sink.all()
	if len(traces) != 1 {
		t.Fatalf("expected exactly 1 emitted trace, got %d", len(traces))
	}
	tr := traces[0]

	if tr.FairnessID != "sess-1" {
		t.Fatalf("FairnessID = %q", tr.FairnessID)
	}
	if tr.WinnerPod != "ns/pod-a" {
		t.Fatalf("WinnerPod = %q", tr.WinnerPod)
	}
	if !(tr.ArrivalTime.Before(tr.DispatchTime) &&
		tr.DispatchTime.Before(tr.HeadersTime) &&
		tr.HeadersTime.Before(tr.FirstTokenTime) &&
		tr.FirstTokenTime.Before(tr.CompletionTime)) {
		t.Fatalf("timestamps not strictly ordered: %+v", tr)
	}
	if tr.Usage.TotalTokens != 15 {
		t.Fatalf("usage not captured: %+v", tr.Usage)
	}
	if len(tr.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(tr.Candidates))
	}
	var winnerSeen bool
	for _, c := range tr.Candidates {
		if c.Pod == "ns/pod-a" {
			winnerSeen = c.IsWinner
			if c.PrefixMatchBlocks == nil || *c.PrefixMatchBlocks != 2 {
				t.Fatalf("pod-a prefix projection wrong: %v", c.PrefixMatchBlocks)
			}
		}
	}
	if !winnerSeen {
		t.Fatalf("winner not flagged on pod-a")
	}
	// TotalEppMs is always set: dispatch(50ms) - arrival(0) = 50ms.
	if tr.TotalEppMs == nil {
		t.Fatalf("TotalEppMs must always be set once dispatched")
	}
	if *tr.TotalEppMs < 49 || *tr.TotalEppMs > 51 {
		t.Fatalf("TotalEppMs = %v, want ~50", *tr.TotalEppMs)
	}
	// Arrival was stamped as a request attribute in RequestHeader.
	if _, ok := tr.ReqAttributes[arrivalTimeKey.String()]; !ok {
		t.Fatalf("arrival-time attribute missing from ReqAttributes")
	}
	if tr.Incomplete {
		t.Fatalf("clean completion must not be Incomplete")
	}
	if tr.ResponseStatus != "200" {
		t.Fatalf("ResponseStatus = %q, want 200", tr.ResponseStatus)
	}
	if tr.ServedPod != "ns/pod-a" {
		t.Fatalf("ServedPod = %q, want ns/pod-a", tr.ServedPod)
	}
}

// TestArrivalFromAttribute confirms arrival is stamped as a request attribute in
// RequestHeader and read back in PreRequest to compute TotalEppMs.
func TestArrivalFromAttribute(t *testing.T) {
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{times: []time.Time{
		base,                             // RequestHeader (arrival)
		base.Add(200 * time.Millisecond), // PreRequest (dispatch)
	}}
	sink := &captureSink{}
	p := newTestPlugin(sink, clock.now)

	req := &fwksched.InferenceRequest{RequestID: "req-2"}

	_ = p.RequestHeader(context.Background(), req)

	// Arrival must now be present on the request itself.
	got, ok := fwksched.ReadRequestAttribute[time.Time](req, arrivalTimeKey)
	if !ok || !got.Equal(base) {
		t.Fatalf("arrival attribute not set on request: ok=%v got=%v", ok, got)
	}

	_ = p.PreRequest(context.Background(), req, resultWith(""))

	e := p.store.get("req-2")
	if e == nil {
		t.Fatal("trace missing after PreRequest")
	}
	e.with(clock.now(), func(tr *RequestTrace) {
		// TotalEppMs = dispatch(200ms) - arrival(0) = 200ms, always set.
		if tr.TotalEppMs == nil || *tr.TotalEppMs < 199 || *tr.TotalEppMs > 201 {
			t.Fatalf("TotalEppMs = %v, want ~200", tr.TotalEppMs)
		}
		if _, ok := tr.ReqAttributes[arrivalTimeKey.String()]; !ok {
			t.Fatalf("ReqAttributes should contain the arrival-time key")
		}
	})
}

func TestSweeperEmitsIncomplete(t *testing.T) {
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{times: []time.Time{base}}
	sink := &captureSink{}
	p := newTestPlugin(sink, clock.now)

	req := &fwksched.InferenceRequest{RequestID: "req-3"}
	_ = p.RequestHeader(context.Background(), req)
	_ = p.PreRequest(context.Background(), req, resultWith(""))
	// never completes; advance the clock past ttl by switching the clock source.
	p.now = func() time.Time { return base.Add(10 * time.Minute) }

	p.sweep(5 * time.Minute)

	traces := sink.all()
	if len(traces) != 1 {
		t.Fatalf("expected 1 swept trace, got %d", len(traces))
	}
	if !traces[0].Incomplete {
		t.Fatalf("swept trace must be Incomplete")
	}
	if p.store.get("req-3") != nil {
		t.Fatalf("swept trace not deleted from store")
	}
}

func TestInterfaceAssertions(t *testing.T) {
	// Compile-time already enforces these; this makes the intent explicit.
	var _ fwkrc.RequestHeaderProcessor = &Plugin{}
	var _ fwkrc.PreRequest = &Plugin{}
	var _ fwkrc.ResponseHeaderProcessor = &Plugin{}
	var _ fwkrc.ResponseBodyProcessor = &Plugin{}
	var _ fwkplugin.Plugin = &Plugin{}
}
