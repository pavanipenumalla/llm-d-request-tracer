# Trace data dictionary

Every field in a `RequestTrace` (one JSONL line per request), what it means, and
exactly where the tracer gets it. The tracer is a pure observer: it reads the
llm-d-router (EPP) request-control hooks and never alters routing.

## How the tracer sees a request

The EPP director runs a fixed sequence per request. The tracer registers on four
hooks (marked below). It correlates all hooks by `requestID` into one in-progress
record, then emits on end-of-stream (or via the staleness sweeper on abort).

```
director.HandleRequest:
  build InferenceRequest
  RequestHeader hooks        <-- tracer.RequestHeader   (stamps arrivalTime as
                                 a request attribute)
  derive FairnessID
  admissionController.Admit  <-- BLOCKS here: flow-control queue wait happens inside
  Screeners
  DataProducer hooks         <-- prefix-cache / in-flight / token producers write
                                 the per-endpoint attributes the tracer later dumps
  Admitter hooks
  Scheduler (filter/score/pick)
  prepareRequest:
    PreRequest hooks         <-- tracer.PreRequest      (stamps dispatchTime, reads
                                 arrival attr + the scheduling result + attributes)
  dispatch to model server
  ResponseHeader hooks       <-- tracer.ResponseHeader  (stamps headersTime)
  ResponseBody hooks         <-- tracer.ResponseBody    (firstToken on StartOfStream;
                                 finalize + emit on EndOfStream)
```

There is no hook that fires strictly between `Admit` and the scheduler, so the
admission controller cannot be bracketed by two tracer timestamps on its own. The
whole span (header processing + Admit queue wait + scheduling) is `totalEppMs`.

## Top-level fields

| Field | Type | Source (hook / call) | Meaning |
|---|---|---|---|
| `requestID` | string | `RequestHeader`; `InferenceRequest.RequestID` | Envoy-generated request id; the correlation key across hooks. |
| `fairnessID` | string | `PreRequest`; `InferenceRequest.FairnessID` | Session/flow grouping key. Resolved by the director before scheduling: explicit `x-llm-d-inference-fairness-id` header, else the agent-identity attribute, else `default-flow`. Read at PreRequest because it is unresolved at RequestHeader. |
| `targetModel` | string | `RequestHeader`; `InferenceRequest.TargetModel` | Final model after any traffic-split rewrite. |
| `arrivalTime` | timestamp | `RequestHeader`; stamped as request attribute `request-tracer/arrival-time`, read back at `PreRequest` | When the tracer first saw the request. Written onto the request's own attribute store so it is the single source of truth (also appears in `reqAttributes`). The earliest observable point. |
| `dispatchTime` | timestamp | `PreRequest`; tracer clock | When scheduling finished and the request was about to be dispatched. |
| `headersTime` | timestamp | `ResponseHeader`; tracer clock | When response headers arrived (response start). NOT first token. |
| `firstTokenTime` | timestamp | `ResponseBody` (first `StartOfStream` chunk); tracer clock | First streamed body chunk. The true TTFT anchor. |
| `completionTime` | timestamp | `ResponseBody` (`EndOfStream`); tracer clock | Last chunk / response complete. |
| `totalEppMs` | float ms | derived: `dispatchTime - arrivalTime` | Total time in the EPP before dispatch (header processing + admission/queue wait + scheduling). ALWAYS set once dispatched; the tracer owns both endpoints, so it never depends on another plugin. |
| `incomplete` | bool | staleness sweeper | `true` when the trace was emitted by the sweeper because the request never reached a clean `EndOfStream` (e.g. aborted/disconnected). |
| `winnerPod` | string | `PreRequest`; `SchedulingResult.TargetEndpoints[0]` | The selected endpoint (`namespace/name`). For disaggregated P/D there may be several targets; `winnerPod` names the first, and each candidate's `isWinner` flag carries the full set. |
| `servedPod` | string | `ResponseHeader`; `targetEndpoint.ID` | The pod that actually served the response. Normally equals `winnerPod`; captured to confirm the scheduled winner is who replied. |
| `responseStatus` | string | `ResponseHeader`; `Response.Headers[":status"]` | HTTP status of the response (e.g. `200`, `503`). Empty if the status header was not surfaced. |
| `candidates[]` | list | `PreRequest`; `SchedulingResult.ProfileResults[primary]` | One entry per scored candidate (see below). |
| `usage` | object | `ResponseBody` (`EndOfStream`); `Response.Usage` | Token counts parsed from the response: `promptTokens`, `completionTokens`, `totalTokens`. |
| `reqAttributes` | map | `PreRequest`; `InferenceRequest.AttributeKeys()`+`GetAttribute()` | Every entry in the request's own attribute store, keyed by string. Holds the tracer's `request-tracer/arrival-time` plus whatever other plugins published (e.g. `agent-identity`). Values are plain JSON. |

