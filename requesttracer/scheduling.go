package requesttracer

import (
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

// candidatesFrom extracts the per-candidate trace entries and the winner pod
// from the primary profile's result. ScoredCandidates carries the final weighted
// score of every candidate; TargetEndpoints identifies the winner(s).
func candidatesFrom(result *fwksched.SchedulingResult) ([]CandidateTrace, string) {
	if result == nil {
		return nil, ""
	}
	primary, ok := result.ProfileResults[result.PrimaryProfileName]
	if !ok || primary == nil {
		return nil, ""
	}

	winners := make(map[string]struct{}, len(primary.TargetEndpoints))
	var winnerPod string
	for _, ep := range primary.TargetEndpoints {
		if ep == nil {
			continue
		}
		id := podID(ep)
		winners[id] = struct{}{}
		if winnerPod == "" {
			winnerPod = id
		}
	}

	out := make([]CandidateTrace, 0, len(primary.ScoredCandidates))
	for i := range primary.ScoredCandidates {
		sc := primary.ScoredCandidates[i]
		if sc.Endpoint == nil {
			continue
		}
		id := podID(sc.Endpoint)
		ct := CandidateTrace{
			Pod:        id,
			FinalScore: sc.Score,
			Metrics:    snapshotMetrics(sc.Endpoint.GetMetrics()),
		}
		if _, isWinner := winners[id]; isWinner {
			ct.IsWinner = true
		}
		dumpEndpointAttributes(sc.Endpoint, &ct)
		out = append(out, ct)
	}
	return out, winnerPod
}

// podID returns a stable identifier for an endpoint: its NamespacedName ID
// ("namespace/name"), falling back to the address.
func podID(ep fwksched.Endpoint) string {
	meta := ep.GetMetadata()
	if meta == nil {
		return ""
	}
	if id := meta.ID.String(); id != "/" && id != "" {
		return id
	}
	return meta.Address
}
