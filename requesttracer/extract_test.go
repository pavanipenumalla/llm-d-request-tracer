package requesttracer

import (
	"encoding/json"
	"testing"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrconcurrency "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/concurrency"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
)

// mkEndpoint builds a scheduling.Endpoint carrying the given attributes.
// Endpoint attributes are keyed by DataKey.
func mkEndpoint(name string, attrs map[fwkplugin.DataKey]fwkdl.Cloneable) fwksched.Endpoint {
	m := fwkdl.NewAttributes()
	for k, v := range attrs {
		m.Put(k, v)
	}
	meta := &fwkdl.EndpointMetadata{Name: name, Address: name}
	return fwksched.NewEndpoint(meta, fwkdl.NewMetrics(), m)
}

// TestPrefixCacheMatchInfoNotSilentlyEmpty is the C1 regression test: a bare
// json.Marshal of PrefixCacheMatchInfo yields "{}" (all fields unexported), so
// the extractor registry MUST project it through accessors instead.
func TestPrefixCacheMatchInfoNotSilentlyEmpty(t *testing.T) {
	info := attrprefix.NewPrefixCacheMatchInfo(3, 10, 256)

	// Prove the hazard exists: the naive path loses everything.
	if b, _ := json.Marshal(info); string(b) != "{}" {
		t.Fatalf("precondition changed: bare marshal of PrefixCacheMatchInfo now yields %s; C1 may no longer apply", b)
	}

	raw, ok := marshalAttr(info)
	if !ok {
		t.Fatalf("marshalAttr reported failure for a known type")
	}
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal extractor output: %v", err)
	}
	if got["matchBlocks"] != 3 || got["totalBlocks"] != 10 || got["blockSizeTokens"] != 256 {
		t.Fatalf("extractor lost data: %v", got)
	}
}

// TestExportedAttrsMarshalFine confirms the default path handles exported-field types.
func TestExportedAttrsMarshalFine(t *testing.T) {
	raw, ok := marshalAttr(&attrconcurrency.InFlightLoad{Tokens: 42, Requests: 2})
	if !ok {
		t.Fatalf("marshalAttr failed on InFlightLoad")
	}
	var got attrconcurrency.InFlightLoad
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tokens != 42 || got.Requests != 2 {
		t.Fatalf("wrong values: %+v", got)
	}
}

// unmarshalableAttr is a Cloneable whose exported-field marshal collapses to "{}".
type unmarshalableAttr struct{ secret int }

func (u *unmarshalableAttr) Clone() fwkdl.Cloneable { cp := *u; return &cp }

// TestUnknownEmptyMarkedNotDropped checks the silent-loss guard for unknown types.
func TestUnknownEmptyMarkedNotDropped(t *testing.T) {
	raw, ok := marshalAttr(&unmarshalableAttr{secret: 9})
	if ok {
		t.Fatalf("expected marshalAttr to flag the empty-object case")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("marker is not a JSON string: %s", raw)
	}
	if want := "unmarshalable: *requesttracer.unmarshalableAttr"; s != want {
		t.Fatalf("marker = %q, want %q", s, want)
	}
}

// TestDumpEndpointAttributesFull dumps a mix of keys, including a non-default
// producer name and an unknown key, and checks the bag + typed projections.
func TestDumpEndpointAttributesFull(t *testing.T) {
	// Producers write endpoint attributes under DataKey.String(). Mirror that,
	// including a non-default producer name.
	prefixKey := attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName("custom-prefix")
	inflightKey := attrconcurrency.InFlightLoadDataKey
	uncachedKey := attrconcurrency.UncachedRequestTokensDataKey
	unknownKey := fwkplugin.NewDataKey("SomeFutureSignal", "future-producer")

	ep := mkEndpoint("pod-a", map[fwkplugin.DataKey]fwkdl.Cloneable{
		prefixKey:   attrprefix.NewPrefixCacheMatchInfo(5, 20, 64),
		inflightKey: &attrconcurrency.InFlightLoad{Tokens: 100, Requests: 3},
		uncachedKey: &attrconcurrency.UncachedRequestTokens{Tokens: 7},
		unknownKey:  &unmarshalableAttr{secret: 1},
	})

	var ct CandidateTrace
	dumpEndpointAttributes(ep, &ct)

	// Every key present, under its string label including the custom producer name.
	if len(ct.Attributes) != 4 {
		t.Fatalf("expected 4 attributes, got %d: %v", len(ct.Attributes), ct.Attributes)
	}
	if _, ok := ct.Attributes[prefixKey.String()]; !ok {
		t.Fatalf("prefix key %q missing from bag", prefixKey.String())
	}
	if _, ok := ct.Attributes[unknownKey.String()]; !ok {
		t.Fatalf("unknown key missing from bag (must not be dropped)")
	}
	for k, src := range ct.AttrSource {
		if src != sourceRead {
			t.Fatalf("attr %s has source %q, want read", k, src)
		}
	}

	// Typed projections filled by concrete type despite the custom producer name.
	if ct.PrefixMatchBlocks == nil || *ct.PrefixMatchBlocks != 5 {
		t.Fatalf("PrefixMatchBlocks projection wrong: %v", ct.PrefixMatchBlocks)
	}
	if ct.PrefixTotalBlocks == nil || *ct.PrefixTotalBlocks != 20 {
		t.Fatalf("PrefixTotalBlocks projection wrong: %v", ct.PrefixTotalBlocks)
	}
	if ct.InFlightTokens == nil || *ct.InFlightTokens != 100 {
		t.Fatalf("InFlightTokens projection wrong: %v", ct.InFlightTokens)
	}
	if ct.UncachedRequestTokens == nil || *ct.UncachedRequestTokens != 7 {
		t.Fatalf("UncachedRequestTokens projection wrong: %v", ct.UncachedRequestTokens)
	}
}

// TestDumpEndpointAttributesEmpty: no attributes -> nil bag, no panic.
func TestDumpEndpointAttributesEmpty(t *testing.T) {
	ep := mkEndpoint("pod-b", nil)
	var ct CandidateTrace
	dumpEndpointAttributes(ep, &ct)
	if ct.Attributes != nil || ct.PrefixMatchBlocks != nil {
		t.Fatalf("expected empty projections, got %+v", ct)
	}
}
