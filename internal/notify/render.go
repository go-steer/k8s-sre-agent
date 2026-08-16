// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-steer/k8s-sre-agent/internal/scheduler"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// Render turns a digest into the message an operator reads.
//
// # What it leads with
//
// The header is the cluster, the trigger and the severity, because those are
// the three things that decide whether the rest is worth reading at 3am. Then
// the escalations, because a full-agent assessment is the only part of a cycle
// that contains a diagnosis. Transitions come after, and `ongoing` ones are
// omitted entirely: the whole reason there is a differ in front of the agent is
// that a steady state should be quiet, and re-listing every open finding every
// cycle is the digest nobody keeps reading.
//
// # Why an escalation prints prose, and exactly one layer of it
//
// The first version printed only each finding's identity triple — severity,
// `Kind/name`, reason — which is the machine-stable half of the contract and
// none of the answer. Reading it in Slack, the complaint was immediate and
// correct: the digest said `critical Deployment/recommendationservice
// ImagePullBackOff` and nowhere said *why* the image would not pull or what to
// do about it, on a message produced by a $0.25 full-agent assessment whose
// entire value is the diagnosis. Kind/name/reason is what internal/monitor
// fingerprints on, not what a human reads.
//
// The version that fixed it printed all four prose fields — Summary, then each
// finding's Title *and* Detail, then RecommendedActions — and the next reading
// in Slack found the opposite failure. A `HealthReport` restates itself by
// design: the model writes a Summary that summarises the findings, a Title that
// names each one, a Detail that argues it at length, and an action per finding
// that inverts it. Printing all four gives the same content four times, and a
// four-finding assessment of a *healthy* namespace ran to forty lines of
// which about six were new information.
//
// So exactly one prose layer survives per escalation: the report's Summary,
// which is the only field written about the namespace as a whole and therefore
// the only one that carries the causal frame — what broke, what it did to
// traffic, what is still working. Findings are one line each (the triple plus a
// Title that adds to it), because their job here is to index the summary
// against objects a differ can track. RecommendedActions stay, because they are
// the one thing the summary does not contain.
//
// Detail is dropped, and the fallback is the argument for the rule: when a
// report has no Summary, the leading finding's Detail is printed in its place.
// The digest needs a paragraph of prose; it does not need three of them saying
// the same thing.
//
// Length is not a transport concern — switchboard chunks a long message into
// ordered in-thread posts, so nothing is truncated on the way out. The clips
// and caps below are about what a person will actually read at 3am.
//
// # What it refuses to leave out
//
// Four things are printed even though they make the message longer, and each
// one is the harness's no-silent-caps rule applied to a different way a cycle
// can be less than it looks:
//
//   - escalations the per-cycle cap refused, because a cycle that assessed two
//     of five namespaces and said nothing about the other three reads exactly
//     like a cycle where two things changed;
//   - cluster-scoped transitions, which are real findings that no
//     namespace-scoped agent can be sent to investigate, so they are nobody's
//     problem unless they are printed;
//   - collection failures, because a digest built from half a scan must say
//     which half;
//   - contract violations, because the bounded pass has no handback and a
//     report accepted under protest must not read like a clean one.
func Render(d scheduler.Digest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "*%s* — %s — %s (%s)\n", d.Cluster, strings.ToUpper(string(headline(d))), triggerWord(d),
		d.At.UTC().Format(time.RFC3339))

	if s := scanProse(d); s != "" {
		fmt.Fprintf(&b, "%s\n", s)
	}

	if len(d.Escalations) > 0 {
		b.WriteString("\n*Assessed*\n")
		for _, e := range d.Escalations {
			switch {
			case e.Err != nil:
				fmt.Fprintf(&b, "• `%s` — assessment failed after %s: %s\n",
					e.Namespace, e.Elapsed.Round(time.Second), firstLine(e.Err.Error()))
			case e.Report != nil:
				fmt.Fprintf(&b, "• `%s` — %s, %d finding(s) in %s\n", e.Namespace,
					e.Report.OverallSeverity, len(e.Report.Findings), e.Elapsed.Round(time.Second))
				if s := prose(e.Report); s != "" {
					fmt.Fprintf(&b, "    _%s_\n", clip(s, summaryLimit))
				}
				for i, f := range e.Report.Findings {
					if i == maxFindings && len(e.Report.Findings) > maxFindings+1 {
						fmt.Fprintf(&b, "    ‣ …and %d more finding(s)\n", len(e.Report.Findings)-i)
						break
					}
					fmt.Fprintf(&b, "    ‣ %s `%s` %s", f.Severity, object(f), f.Reason)
					if t := findingTitle(f); t != "" {
						fmt.Fprintf(&b, " — %s", clip(t, titleLimit))
					}
					b.WriteString("\n")
				}
				for i, a := range e.Report.RecommendedActions {
					if i == maxActions && len(e.Report.RecommendedActions) > maxActions+1 {
						fmt.Fprintf(&b, "    → …and %d more recommended action(s)\n",
							len(e.Report.RecommendedActions)-i)
						break
					}
					if a := oneLine(a); a != "" {
						fmt.Fprintf(&b, "    → %s\n", clip(a, actionLimit))
					}
				}
			default:
				fmt.Fprintf(&b, "• `%s` — no report\n", e.Namespace)
			}
		}
	}

	// Only what changed. An `ongoing` subject is the steady state and printing
	// it turns the digest back into the every-cycle finding list the diff
	// exists to replace.
	if changed := changedTransitions(d.Transitions); len(changed) > 0 {
		b.WriteString("\n*Changed*\n")
		for _, t := range changed {
			fmt.Fprintf(&b, "• %s `%s` %s", t.Class, t.Target(), t.Reason)
			if t.PrevSeverity != "" && t.PrevSeverity != t.Severity {
				fmt.Fprintf(&b, " (%s → %s)", t.PrevSeverity, t.Severity)
			} else {
				fmt.Fprintf(&b, " (%s)", t.Severity)
			}
			if age := openFor(t, d.At); age != "" {
				fmt.Fprintf(&b, " — open %s", age)
			}
			b.WriteString("\n")
		}
	}

	writeCaveat(&b, "Not assessed — per-cycle escalation cap", targets(d.Dropped))
	writeCaveat(&b, "Cluster-scoped — no namespace to assess", targets(d.Unscoped))
	writeCaveat(&b, "Scan incomplete", d.CollectErrors)
	writeCaveat(&b, "Findings not tracked", violations(d.Protests))

	return strings.TrimRight(b.String(), "\n")
}

