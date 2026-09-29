// Package requesttracer is an EPP request-control plugin that assembles a
// per-request trace -- placement, per-candidate scores and datalayer signals,
// flow-control wait time, and response timings -- and emits each completed trace
// to a pluggable sink (default: an async JSONL file writer). It observes only;
// it never alters routing.
package requesttracer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwkrc "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

// PluginType is the config `type:` string that selects this plugin.
const PluginType = "request-tracer"

// arrivalTimeKey is the request-attribute key under which the tracer stamps the
// arrival time in RequestHeader and reads it back in PreRequest. Keeping arrival
// on the request (rather than only in the tracer's store) makes it part of the
// request's own attribute set and a single source of truth.
var arrivalTimeKey = fwkplugin.NewDataKey("ArrivalTimeDataKey", PluginType)

// Config is the plugin's YAML parameters.
type Config struct {
	// Sink selects the output: "file" (default) or "nop".
	Sink string `json:"sink,omitempty"`
	// FilePath is the JSONL output path when Sink is "file".
	FilePath string `json:"filePath,omitempty"`
	// BufferSize is the sink channel depth; traces are dropped when it is full.
	BufferSize int `json:"bufferSize,omitempty"`
	// StaleTraceTTLSeconds bounds how long an unfinished trace is retained before
	// the sweeper emits it as incomplete.
	StaleTraceTTLSeconds float64 `json:"staleTraceTtlSeconds,omitempty"`
	// SweepSeconds is the sweeper interval.
	SweepSeconds float64 `json:"sweepSeconds,omitempty"`
}

// DefaultConfig returns the defaults overlaid before decoding parameters.
func DefaultConfig() Config {
	return Config{
		Sink:                 "file",
		FilePath:             "/var/log/epp/traces.jsonl",
		BufferSize:           4096,
		StaleTraceTTLSeconds: 300,
		SweepSeconds:         60,
	}
}

func (c Config) validate() error {
	switch c.Sink {
	case "file", "nop":
	default:
		return fmt.Errorf("sink must be 'file' or 'nop', got %q", c.Sink)
	}
	if c.Sink == "file" && c.FilePath == "" {
		return fmt.Errorf("filePath is required when sink is 'file'")
	}
	if c.BufferSize <= 0 {
		return fmt.Errorf("bufferSize must be > 0, got %d", c.BufferSize)
	}
	if c.StaleTraceTTLSeconds <= 0 {
		return fmt.Errorf("staleTraceTtlSeconds must be > 0, got %v", c.StaleTraceTTLSeconds)
	}
	if c.SweepSeconds <= 0 {
		return fmt.Errorf("sweepSeconds must be > 0, got %v", c.SweepSeconds)
	}
	return nil
}

// Plugin observes the request lifecycle and emits per-request traces.
type Plugin struct {
	name    string
	sink    Sink
	store   *store
	metrics *collectors
	now     func() time.Time
}

var (
	_ fwkrc.RequestHeaderProcessor  = &Plugin{}
	_ fwkrc.PreRequest              = &Plugin{}
	_ fwkrc.ResponseHeaderProcessor = &Plugin{}
	_ fwkrc.ResponseBodyProcessor   = &Plugin{}
	_ fwksched.Filter               = &Plugin{}
)

