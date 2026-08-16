// Command rescore re-grades saved tier-2 transcripts under the evaluators in
// the working tree.
//
// It needs no cluster, no lookout binary and no Vertex call: cmd/sre-eval-live
// serializes Run.Health, and evals.ScoreLive is a pure function of that report
// and the fixture. So the cost of a metric change is measurable against every
// run ever recorded, which is the only way to tell a scoring change that moved
// a number from one that did not.
//
// That distinction is the reason this exists. Three of the four corrections to
// failureFamilies/genericReasons were found by a live run charging an honest
// report as an invention, and each one cost a suite to find. Re-scoring costs
// nothing, so a change to the tables should be run past every transcript on
// disk before it is believed:
//
//	go run ./dev/rescore /tmp/eval-live-*.json
//
// Aggregates are means over the scored (non-skipped) cells, exactly as
// cmd/sre-eval-live computes them, so they are comparable to a published
// baseline. Below-ceiling cells are printed individually because the aggregate
// is what stays still while two fixtures move in opposite directions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/go-steer/k8s-sre-agent/internal/evals"
	"github.com/go-steer/k8s-sre-agent/internal/faults"
)

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: rescore <eval-live.json>...")
		os.Exit(2)
	}

	byName := map[string]faults.Fault{}
	for _, f := range faults.All() {
		byName[f.Name] = f
	}

	status := 0
	for _, path := range flag.Args() {
		if err := rescore(path, byName); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			status = 1
		}
	}
	os.Exit(status)
}

func rescore(path string, byName map[string]faults.Fault) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Only the two fields that matter are decoded. The rest of the record —
	// trajectory, usage, elapsed — is a property of the run that produced the
	// transcript and cannot be re-derived, so re-stating its shape here would
	// only be one more thing to keep in step with cmd/sre-eval-live.
	var doc struct {
		Results []struct {
			Fixture string    `json:"fixture"`
			Run     evals.Run `json:"run"`
		} `json:"results"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		return err
	}

	fmt.Printf("== %s (%d fixtures)\n", path, len(doc.Results))

	type agg struct {
		sum     float64
		scored  int
		skipped int
	}
	sums := map[string]*agg{}
	var order []string
	for _, r := range doc.Results {
		f, ok := byName[r.Fixture]
		if !ok {
			// A fixture that has been renamed or deleted since the transcript
			// was written. Reported rather than skipped silently: the run it
			// came from is no longer comparable to a current one, and an
			// aggregate quietly computed over nine of ten fixtures is worse
			// than no aggregate.
			fmt.Printf("   !! %-20s no such fixture in the current suite; not scored\n", r.Fixture)
			continue
		}
		for _, s := range evals.ScoreLive(f, r.Run) {
			a, seen := sums[s.Name]
			if !seen {
				a = &agg{}
				sums[s.Name] = a
				order = append(order, s.Name)
			}
			if s.Skipped {
				a.skipped++
				continue
			}
			a.sum += s.Value
			a.scored++
			if s.Value < 1 {
				fmt.Printf("   %-20s %-19s %.3f  %s\n", r.Fixture, s.Name, s.Value, s.Comment)
			}
		}
	}

	sort.Strings(order)
	for _, name := range order {
		a := sums[name]
		if a.scored == 0 {
			// "not measured" and "perfect" must not render alike, the same
			// rule UsageSummary's coverage line follows.
			fmt.Printf("   %-20s   —    (0 scored, %d skipped)\n", name, a.skipped)
			continue
		}
		line := fmt.Sprintf("   %-20s %.3f  (%d scored", name, a.sum/float64(a.scored), a.scored)
		if a.skipped > 0 {
			line += fmt.Sprintf(", %d skipped", a.skipped)
		}
		fmt.Println(line + ")")
	}
	return nil
}
