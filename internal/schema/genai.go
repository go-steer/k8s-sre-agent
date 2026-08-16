package schema

import "google.golang.org/genai"

// ReportSchema is HealthReport expressed as a genai.Schema, for use as a
// Task-mode agent's OutputSchema — the model then fills it via finish_task.
//
// Written by hand rather than reflected from the struct. The descriptions are
// the point: they are the only place the model learns that `reason` must stay
// stable across runs (monitor fingerprints depend on it) and that `ok` and
// `info` are not interchangeable. A reflected schema would carry the field
// names and lose all of that.
//
// Kept in sync with the struct by TestReportSchemaMatchesStruct.
func ReportSchema() *genai.Schema {
	finding := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"severity": {
				Type:        genai.TypeString,
				Enum:        []string{string(SeverityCritical), string(SeverityWarning), string(SeverityInfo)},
				Description: "How urgent this single finding is.",
			},
			"title": {
				Type:        genai.TypeString,
				Description: "One-line headline naming the object and the condition.",
			},
			"detail": {
				Type:        genai.TypeString,
				Description: "The evidence: what the checks showed and why it means what you say it means.",
			},
			"namespace": {
				Type:        genai.TypeString,
				Description: "Namespace of the affected object; empty for cluster-scoped findings.",
			},
			"kind": {
				Type:        genai.TypeString,
				Description: "Kubernetes kind of the affected object, e.g. Pod, Deployment, Node.",
			},
			"resource_name": {
				Type:        genai.TypeString,
				Description: "Name of the affected object, exactly as the cluster reports it.",
			},
			"reason": {
				Type: genai.TypeString,
				Description: "Terse, stable condition word — CrashLoopBackOff, OOMKilled, " +
					"ImagePullBackOff, Unschedulable. Reuse the same word for the same " +
					"condition on every run: this field identifies the finding across " +
					"monitoring cycles, and rewording it reports a new incident.",
			},
		},
		Required: []string{"severity", "title", "detail"},
	}

	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"overall_severity": {
				Type: genai.TypeString,
				Enum: []string{
					string(OverallOK), string(OverallInfo),
					string(OverallWarning), string(OverallCritical),
				},
				Description: "Highest severity among the findings; 'ok' only when there are none. " +
					"Findings that are purely informational make this 'info', not 'ok'.",
			},
			"summary": {
				Type: genai.TypeString,
				Description: "One or two sentences leading with the severity word and the object, " +
					"e.g. \"CRITICAL: api-server-7d8f9c-xkp2v is CrashLoopBackOff (18 restarts)\".",
			},
			"findings": {
				Type:        genai.TypeArray,
				Items:       finding,
				Description: "Every issue found, most severe first. Empty when the cluster is healthy.",
			},
			"recommended_actions": {
				Type:        genai.TypeArray,
				Items:       &genai.Schema{Type: genai.TypeString},
				Description: "Concrete ordered remediation steps naming the object each acts on.",
			},
		},
		Required: []string{"overall_severity", "summary", "findings"},
	}
}
