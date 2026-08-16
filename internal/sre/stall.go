package sre

import (
	mastagent "github.com/go-steer/mast/pkg/agent"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// stallReport is the mastagent.StallPayload every Task specialist is built
// with: the HealthReport a specialist that stopped without reporting did not
// write.
//
// # What mast owns and what this owns
//
// The guard itself is mastagent.FinishOnStall, because the hazard is a property
// of ADK's Task mode rather than of this repo — a Task sub-agent that drains its
// iterator without calling finish_task ends the *caller's* turn too, so the
// orchestrator never regains control and the run produces no report at all. The
// mechanism, the argument for intercepting at the response rather than wrapping
// the agent, and the reason a prompt cannot do this are all documented there;
// this repo carried its own copy until mast#128 landed.
//
// What did not move upstream is this function, and mast is explicit about why:
// a value that *conforms* to a schema is not the same as an *empty* one, and
// only the roster that wrote the schema knows the difference. mast can already
// synthesise a conforming value — conformingArgs in its schemafill.go does it
// for the offline fakes — and using it here would invent content. Both of the
// judgement calls below are exactly that difference.
//
// # Empty findings
//
// A finding is a cluster fault: internal/monitor fingerprints it and tracks it
// across cycles. "A subagent stopped talking" is not one, and a fabricated entry
// would enter the incident stream as though it were.
//
// # Which forces the severity
//
// schema.HealthReport.Validate rejects any severity but "ok" with no findings,
// and that rule is right — a severity is a severity *of* something. But "ok"
// read on its own is an affirmative claim of health nobody made, which is the
// whole reason the marker leads the summary: a severity field cannot carry "not
// checked", so only the text can. mastagent.StallText writes that text, and
// mastagent.Stalled is how internal/evals finds it again — a run the guard
// rescued reached the end with a hole in it and must not be filed next to a run
// that had nothing missing.
//
// The specialist's own last words come through StallText verbatim, after a
// blank line. They are usually the most informative thing in the delegation:
// the question it wanted to ask names the data it could not get, which is how
// the missing enumeration tool was found.
//
// The shape has to satisfy schema.ReportSchema(), which is the specialist's
// OutputSchema and therefore finish_task's parameter schema: ADK validates the
// injected call exactly as it validates a model-issued one and hands back a
// retryable error if it does not fit, which would leave the run in exactly the
// state the guard exists to prevent. TestTheStallReportSatisfiesTheContract
// checks it against the schema *and* against the report tool's own validator.
func stallReport(agentName, lastWords string) map[string]any {
	return map[string]any{
		"overall_severity": string(schema.OverallOK),
		"summary":          mastagent.StallText(agentName, lastWords),
		"findings":         []any{},
	}
}