### Deriving other latencies offline

All raw timestamps are in the trace, so anything can be computed after the fact
without changing the plugin. Examples:

- inference latency: `completionTime - dispatchTime`
- time to first token (from dispatch): `firstTokenTime - dispatchTime`
- response header latency: `headersTime - dispatchTime`
- streaming duration: `completionTime - firstTokenTime`
- end-to-end (arrival to completion): `completionTime - arrivalTime`

## `candidates[]` entry

Each candidate the scheduler scored, read at `PreRequest` from
`SchedulingResult.ProfileResults[primaryProfile]`.

| Field | Type | Source | Meaning |
|---|---|---|---|
| `pod` | string | `ScoredCandidate.Endpoint` metadata ID | Candidate endpoint (`namespace/name`, else address). |
| `finalScore` | float | `ScoredCandidate.Score` | The FINAL WEIGHTED score for this endpoint (sum over scorers x weights). NOT a per-scorer breakdown -- it is the outcome, not which scorer drove it. |
| `isWinner` | bool | membership in `TargetEndpoints` | Whether this candidate was selected. |
| `metrics` | object | `Endpoint.GetMetrics()` | Full scraped metrics snapshot at scheduling time: `kvCacheUsagePercent`, `waitingQueueSize`, `runningRequestsSize`, active/waiting models, cache block info, `updateTime`. |
| `attributes` | map | `Endpoint.Keys()`+`Get()` | EVERY datalayer attribute present on the endpoint, keyed by `dataType/producerName`. Captures signals from any producer the deployment runs -- known or not. See serialization note. |
| `attrSource` | map | tracer | Per attribute key: `read` (verbatim from the producer) or `derived` (computed by the tracer). This build emits only `read`. |
| `uncachedRequestTokens` | int | projection of `UncachedRequestTokens` attr | Convenience field: uncached input tokens this request would add to this endpoint (written by the inflight-load producer). Nil if that producer is absent. |
| `prefixMatchBlocks` | int | projection of `PrefixCacheMatchInfo` attr | Convenience field: matched prefix-cache blocks on this endpoint. Nil if no prefix producer. |
| `prefixTotalBlocks` | int | projection of `PrefixCacheMatchInfo` attr | Convenience field: total prompt blocks. |
| `inFlightTokens` | int | projection of `InFlightLoad` attr | Convenience field: in-flight token load on this endpoint (read LIVE at PreRequest). Nil if no in-flight producer. |

### Attribute serialization (why the convenience fields exist)

The `attributes` map dumps whatever is present, but some attribute types
(notably `PrefixCacheMatchInfo`) have only unexported fields, so a naive
`json.Marshal` would emit `{}` and silently lose the data. The tracer uses a
per-type extractor that projects known types through their accessor methods, and
marks any unknown value that collapses to `{}` as `"unmarshalable: <type>"` so
the loss is visible rather than silent. The typed convenience fields
(`prefixMatchBlocks`, etc.) are the analysis-critical signals surfaced directly,
matched by concrete Go type (robust to producer-name differences).

## Provenance contract

`attrSource` distinguishes producer-read values from tracer-derived ones. This
build never derives, so every present attribute is `read`. The field exists so
that if reconstruction is ever added, a derived value can never masquerade as a
real producer value -- it will be tagged `derived` in the data.