// Factory instantiates the plugin from config. Matches fwkplugin.FactoryFunc.
func Factory(name string, parameters *json.Decoder, handle fwkplugin.Handle) (fwkplugin.Plugin, error) {
	cfg := DefaultConfig()
	if parameters != nil {
		if err := parameters.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("invalid config for %s plugin %q: %w", PluginType, name, err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s plugin %q: %w", PluginType, name, err)
	}

	metrics := newCollectors()

	var sink Sink
	switch cfg.Sink {
	case "nop":
		sink = nopSink{}
	default:
		fs, err := newFileSink(cfg.FilePath, cfg.BufferSize, metrics)
		if err != nil {
			return nil, fmt.Errorf("%s plugin %q: %w", PluginType, name, err)
		}
		sink = fs
	}

	p := &Plugin{
		name:    name,
		sink:    sink,
		store:   newStore(),
		metrics: metrics,
		now:     time.Now,
	}

	if handle != nil {
		if reg := handle.Metrics(); reg != nil {
			for _, c := range metrics.all() {
				reg.MustRegister(c)
			}
		}
		ttl := time.Duration(cfg.StaleTraceTTLSeconds * float64(time.Second))
		interval := time.Duration(cfg.SweepSeconds * float64(time.Second))
		go p.runSweeper(handle.Context(), interval, ttl)
	}
	return p, nil
}

func (p *Plugin) TypedName() fwkplugin.TypedName {
	return fwkplugin.TypedName{Type: PluginType, Name: p.name}
}

// RequestHeader starts the trace and stamps the arrival time as a request
// attribute (read back at PreRequest). FairnessID is not read here -- it is still
// unresolved at this point.
func (p *Plugin) RequestHeader(_ context.Context, request *fwksched.InferenceRequest) error {
	if request == nil || request.RequestID == "" {
		// Without a RequestID the tracer cannot correlate hooks; every empty-ID
		// request would collide on the same store entry. Skip rather than clobber.
		return nil
	}
	now := p.now()
	request.PutAttribute(arrivalTimeKey, now)
	e := p.store.getOrCreate(request.RequestID, now)
	e.with(now, func(t *RequestTrace) {
		t.TargetModel = request.TargetModel
		t.ArrivalTime = now
	})
	if p.metrics != nil {
		p.metrics.inProgress.Inc()
	}
	return nil
}

// Filter records the candidate set as it enters the scheduling profile and
// returns it unchanged. Referencing this plugin FIRST in a profile's plugin list
// is what makes the endpoints a filter later removes visible at all: the
// scheduling result carries only what survived.
//
// It never drops anything, so it is safe at any position; earlier simply sees
// more.
func (p *Plugin) Filter(_ context.Context, request *fwksched.InferenceRequest, endpoints []fwksched.Endpoint) []fwksched.Endpoint {
	if request == nil || request.RequestID == "" {
		return endpoints
	}
	now := p.now()
	snap := snapshotEndpoints(endpoints)
	e := p.store.getOrCreate(request.RequestID, now)
	e.with(now, func(t *RequestTrace) {
		// A request can run several profiles; keep the first, widest set.
		if t.EntryCandidates == nil {
			t.EntryCandidates = snap
		}
	})
	return endpoints
}

// Consumes declares the endpoint attributes the Filter hook reads. Declaring is
// mandatory, not optional: the framework hands filters scope-wrapped endpoints,
// where Get on an undeclared key is rejected and Keys() hides it, so an
// undeclared tracer would record empty attribute bags.
//
// All Optional: a missing producer must degrade the trace, never fail the run.
func (p *Plugin) Consumes() fwkplugin.DataDependencies {
	return fwkplugin.DataDependencies{
		Optional: map[fwkplugin.DataKey]any{
			attrprefix.PrefixCacheMatchInfoDataKey:       attrprefix.PrefixCacheMatchInfo{},
			attrconcurrency.InFlightLoadDataKey:          attrconcurrency.InFlightLoad{},
			attrconcurrency.UncachedRequestTokensDataKey: attrconcurrency.UncachedRequestTokens{},
		},
	}
}

// PreRequest records the resolved FairnessID, the scheduling outcome (winner,
// per-candidate final scores + metrics + attributes), the queue-wait if a
// fairness plugin published it, the request's own attribute store, and the
// dispatch time.
func (p *Plugin) PreRequest(_ context.Context, request *fwksched.InferenceRequest, result *fwksched.SchedulingResult) error {
	if request == nil || request.RequestID == "" {
		return nil
	}
	now := p.now()
	e := p.store.getOrCreate(request.RequestID, now)

	// Arrival is read from the request attribute (source of truth), falling back
	// to whatever the store holds if RequestHeader never ran for this request.
	arrival, hasArrival := fwksched.ReadRequestAttribute[time.Time](request, arrivalTimeKey)
	reqAttrs := dumpRequestAttributes(request)
	candidates, winner := candidatesFrom(result)

	e.with(now, func(t *RequestTrace) {
		t.FairnessID = request.FairnessID
		t.DispatchTime = now
		t.WinnerPod = winner
		t.Candidates = candidates
		t.ReqAttributes = reqAttrs
		if hasArrival && !arrival.IsZero() {
			t.ArrivalTime = arrival
		}
		// TotalEppMs: dispatch-arrival. We own both timestamps, so this is always
		// available and never depends on another plugin.
		if !t.ArrivalTime.IsZero() {
			total := durationMs(t.ArrivalTime, now)
			t.TotalEppMs = &total
		}
	})
	return nil
}

// durationMs returns end-start in fractional milliseconds.
func durationMs(start, end time.Time) float64 {
	return float64(end.Sub(start).Microseconds()) / 1000.0
}

// ResponseHeader records the response-start (headers received) time, the HTTP
// status, and the pod that actually served the response. HeadersTime is not
// first-token; that is captured from the first streamed body chunk.
func (p *Plugin) ResponseHeader(_ context.Context, request *fwksched.InferenceRequest, response *fwkrc.Response, served *fwkdl.EndpointMetadata) {
	if request == nil || request.RequestID == "" || response == nil {
		return
	}
	now := p.now()
	status := responseStatus(response.Headers)
	servedPod := ""
	if served != nil {
		servedPod = served.ID.String()
	}
	if e := p.store.get(request.RequestID); e != nil {
		e.with(now, func(t *RequestTrace) {
			if t.HeadersTime.IsZero() {
				t.HeadersTime = now
			}
			if status != "" {
				t.ResponseStatus = status
			}
			if servedPod != "" && servedPod != "/" {
				t.ServedPod = servedPod
			}
		})
	}
}

// responseStatus pulls the HTTP status from response headers, tolerating the
// HTTP/2 pseudo-header form and casing variants.
func responseStatus(h map[string]string) string {
	for _, k := range []string{":status", "status", ":Status"} {
		if v, ok := h[k]; ok {
			return v
		}
	}
	return ""
}

// ResponseBody records first-token on the first StartOfStream chunk, and
// finalizes + emits the trace on EndOfStream.
func (p *Plugin) ResponseBody(_ context.Context, request *fwksched.InferenceRequest, response *fwkrc.Response, _ *fwkdl.EndpointMetadata) {
	if request == nil || request.RequestID == "" || response == nil {
		return
	}
	now := p.now()
	e := p.store.get(request.RequestID)
	if e == nil {
		return
	}

	if response.StartOfStream {
		e.with(now, func(t *RequestTrace) {
			if t.FirstTokenTime.IsZero() {
				t.FirstTokenTime = now
			}
		})
	}

	if !response.EndOfStream {
		return
	}

	var out RequestTrace
	e.with(now, func(t *RequestTrace) {
		t.CompletionTime = now
		t.Usage = Usage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		}
		if d := response.Usage.PromptTokenDetails; d != nil {
			cached := d.CachedTokens
			t.Usage.CachedTokens = &cached
		}
		out = *t
	})
	p.store.delete(request.RequestID)
	if p.metrics != nil {
		p.metrics.inProgress.Dec()
	}
	p.sink.Emit(&out)
}

// runSweeper is the primary finalize path for aborted requests: a request that
// dispatches but never reaches EndOfStream is emitted here as Incomplete once it
// has been idle longer than ttl.
func (p *Plugin) runSweeper(ctx context.Context, interval, ttl time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweep(ttl)
		}
	}
}

func (p *Plugin) sweep(ttl time.Duration) {
	now := p.now()
	p.store.m.Range(func(key, value any) bool {
		e := value.(*inProgress)
		var emit *RequestTrace
		e.mu.Lock()
		if now.Sub(e.lastTouch) > ttl {
			e.trace.Incomplete = true
			cp := *e.trace
			emit = &cp
		}
		e.mu.Unlock()
		if emit != nil {
			p.store.m.Delete(key)
			if p.metrics != nil {
				p.metrics.inProgress.Dec()
				p.metrics.swept.Inc()
			}
			p.sink.Emit(emit)
		}
		return true
	})
}

// Close releases the sink. Not part of the plugin interface; call from the
// consumer if it manages lifecycle explicitly (otherwise the process exit
// flushes via the OS).
func (p *Plugin) Close() error { return p.sink.Close() }
