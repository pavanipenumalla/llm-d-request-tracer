package requesttracer

import (
	"encoding/json"
	"sync"
	"time"
)

// RequestTrace is the per-request record emitted once a request completes (or is
// swept). It is the serialized event; all timestamps are wall-clock.
type RequestTrace struct {
	RequestID   string `json:"requestID"`
	FairnessID  string `json:"fairnessID,omitempty"`
	TargetModel string `json:"targetModel,omitempty"`
	// ArrivalTime is when the tracer first saw the request. Stamped as a request
	// attribute in RequestHeader and read back from there, so it is part of the
	// request's own attribute store (also visible in ReqAttributes).
	ArrivalTime time.Time `json:"arrivalTime"`
	// DispatchTime is when scheduling completed and the request was sent onward.
	DispatchTime time.Time `json:"dispatchTime,omitempty"`
	// HeadersTime is when response headers arrived (response start), not first token.
	HeadersTime time.Time `json:"headersTime,omitempty"`
	// FirstTokenTime is the first streamed body chunk (StartOfStream).
	FirstTokenTime time.Time `json:"firstTokenTime,omitempty"`
	CompletionTime time.Time `json:"completionTime,omitempty"`
	// TotalEppMs is DispatchTime-ArrivalTime: the full time the request spent in
	// the EPP before dispatch (header processing + admission/flow-control wait +
	// scheduling). Always set once dispatched; the tracer owns both timestamps,
	// so it never depends on another plugin.
	TotalEppMs *float64 `json:"totalEppMs,omitempty"`
	// Incomplete is true when the trace was finalized by the staleness sweeper
	// rather than by a clean end-of-stream (e.g. an aborted request).
	Incomplete bool   `json:"incomplete,omitempty"`
	WinnerPod  string `json:"winnerPod,omitempty"`
	// ServedPod is the endpoint that actually served the response, from the
	// targetEndpoint passed to the response hooks. Normally equals WinnerPod;
	// captured separately to confirm the scheduled winner is who replied.
	ServedPod string `json:"servedPod,omitempty"`
	// ResponseStatus is the HTTP status from the response headers (e.g. "200"),
	// when present. Empty if the header was not surfaced.
	ResponseStatus string `json:"responseStatus,omitempty"`
	// EntryCandidates is every endpoint the scheduler started with, captured by
	// the Filter hook before any filter narrowed the set. Candidates below holds
	// only what survived, so the difference is what the filters removed. Empty
	// unless this plugin is referenced from a schedulingProfile. FinalScore is
	// always 0 here: scoring runs after filtering, so a removed endpoint never
	// had one.
	EntryCandidates []CandidateTrace `json:"entryCandidates,omitempty"`
	Candidates      []CandidateTrace `json:"candidates,omitempty"`
	Usage           Usage            `json:"usage"`
	// ReqAttributes is every entry in the request's own attribute store -- the
	// arrival-time the tracer stamps, plus whatever other plugins published
	// (e.g. agent-identity).
	ReqAttributes map[string]json.RawMessage `json:"reqAttributes,omitempty"`
}

// Usage mirrors the token counts parsed from the response.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// CandidateTrace records one scheduling candidate: its final weighted score, a
// snapshot of its metrics, and every datalayer attribute present on it.
type CandidateTrace struct {
	Pod        string  `json:"pod"`
	FinalScore float64 `json:"finalScore"`
	IsWinner   bool    `json:"isWinner"`
	// Metrics is the whole Endpoint.GetMetrics() snapshot.
	Metrics *MetricsSnapshot `json:"metrics,omitempty"`
	// Attributes is every datalayer attribute on the endpoint, keyed by
	// DataKey.String() ("dataType/producerName").
	Attributes map[string]json.RawMessage `json:"attributes,omitempty"`
	// AttrSource records provenance per attribute key: "read" (verbatim from the
	// producer) or "derived" (computed by the tracer). Build 1 emits only "read".
	AttrSource map[string]string `json:"attrSource,omitempty"`

	// Convenience projections of well-known attributes, matched by concrete Go
	// type. Nil when the producing plugin is absent.
	UncachedRequestTokens *int64 `json:"uncachedRequestTokens,omitempty"`
	PrefixMatchBlocks     *int   `json:"prefixMatchBlocks,omitempty"`
	PrefixTotalBlocks     *int   `json:"prefixTotalBlocks,omitempty"`
	InFlightTokens        *int64 `json:"inFlightTokens,omitempty"`
}

// MetricsSnapshot is the JSON-friendly projection of datalayer.Metrics.
type MetricsSnapshot struct {
	ActiveModels            map[string]int `json:"activeModels,omitempty"`
	WaitingModels           map[string]int `json:"waitingModels,omitempty"`
	MaxActiveModels         int            `json:"maxActiveModels"`
	RunningRequestsSize     int            `json:"runningRequestsSize"`
	WaitingQueueSize        int            `json:"waitingQueueSize"`
	KVCacheUsagePercent     float64        `json:"kvCacheUsagePercent"`
	KvCacheMaxTokenCapacity int            `json:"kvCacheMaxTokenCapacity"`
	CacheBlockSize          int            `json:"cacheBlockSize"`
	CacheNumBlocks          int            `json:"cacheNumBlocks"`
	UpdateTime              time.Time      `json:"updateTime"`
}

// inProgress is one request's trace while it is still being assembled, plus the
// mutex guarding it. Hooks for one request run on more than one goroutine
// (header/prerequest on the request goroutine, body chunks on a per-request
// queue goroutine), so every field access goes through mu.
type inProgress struct {
	mu    sync.Mutex
	trace *RequestTrace
	// lastTouch is updated on every hook, used by the staleness sweeper.
	lastTouch time.Time
}

// store holds in-progress traces keyed by RequestID. sync.Map because entries
// are created/read/deleted from multiple goroutines.
type store struct {
	m sync.Map // key: string (RequestID), value: *inProgress
}

func newStore() *store { return &store{} }

// getOrCreate returns the in-progress entry for id, creating an empty one if
// absent. ArrivalTime is set by the caller (from the request attribute), not here.
func (s *store) getOrCreate(id string, now time.Time) *inProgress {
	if v, ok := s.m.Load(id); ok {
		return v.(*inProgress)
	}
	fresh := &inProgress{
		trace:     &RequestTrace{RequestID: id},
		lastTouch: now,
	}
	actual, _ := s.m.LoadOrStore(id, fresh)
	return actual.(*inProgress)
}

// get returns the in-progress entry for id, or nil if absent.
func (s *store) get(id string) *inProgress {
	if v, ok := s.m.Load(id); ok {
		return v.(*inProgress)
	}
	return nil
}

func (s *store) delete(id string) { s.m.Delete(id) }

// with runs fn under the entry's lock, refreshing lastTouch.
func (p *inProgress) with(now time.Time, fn func(t *RequestTrace)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastTouch = now
	fn(p.trace)
}
