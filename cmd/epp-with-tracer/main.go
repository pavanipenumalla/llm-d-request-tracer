// Command epp-with-tracer is a drop-in replacement for the standard llm-d EPP
// binary that additionally compiles in the request-tracer plugin. It is the same
// entrypoint as the upstream cmd/epp/main.go, plus a single blank import whose
// init() registers `request-tracer` in the plugin registry before the runner
// reads the config. Enable it by adding `- type: request-tracer` to your
// EndpointPickerConfig.
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-router/cmd/epp/runner"

	// Registers the request-tracer plugin via init().
	_ "github.com/D-Sai-Venkatesh/llm-d-request-tracer/requesttracer"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx := ctrl.SetupSignalHandler()
	if err := runner.NewRunner().Run(ctx); err != nil {
		return 1
	}
	return 0
}
