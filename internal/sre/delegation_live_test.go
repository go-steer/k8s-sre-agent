package sre

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/llm"
	"github.com/go-steer/k8s-sre-agent/internal/lookout"
)

// Does delegation actually execute?
//
// The eval harness counts a delegation when it sees a FunctionCall named after
// a specialist. That counts the *attempt*. It says nothing about whether the
// specialist ran, because a failed agenttool call is still a function call —
// the failure arrives later, as a FunctionResponse carrying an error, and the
// orchestrator is free to shrug and do the work itself with its own tools.
//
// So the delegation numbers in both eval tiers are only meaningful if the
// specialist responses are not errors. This test looks at the responses.
func TestDelegationActuallyExecutes(t *testing.T) {
	if os.Getenv(envLiveModel) == "" {
		t.Skipf("set %s=1 to run", envLiveModel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	main, sub, err := llm.Models(ctx)
	if err != nil {
		t.Fatalf("resolve models: %v", err)
	}
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}
	root, err := Build(Config{Main: main, Subagent: sub, Toolsets: []tool.Toolset{offline}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	roster, err := SpecialistNames("")
	if err != nil {
		t.Fatal(err)
	}
	isSpecialist := map[string]bool{}
	for _, n := range roster {
		isSpecialist[n] = true
	}

	rn, err := runner.NewInMemory("sre-delegation-check", root)
	if err != nil {
		t.Fatal(err)
	}

	// Shaped like a tier-1 scenario: the fault is fully stated, so the offline
	// toolset's "the situation is described in the user's message, continue
	// from the description" response is enough for a specialist to finish.
	// A vaguer prompt makes the specialist stop and ask for a live cluster,
	// which is a real behaviour but not the one under test here.
	const prompt = "Pod 'api-server-7d8f9c-xkp2v' in namespace 'production' is in " +
		"CrashLoopBackOff. Restart count: 18. Container exits with code 1 within " +
		"seconds of starting. Delegate the pod-level investigation to your " +
		"pod-inspector specialist, then report what it found."

	var events []*session.Event
	for ev, err := range rn.Run(ctx, "check", "delegation",
		genai.NewContentFromText(prompt, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		events = append(events, ev)
	}

	var attempted, failed, answered int
	for _, ev := range events {
		if ev == nil || ev.Partial || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && isSpecialist[p.FunctionCall.Name] {
				attempted++
			}
			fr := p.FunctionResponse
			if fr == nil || !isSpecialist[fr.Name] {
				continue
			}
			if errText := responseError(fr.Response); errText != "" {
				failed++
				t.Logf("delegation to %s FAILED: %s", fr.Name, errText)
				continue
			}
			if len(fr.Response) > 0 {
				answered++
			}
		}
	}

	t.Logf("%d delegation call(s), %d failed, %d answered", attempted, failed, answered)
	if failed > 0 || answered == 0 {
		t.Logf("event stream:\n%s", summarize(events))
	}
	if attempted == 0 {
		t.Skip("the model did not delegate; this run proves nothing either way")
	}
	if failed > 0 {
		t.Fatalf("%d of %d delegations returned an error — the specialists are not "+
			"executing, and every delegation count reported by the eval harness is "+
			"counting attempts that failed", failed, attempted)
	}
	// An error-free response is not the same as a response. agenttool's failure
	// mode was an error, but a delegation that resolves to an empty payload
	// would score identically to a real one and is just as inert.
	if answered == 0 {
		t.Fatalf("%d delegation(s), none of which came back with a payload", attempted)
	}
}

// responseError extracts an error message from a function response payload,
// or "" when the response is a normal result.
func responseError(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	for _, key := range []string{"error", "Error"} {
		if v, ok := resp[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}
