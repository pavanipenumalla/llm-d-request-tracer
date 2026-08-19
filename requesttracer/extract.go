package requesttracer

import (
	"encoding/json"
	"fmt"
	"reflect"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

// attrExtractor turns a Cloneable attribute value into a JSON-marshalable view.
// It exists because some attribute types (notably PrefixCacheMatchInfo) have
// only unexported fields, so a bare json.Marshal yields "{}" and loses the data
// silently. Known types are projected through their accessors; unknown types
// fall through to json.Marshal with empty-object detection.
type attrExtractor func(fwkdl.Cloneable) any

// extractorRegistry maps the concrete Go type of an attribute value to its
// projection. Type-keyed (not DataKey-keyed) so it is robust to producer-name
// renames -- DataKey's fields are unexported and unreachable externally anyway.
var extractorRegistry = map[reflect.Type]attrExtractor{
	reflect.TypeOf(&attrprefix.PrefixCacheMatchInfo{}): func(c fwkdl.Cloneable) any {
		p := c.(*attrprefix.PrefixCacheMatchInfo)
		return map[string]any{
			"matchBlocks":      p.MatchBlocks(),
			"totalBlocks":      p.TotalBlocks(),
			"blockSizeTokens":  p.BlockSizeTokens(),
			"cachedBlockCount": p.CachedBlockCount(),
		}
	},
	// InFlightLoad and UncachedRequestTokens have exported fields and marshal
	// cleanly; no entry needed. They are handled by the default path.
}

// marshalAttr serializes one attribute value. Returns the JSON and true, or a
// visible "unmarshalable: <type>" marker and false when a value that is not a
// legitimately-empty struct serializes to an empty object (the silent-loss case).
func marshalAttr(v fwkdl.Cloneable) (json.RawMessage, bool) {
	if v == nil {
		return json.RawMessage("null"), true
	}
	if ex, ok := extractorRegistry[reflect.TypeOf(v)]; ok {
		b, err := json.Marshal(ex(v))
		if err != nil {
			return unmarshalableMarker(v), false
		}
		return b, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return unmarshalableMarker(v), false
	}
	// Detect the silent-loss case: a non-empty value whose exported-field marshal
	// collapsed to an empty object. A genuinely empty struct also yields "{}",
	// but marking it costs nothing and never hides real data.
	if string(b) == "{}" {
		return unmarshalableMarker(v), false
	}
	return b, true
}

func unmarshalableMarker(v any) json.RawMessage {
	b, _ := json.Marshal(fmt.Sprintf("unmarshalable: %T", v))
	return b
}

// dumpEndpointAttributes reads every datalayer attribute present on the endpoint
// into a bag keyed by the attribute's string key ("dataType/producerName"), with
// per-key provenance, and fills the well-known typed convenience fields by
// concrete type. Endpoint attributes are keyed by string in llm-d-router.
func dumpEndpointAttributes(ep fwksched.Endpoint, ct *CandidateTrace) {
	keys := ep.Keys()
	if len(keys) == 0 {
		return
	}
	ct.Attributes = make(map[string]json.RawMessage, len(keys))
	ct.AttrSource = make(map[string]string, len(keys))
	for _, k := range keys {
		val, ok := ep.Get(k)
		if !ok || val == nil {
			continue
		}
		raw, _ := marshalAttr(val)
		ct.Attributes[k] = raw
		ct.AttrSource[k] = sourceRead
		projectKnown(val, ct)
	}
}

// projectKnown fills the typed convenience fields when val is a recognized type.
func projectKnown(val fwkdl.Cloneable, ct *CandidateTrace) {
	switch v := val.(type) {
	case *attrconcurrency.UncachedRequestTokens:
		t := v.Tokens
		ct.UncachedRequestTokens = &t
	case *attrconcurrency.InFlightLoad:
		t := v.Tokens
		ct.InFlightTokens = &t
	case *attrprefix.PrefixCacheMatchInfo:
		mb, tb := v.MatchBlocks(), v.TotalBlocks()
		ct.PrefixMatchBlocks = &mb
		ct.PrefixTotalBlocks = &tb
	}
}

// snapshotMetrics projects datalayer.Metrics into the JSON-friendly form.
func snapshotMetrics(m *fwkdl.Metrics) *MetricsSnapshot {
	if m == nil {
		return nil
	}
	return &MetricsSnapshot{
		ActiveModels:            m.ActiveModels,
		WaitingModels:           m.WaitingModels,
		MaxActiveModels:         m.MaxActiveModels,
		RunningRequestsSize:     m.RunningRequestsSize,
		WaitingQueueSize:        m.WaitingQueueSize,
		KVCacheUsagePercent:     m.KVCacheUsagePercent,
		KvCacheMaxTokenCapacity: m.KvCacheMaxTokenCapacity,
		CacheBlockSize:          m.CacheBlockSize,
		CacheNumBlocks:          m.CacheNumBlocks,
		UpdateTime:              m.UpdateTime,
	}
}

// dumpRequestAttributes reads the request's own attribute store. This is a
// DIFFERENT path from the endpoint store: GetAttribute returns `any` (not
// Cloneable) holding plain JSON-friendly values (time.Time, string), so a
// straight json.Marshal is correct here -- do not route it through the endpoint
// extractor registry.
func dumpRequestAttributes(req *fwksched.InferenceRequest) map[string]json.RawMessage {
	keys := req.AttributeKeys()
	if len(keys) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		v, ok := req.GetAttribute(k)
		if !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			// Request attributes are plain `any` (time.Time, string, ...), not
			// accessor-only Cloneables, so a marshal error is the only real loss
			// case here -- an empty "{}" is a legitimate empty value, not the
			// silent-loss hazard the endpoint path guards against.
			out[k] = unmarshalableMarker(v)
			continue
		}
		out[k] = b
	}
	return out
}

const sourceRead = "read"