// headline is the worst severity anywhere in the digest, not just the bounded
// pass's.
//
// Found by running the demo: a cycle came back with the bounded pass reporting
// `ok` and an escalation reporting `warning`, and the header said OK. The whole
// job of that line is to tell somebody scanning a channel whether to read the
// rest, and a header that contradicts its own body is worse than no header —
// the bounded pass is a ten-category scan and the escalation is a full agent
// assessment, so when they disagree the agent is the one that looked harder.
func headline(d scheduler.Digest) schema.OverallSeverity {
	worst := schema.OverallOK
	if d.Report != nil && d.Report.OverallSeverity.AtLeast(worst) {
		worst = d.Report.OverallSeverity
	}
	for _, e := range d.Escalations {
		if e.Report == nil {
			continue
		}
		if e.Report.OverallSeverity.AtLeast(worst) {
			worst = e.Report.OverallSeverity
		}
	}
	return worst
}

// triggerWord is the scheduler's trigger, unless the cycle is a recovery.
//
// A cycle whose only changes are resolutions has nothing for anybody to do, and
// saying so in the header is the difference between a message that gets read
// and one that gets skipped. Found by reading one: the fault-cleared cycle went
// out headed `INFO — changes`, where the INFO came from an unrelated
// control-plane advisory in the bounded scan and the whole body was two
// subjects closing. Upstream makes the same distinction in its own title
// (`— Recovered` when `not diff.active and diff.resolved`).
//
// The severity is left alone. It is the bounded pass's statement about the
// cluster right now, and a namespace recovering does not make an unrelated
// advisory go away.
func triggerWord(d scheduler.Digest) string {
	resolved := false
	for _, t := range d.Transitions {
		if t.Actionable() {
			return d.Trigger
		}
		resolved = resolved || t.Class == "resolved"
	}
	if resolved && len(d.Escalations) == 0 {
		return "recovered"
	}
	return d.Trigger
}

