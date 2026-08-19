package requesttracer

import fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"

// init registers the plugin factory in the llm-d-router global plugin registry.
// Blank-importing this package from a consumer's main (before runner.Run) makes
// `- type: request-tracer` resolvable in the EndpointPickerConfig. Registered
// Beta, so no --allow-experimental-plugins flag is needed. (When/if this is
// upstreamed in-tree it should land Alpha, per the new-plugin convention.)
func init() {
	fwkplugin.Register(PluginType, fwkplugin.StabilityBeta, Factory)
}
