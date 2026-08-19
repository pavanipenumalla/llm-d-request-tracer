package requesttracer

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"default", DefaultConfig(), false},
		{"nop", Config{Sink: "nop", BufferSize: 1, StaleTraceTTLSeconds: 1, SweepSeconds: 1}, false},
		{"bad sink", Config{Sink: "kafka", BufferSize: 1, StaleTraceTTLSeconds: 1, SweepSeconds: 1}, true},
		{"file no path", Config{Sink: "file", BufferSize: 1, StaleTraceTTLSeconds: 1, SweepSeconds: 1}, true},
		{"zero buffer", Config{Sink: "nop", BufferSize: 0, StaleTraceTTLSeconds: 1, SweepSeconds: 1}, true},
		{"zero ttl", Config{Sink: "nop", BufferSize: 1, StaleTraceTTLSeconds: 0, SweepSeconds: 1}, true},
		{"zero sweep", Config{Sink: "nop", BufferSize: 1, StaleTraceTTLSeconds: 1, SweepSeconds: 0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestFactoryStrictDecode confirms parameters decode and an unknown field is
// rejected by the strict decoder the framework supplies.
func TestFactoryStrictDecode(t *testing.T) {
	good := `{"sink":"nop","bufferSize":16,"staleTraceTtlSeconds":10,"sweepSeconds":2}`
	dec := json.NewDecoder(bytes.NewReader([]byte(good)))
	p, err := Factory("t", dec, nil)
	if err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	if p.TypedName().Type != PluginType {
		t.Fatalf("wrong type: %s", p.TypedName().Type)
	}

	bad := `{"sink":"nop","unknownField":true}`
	strict := json.NewDecoder(bytes.NewReader([]byte(bad)))
	strict.DisallowUnknownFields()
	if _, err := Factory("t", strict, nil); err == nil {
		t.Fatalf("expected strict decode to reject unknown field")
	}
}