// scanProse is the bounded pass's summary, unless an escalation supersedes it.
//
// The one-prose-layer rule from the section above, applied a level up. Both
// producers write a Summary against the same contract, so a cycle that
// escalated prints two of them — and reading the real messages, they were never
// complementary. Either the escalation says the same thing at more length (the
// scan wrote "Pod … is ImagePullBackOff … 1 of 2 replicas updated" and the agent
// wrote the same fault with the cause, the blast radius and the surviving
// ReplicaSet), or the scan flatly contradicts the header: a cycle whose bounded
// pass said `ok` and whose escalation said `warning` went out headed WARNING
// with "OK: Cluster is healthy" as its first line of body.
//
// So when the full agent has spoken about this cycle, it is the answer. Nothing
// diffable is lost — the bounded pass's findings reach the digest as transitions
// and never as prose — and the suppressed text is in the process log. With no
// escalation the summary is the only prose in the message and is always printed,
// which is the quiet-cycle case and the one it was written for.
func scanProse(d scheduler.Digest) string {
	if d.Report == nil {
		return ""
	}
	for _, e := range d.Escalations {
		if e.Report != nil && prose(e.Report) != "" {
			return ""
		}
	}
	return strings.TrimSpace(d.Report.Summary)
}

// prose is the one prose block an escalation gets.
//
// The Summary is the only field the model writes about the namespace as a
// whole, so it is the only one that can carry a causal frame. The fallback
// exists because the field is optional in the contract and the bounded pass
// fills it unevenly — a digest whose escalation is nothing but identity lines
// is the defect this whole section is about, so an assessment with no summary
// borrows its leading finding's Detail rather than going silent.
func prose(h *schema.HealthReport) string {
	if s := oneLine(h.Summary); s != "" {
		return s
	}
	for _, f := range h.Findings {
		if d := oneLine(f.Detail); d != "" {
			return d
		}
	}
	return ""
}

