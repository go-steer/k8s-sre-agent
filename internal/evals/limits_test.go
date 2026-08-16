package evals

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/mast/pkg/budget"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// meteredModel answers every request with one tool call, then text, and reports
// usage on both — the shape a real turn has, and the only thing the meter reads.
type meteredModel struct {
	mu    sync.Mutex
	calls int
}

func (m *meteredModel) Name() string { return "claude-sonnet-5" }

func (m *meteredModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	n := m.calls
	m.calls++
	m.mu.Unlock()

	parts := []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "peek", Args: map[string]any{}}}}
	if n >= 3 {
		parts = []*genai.Part{{Text: "done"}}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
			ModelVersion: "claude-sonnet-5",
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     1000,
				CandidatesTokenCount: 100,
				TotalTokenCount:      1100,
			},
		}, nil)
	}
}

func (m *meteredModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// meteredRunner builds the smallest agent that will loop: a Chat agent whose
// model keeps calling one trivial tool. The loop is the point — a ceiling is
// for an agent that will not stop on its own.
func meteredRunner(t *testing.T, limits budget.Limits) (*Runner, *meteredModel) {
	t.Helper()
	type noArgs struct{}
	peek, err := functiontool.New(
		functiontool.Config{Name: "peek", Description: "looks at nothing"},
		func(adkagent.Context, noArgs) (string, error) { return "nothing here", nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	m := &meteredModel{}
	ag, err := llmagent.New(llmagent.Config{
		Name:        "sre-orchestrator",
		Description: "spends money",
		Instruction: "Keep looking.",
		Model:       m,
		Tools:       []tool.Tool{peek},
		Mode:        llmagent.ModeChat,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	return &Runner{Agent: ag, Limits: limits}, m
}

// A ceiling that fires has to hand back what the run had already produced. The
// alternative — Run{} plus an error — is the failure mode snapshot() exists to
// prevent, and it is worse here than on a crash: the operator chose this stop,
// so the partial result is the thing they asked for rather than wreckage.
func TestATightBudgetAbortsAndKeepsThePartialRun(t *testing.T) {
	ex := Example{}
	ex.Inputs.Scenario = "Assess the health of the \"shop\" namespace."

	r, m := meteredRunner(t, budget.Limits{MaxTurns: 2})
	got, err := r.Run(context.Background(), "budget-01", ex)
	if err == nil {
		t.Fatalf("run completed under a 2-turn ceiling after %d model calls", m.count())
	}
	if !errors.Is(err, budget.ErrExceeded) {
		t.Fatalf("Run error = %v, want budget.ErrExceeded", err)
	}
	// The commands classify a stop off the recorded string, not the error
	// value: the transcript keeps errors as text.
	if !ExceededBudget(err.Error()) {
		t.Errorf("ExceededBudget(%q) = false — the commands would file this as a failure", err)
	}
	if !strings.Contains(err.Error(), "budget-01") {
		t.Errorf("error does not name the run: %v", err)
	}

	// Enforcement is after the call, so the third request is what trips a
	// two-turn ceiling — and its tokens are billed. Both are in the snapshot.
	if got.Usage.Empty() {
		t.Fatal("the aborted run reports no usage at all")
	}
	if reqs := got.Usage.Total().Requests; reqs != 3 {
		t.Errorf("recorded %d requests, want 3 (two under the cap, one that crossed it)", reqs)
	}
	if m.count() != 3 {
		t.Errorf("model was called %d times, want 3 — the run did not stop when the meter said so", m.count())
	}
}

// And the ceiling is what stopped it. Without one the identical script runs to
// completion, so the test above is not measuring a scripted model that ran out.
func TestTheSameRunCompletesWithNoCeiling(t *testing.T) {
	ex := Example{}
	ex.Inputs.Scenario = "Assess the health of the \"shop\" namespace."

	r, m := meteredRunner(t, budget.Limits{})
	got, err := r.Run(context.Background(), "budget-02", ex)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Response != "done" {
		t.Errorf("Response = %q, want the script's last turn", got.Response)
	}
	if m.count() != 4 {
		t.Errorf("model was called %d times, want 4", m.count())
	}
}

// The fallback rate is the one that matters for a ceiling: a model the catalog
// cannot price must not be free, or -max-cost would silently stop metering the
// moment someone points this at a model nobody added to the table.
func TestAnUnpricedModelStillCountsAgainstACostCeiling(t *testing.T) {
	lim := Limits(testCatalog(t), 5, 0)
	if lim.RatePer1K <= 0 {
		t.Fatalf("RatePer1K = %v, want a positive fallback", lim.RatePer1K)
	}
	if lim.Catalog == nil {
		t.Error("Limits dropped the catalog, so every call would price at the flat rate")
	}
	if lim.MaxCostUSD != 5 {
		t.Errorf("MaxCostUSD = %v, want 5", lim.MaxCostUSD)
	}

	// The zero value has to stay unlimited: it is what every command ships,
	// and a default ceiling would truncate baselines nobody asked to bound.
	if zero := Limits(testCatalog(t), 0, 0); zero.MaxCostUSD != 0 || zero.MaxTurns != 0 {
		t.Errorf("Limits(cat, 0, 0) = %+v, want unlimited", zero)
	}
}
