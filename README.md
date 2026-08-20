# llm-d-request-tracer

An out-of-tree [llm-d-router](https://github.com/llm-d/llm-d-router) (EPP) plugin
that records a **per-request trace** of the routing decision and emits it as
JSONL. For each request it captures where it was placed, the final weighted score
and datalayer signals for every candidate endpoint, the flow-control wait time
(when a fairness plugin is present), and the response timings.

It observes only; it never changes routing.

## What a trace contains

One JSON object per request (see `requesttracer/trace.go` for the exact schema):

- `requestID`, `fairnessID` (session grouping key), `targetModel`
- timings: `arrivalTime`, `dispatchTime`, `headersTime`, `firstTokenTime`,
  `completionTime`; derived `totalEppMs` (dispatch-arrival, always set). All raw
  timestamps are recorded, so any other latency is computable offline -- see
  `docs/data-dictionary.md`.
- `winnerPod` and a `candidates[]` list; each candidate carries its
  `finalScore`, a full `metrics` snapshot (KV%, queue depth, running/waiting),
  and an `attributes` bag holding **every** datalayer attribute present on the
  endpoint (prefix-cache match, in-flight load, uncached tokens, and anything
  else a producer published), keyed by `dataType/producerName`
- `reqAttributes`: the request's own attribute store
- `usage`: token counts from the response
- `incomplete: true` when the request never completed cleanly (emitted by the
  staleness sweeper)

### Provenance (`attrSource`)

Every captured attribute is tagged `read` (taken verbatim from the producer) or
`derived` (computed by the tracer). This build only ever emits `read`; the field
exists so that any future reconstructed value is visibly distinct from a real
producer value.

## Why a custom binary is needed

llm-d-router (EPP) has no plugin directory it loads at runtime. Every plugin must
be **compiled into the EPP binary**. So using this tracer means building your own
EPP binary that (a) imports this module and (b) enables it in the config. That is
all -- no fork of llm-d-router, no patch to its source. Two ways to get the
binary, below. Pick ONE.

## Option A -- fastest: build the ready-made binary in this repo

This repo ships a complete EPP entrypoint at `cmd/epp-with-tracer/` that is the
standard EPP `main` plus this plugin. Clone and build it:

```bash
git clone https://github.com/D-Sai-Venkatesh/llm-d-request-tracer.git
cd llm-d-request-tracer

# Build the EPP binary (outputs ./epp-with-tracer in the repo root).
go build -o epp-with-tracer ./cmd/epp-with-tracer
```

`./epp-with-tracer` is a drop-in replacement for the stock `epp` binary. Run it
exactly as you run stock EPP, pointing it at a config that enables the tracer
(see "Configure" below). Example:

```bash
./epp-with-tracer --config-file /path/to/epp-config.yaml   # plus your usual EPP flags
```

By default it depends on upstream `llm-d-router` `v0.10.0` (pinned in this repo's
`go.mod`). If you run a different router version or a fork, see "Match it to your
router" before building.

## Option B -- add the tracer to your own EPP `main`

If you already build your own EPP binary, add the plugin to it with one dependency
and one import. Nothing else changes.

1. In YOUR EPP module, add the dependency:

   ```bash
   go get github.com/D-Sai-Venkatesh/llm-d-request-tracer@latest
   ```

2. In your `main.go`, add a blank import next to where you call the runner. The
   underscore import runs the plugin's `init()`, which registers it. A complete
   minimal main looks exactly like this:

   ```go
   package main

   import (
       "os"

       ctrl "sigs.k8s.io/controller-runtime"

       "github.com/llm-d/llm-d-router/cmd/epp/runner"

       // This blank import is the only tracer-specific line. Its init()
       // registers "request-tracer" before the runner reads the config.
       _ "github.com/D-Sai-Venkatesh/llm-d-request-tracer/requesttracer"
   )

   func main() {
       ctx := ctrl.SetupSignalHandler()
       if err := runner.NewRunner().Run(ctx); err != nil {
           os.Exit(1)
       }
   }
   ```

3. Build your binary as usual (`go build`).

The plugin is registered at `Beta` stability, so you do **not** need the
`--allow-experimental-plugins` flag to enable it.

## Build a container image (for running in a cluster)

To run in Kubernetes you need a container image. This repo's `Dockerfile` mirrors
upstream's `Dockerfile.epp` and builds `cmd/epp-with-tracer` into a distroless
image that is a drop-in replacement for the stock EPP image (same ports, same
`/app/epp` entrypoint, nonroot). The `Makefile` wraps it:

```bash
# Build. Point it at your registry/name/tag. Set TARGETARCH to your CLUSTER's
# arch (amd64 for most GPU clusters; defaults to your host arch otherwise).
make image-build \
  IMAGE_REGISTRY=quay.io/you \
  IMAGE_NAME=llm-d-router \
  IMAGE_TAG=tracer \
  TARGETARCH=amd64

# Push (docker login first).
make image-push \
  IMAGE_REGISTRY=quay.io/you IMAGE_NAME=llm-d-router IMAGE_TAG=tracer
```

That produces `quay.io/you/llm-d-router:tracer`. In your EPP Deployment, swap the
EPP container image for this one and keep everything else (args, ports, mounts)
the same -- then enable the plugin in the config as below.

To build the image against a fork or local router checkout, add a `replace` to
`go.mod` first (see "Match it to your router version or fork").

## Configure -- enable the tracer in your EPP config

Whichever binary you built, the tracer does nothing until you list it in the
`EndpointPickerConfig` YAML that EPP loads (the file you pass via EPP's
`--config-file`, or the config your deployment mounts). Add this top-level plugin
entry -- no `schedulingProfile` reference is needed, the tracer runs for every
request:

```yaml
apiVersion: llm-d.ai/v1alpha1
kind: EndpointPickerConfig
plugins:
  - type: request-tracer
    parameters:
      sink: file                          # "file" (default) or "nop" to disable output
      filePath: /var/log/epp/traces.jsonl # ABSOLUTE path; must be writable by the EPP process
      bufferSize: 4096                    # in-memory queue depth; traces drop (and are counted) when full
      staleTraceTtlSeconds: 300           # emit an incomplete trace if a request never finishes within this
      sweepSeconds: 60                    # how often to check for stale requests
  # ... your existing routing plugins (filters/scorers/producers) stay as they are ...
```

A full example config with typical routing plugins alongside the tracer is at
[`examples/epp-config-with-tracer.yaml`](examples/epp-config-with-tracer.yaml).

**About `filePath`:** it is a path on the machine/pod where EPP runs, not where you
run `go build`. In Kubernetes, mount a volume and point `filePath` inside it (e.g.
`/var/log/epp/traces.jsonl` on an `emptyDir` or PVC) so you can retrieve the file
after a run. The directory must already exist and be writable.

## Read the output

`traces.jsonl` is one JSON object per request, one per line (JSON Lines). It is
flushed at least once per second, so you can `tail -f` it live during a run. Copy
it off the pod when the run finishes:

```bash
kubectl cp <epp-namespace>/<epp-pod>:/var/log/epp/traces.jsonl ./traces.jsonl
```

Every field and where it comes from is documented in
[`docs/data-dictionary.md`](docs/data-dictionary.md).

## Visualize the output

[`visualizer.html`](visualizer.html) is a self-contained, dependency-free page
for exploring a `traces.jsonl`. Open it in any browser and drag the file onto the
page (or click "Load traces"); everything is parsed and rendered locally, nothing
leaves the browser. It works from `file://` -- no server needed.

Three tabs plus a click-through decision panel:

- **Overview** -- pool-wide tiles (requests, sessions, pods, avg/p95 TTFT and
  end-to-end, tokens), requests-per-pod, a score-vs-placement summary (how often
  the chosen winner was the top-scored candidate), and a session table.
- **Session** -- one `fairnessID`: per-turn latency and token charts, a per-pod
  distribution (offered-as-candidate vs actually chosen), and a **Gantt
  timeline** of every turn over wall-clock (wait/TTFT vs decode, colored by pod,
  zoomable).
- **Requests** -- a flat, sortable table of every request across all sessions,
  for finding outliers by TTFT, end-to-end, or score gap.

Clicking any turn (table row or Gantt bar) opens the **decision panel**: the
timing breakdown plus every candidate pod ranked by `finalScore` with its
gap-to-winner, KV/queue/in-flight/prefix state at decision time, and a per-
candidate expander that dumps the full raw attribute bag with each value's
`read`/`derived` provenance badge.

## Match it to your router version or fork

This module pins upstream `github.com/llm-d/llm-d-router v0.10.0` in its `go.mod`.
It uses pre-1.0 router interfaces (`Endpoint`, `AttributeMap`, the request-control
hooks, the datalayer attribute packages) that can change between releases, so the
plugin and the router binary must agree on those interfaces. Three cases:

**1. You run stock upstream v0.10.0.** Nothing to do -- Option A builds against it
directly. For Option B, your EPP module already requires some router version; if
it is not v0.10.0, align them (`go get github.com/llm-d/llm-d-router@v0.10.0`) or
expect a build error that names any interface drift.

**2. You run your own router fork or an unreleased version.** Add a `replace` to
the `go.mod` of whichever module you `go build` (the repo root `go.mod` for Option
A, or your EPP module's `go.mod` for Option B). Go honors the top-level module's
`replace`, so both EPP and this plugin then compile against your fork:

```
require github.com/llm-d/llm-d-router v0.10.0
replace github.com/llm-d/llm-d-router => github.com/you/your-router-fork v0.0.0-<...>
// or a local checkout on disk:
replace github.com/llm-d/llm-d-router => /abs/path/to/your-router-checkout
```

Caveat: some forks change the attribute-store key type (this fork family has both
a `string`-keyed variant and a `DataKey`-keyed variant). Those are source-
incompatible; against a `DataKey`-keyed fork the plugin will not compile without
adjusting its attribute reads. The build error tells you which.

**3. You are hacking on this plugin against a local router checkout.** Same
mechanism -- add a `replace` in THIS repo's `go.mod` pointing at the checkout, and
do not commit it:

```
replace github.com/llm-d/llm-d-router => /abs/path/to/llm-d-router-checkout
```

## Notes and limitations

- **Scores are the final weighted score per candidate**, not a per-scorer
  breakdown. It tells you the outcome, not which scorer drove it. The raw
  per-candidate signals (prefix match, load, KV) are captured so you can
  reconstruct intuition offline.
- **`totalEppMs` is always available** (dispatch-arrival, both stamped by the
  tracer, arrival via a request attribute). Standalone admission-controller time
  is not separately observable -- no hook brackets it -- so `totalEppMs` covers
  the whole pre-dispatch span (header processing + queue wait + scheduling). See
  `docs/data-dictionary.md`.
- **Backpressure**: the file sink drops traces (and counts the drops) rather than
  block the request path when its buffer is full. Size `bufferSize` for your
  throughput.

## Metrics

The plugin registers Prometheus collectors under the `request_tracer_` subsystem:
`traces_emitted_total`, `traces_dropped_total`, `write_errors_total`,
`in_progress`, `traces_swept_total`.