// openFor is how long a transition's subject has been on the books.
//
// It replaces the raw `since 2026-08-16T11:27:01Z` the first version printed,
// which is the same information in the form that takes longest to read: what a
// person wants from a resolution is whether it was a blip or a week, and from
// an escalation how long the thing has been getting worse. `new` has no age by
// definition, and an unparseable or future FirstSeen prints nothing rather than
// a negative duration.
func openFor(t scheduler.Transition, at time.Time) string {
	if t.Class == "new" || t.FirstSeen == "" {
		return ""
	}
	first, err := time.Parse(time.RFC3339, t.FirstSeen)
	if err != nil {
		return ""
	}
	d := at.Sub(first)
	if d < time.Second {
		return ""
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// violations keeps the half of a protest that is addressed to a person.
//
// A protest string is a rejection the report tool handed back to the *model*,
// so it is written in the imperative and most of it is remediation:
// `finding 1 ("…") is missing resource_name — name the object the finding is
// about and give a terse stable condition word; these fields identify the
// finding across monitoring runs…`. Posting that verbatim to Slack tells an
// operator to do something only the model can do, in three lines, usually at
// the bottom of an otherwise clean message. Reading one there is what found it.
//
// The violation is the clause before the instruction, and every check in
// internal/sre writes it that way — `… is missing X — <do this>` or
// `…: <what is wrong>. <do this>`. Cutting at the first of those two separators
// keeps the fact and drops the instruction. If neither is present the whole
// string survives, because a protest that goes unprinted is the one outcome
// this section exists to prevent; the full text is in the process log either
// way.
func violations(protests []string) []string {
	var out []string
	for _, p := range protests {
		out = append(out, firstClause(p))
	}
	return out
}

func firstClause(s string) string {
	s = oneLine(s)
	cut := len(s)
	if i := strings.Index(s, " — "); i >= 0 {
		cut = i
	}
	// ". " only counts after the quoted title, which itself may end in a period.
	if i := strings.Index(s, ". "); i >= 0 && i < cut {
		cut = i + 1
	}
	return strings.TrimRight(s[:cut], " ,;:")
}

func writeCaveat(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n*%s*\n", heading)
	for _, l := range lines {
		fmt.Fprintf(b, "• %s\n", firstLine(l))
	}
}

// changedTransitions keeps the classes an operator has to act on or can stop
// worrying about, worst first.
func changedTransitions(ts []scheduler.Transition) []scheduler.Transition {
	var out []scheduler.Transition
	for _, t := range ts {
		if t.Actionable() || t.Class == "resolved" {
			out = append(out, t)
		}
	}
	rank := map[string]int{"critical": 3, "warning": 2, "info": 1}
	sort.SliceStable(out, func(i, j int) bool {
		return rank[strings.ToLower(out[i].Severity)] > rank[strings.ToLower(out[j].Severity)]
	})
	return out
}

func targets(ts []scheduler.Transition) []string {
	var out []string
	for _, t := range ts {
		out = append(out, fmt.Sprintf("`%s` %s (%s)", t.Target(), t.Reason, t.Severity))
	}
	return out
}

// firstLine keeps a multi-line error from taking over the digest. The rest is
// in the process log, which is where somebody debugging goes anyway.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// How much of each prose field survives into the digest, and how many of the
// repeated ones are printed at all.
//
// The summary limit is generous and deliberately not firstLine: a summary's
// second sentence is usually the impact and its first is usually the symptom,
// so keeping one line would drop exactly the half that makes it a diagnosis.
//
// The two caps almost never bind — a fixture namespace holds two objects and
// the busiest real assessment so far filed six findings — but tier 3's
// `online-boutique` has fourteen workloads in it, and a digest that lists all
// of them is one nobody finishes. They print what they dropped, and they only
// engage when there is more than one line to save: eliding a single finding
// behind "…and 1 more" costs a line to save a line.
const (
	summaryLimit = 600
	titleLimit   = 160
	actionLimit  = 300
	maxFindings  = 8
	maxActions   = 5
)

// oneLine folds a multi-line field into a single line.
//
// Findings arrive with newlines in them — a Detail that quotes an event or a
// log excerpt, a RecommendedAction written as a numbered list — and a bare
// newline inside a bullet breaks the indentation the digest uses to show what
// belongs to which finding.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clip bounds a field and says that it did.
//
// The ellipsis is the no-silent-caps rule at its smallest: a truncated detail
// that ends mid-sentence with no marker reads as a model that stopped talking.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

// findingTitle is the Title, unless the Title is the identity triple again.
//
// Models write "ImagePullBackOff on recommendationservice" for a finding whose
// reason is `ImagePullBackOff` and whose resource_name is
// `recommendationservice`, which the bullet has already printed — so the line
// reads `‣ critical `Deployment/recommendationservice` ImagePullBackOff —
// ImagePullBackOff on recommendationservice`. The test is deliberately strict:
// the title is dropped only when *nothing* is left of it after the two tokens
// come out, because a title that adds half a clause is worth keeping and a
// digest that silently eats a headline is the worse failure of the two.
func findingTitle(f schema.Finding) string {
	t := oneLine(f.Title)
	if t == "" {
		return ""
	}
	for _, w := range strings.Fields(strings.ToLower(t)) {
		w = strings.Trim(w, ",.;:'\"()`")
		switch {
		case w == "", len(w) <= 3: // "on", "in", "the", "has", "is"
		case strings.EqualFold(w, f.Reason), strings.EqualFold(w, f.ResourceName), strings.EqualFold(w, f.Kind):
		default:
			return t
		}
	}
	return ""
}

// object renders the finding's identity for the bullet.
//
// Both fields are optional in the contract — a cluster-wide observation has no
// kind and `unidentifiedFindings` only rejects them at the agent's submission,
// not on the bounded pass — so the naive "Kind/Name" prints a bare "/" for a
// finding that names nothing. That reads as a bug in the tool rather than as a
// finding with nothing in those fields, which is the thing worth seeing.
func object(f schema.Finding) string {
	switch {
	case f.Kind != "" && f.ResourceName != "":
		return f.Kind + "/" + f.ResourceName
	case f.ResourceName != "":
		return f.ResourceName
	case f.Kind != "":
		return f.Kind
	}
	return "(unidentified)"
}
