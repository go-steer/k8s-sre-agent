package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-sre-agent/internal/scheduler"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

var at = time.Date(2026, 8, 15, 17, 12, 38, 0, time.UTC)

// capture is a stand-in for switchboard's ingress. It records what arrived and
// replies with whatever the test needs.
type capture struct {
	reqs   []messageRequest
	auth   []string
	keys   []string
	status int
	body   string
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req messageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.reqs = append(c.reqs, req)
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.keys = append(c.keys, r.Header.Get("Idempotency-Key"))
		status := c.status
		if status == 0 {
			status = http.StatusOK
		}
		body := c.body
		if body == "" && status == http.StatusOK {
			body = `{"conversation":"C0123","id":"1723742401.001900"}`
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}
}

func serve(t *testing.T, c *capture) *Switchboard {
	t.Helper()
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	s, err := New(Config{BaseURL: srv.URL, Conversation: "C0123", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digest() scheduler.Digest {
	return scheduler.Digest{
		Cluster: "simian-test", At: at, Trigger: "floor",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallCritical,
			Summary:         "gemma4-vllm cannot roll out",
		},
	}
}

func TestNotifyPostsToTheIngress(t *testing.T) {
	c := &capture{}
	s := serve(t, c)
	if err := s.Notify(context.Background(), digest()); err != nil {
		t.Fatal(err)
	}
	if len(c.reqs) != 1 {
		t.Fatalf("sent %d requests, want 1", len(c.reqs))
	}
	got := c.reqs[0]
	// A bare channel ID, which is the case switchboard's ingress was added for:
	// its Slack adapter used to reject a conversation key with no thread, so an
	// unsolicited digest had nowhere to go.
	if got.Conversation != "C0123" {
		t.Errorf("conversation = %q, want the bare channel", got.Conversation)
	}
	if !strings.Contains(got.Text, "simian-test") || !strings.Contains(got.Text, "CRITICAL") {
		t.Errorf("text does not carry the headline: %q", got.Text)
	}
	if c.auth[0] != "Bearer tok" {
		t.Errorf("Authorization = %q", c.auth[0])
	}
}

// A retry after a timeout must not double-post, and the key has to be stable
// across those retries but distinct between cycles. switchboard fingerprints
// the body as well, so a key reused with different content is refused rather
// than silently answering with the first message's ref — which means a key
// derived from anything coarser than the cycle would break on the second use.
func TestTheIdempotencyKeyIsPerCycle(t *testing.T) {
	c := &capture{}
	s := serve(t, c)
	d := digest()
	if err := s.Notify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := s.Notify(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if c.keys[0] == "" {
		t.Fatal("no Idempotency-Key sent, so a retry after a timeout double-posts")
	}
	if c.keys[0] != c.keys[1] {
		t.Errorf("the same digest produced two keys (%q, %q), so a retry is a new message",
			c.keys[0], c.keys[1])
	}

	next := digest()
	next.At = at.Add(3 * time.Minute)
	if err := s.Notify(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if c.keys[2] == c.keys[0] {
		t.Error("two different cycles share an Idempotency-Key; switchboard would refuse the " +
			"second as a reused key with a different body")
	}
}

// 409 means switchboard's per-process memory of the message is gone — a
// restart between the post and the append is the realistic way there. It is
// not an error to log and drop: the caller still holds the full text, so it is
// a signal to Replace. Surfacing it as a distinct error is what lets a caller
// tell "resend everything" from "this genuinely failed".
func TestAnAppendToAForgottenMessageIsDistinguishable(t *testing.T) {
	c := &capture{status: http.StatusConflict, body: `{"error":"send the full text"}`}
	s := serve(t, c)
	if _, err := s.Append(context.Background(), Ref{Conversation: "C0123", ID: "1.1"}, "line"); !errors.Is(err, ErrForgotten) {
		t.Errorf("Append error = %v, want ErrForgotten", err)
	}
}

// 204 is an edit in place and the ref does not move; 200 with a body means the
// message filled up and switchboard posted the continuation in the same thread,
// so later appends go to the continuation. Getting this backwards would append
// every subsequent line to a message that is already full.
func TestAppendFollowsARollover(t *testing.T) {
	inPlace := &capture{status: http.StatusNoContent}
	s := serve(t, inPlace)
	ref := Ref{Conversation: "C0123", ID: "1.1"}
	got, err := s.Append(context.Background(), ref, "line")
	if err != nil {
		t.Fatal(err)
	}
	if got != ref {
		t.Errorf("ref = %+v after an in-place edit, want it unchanged", got)
	}

	rolled := &capture{status: http.StatusOK, body: `{"conversation":"C0123:1.1","id":"2.2"}`}
	s2 := serve(t, rolled)
	got, err = s2.Append(context.Background(), ref, "line")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "2.2" {
		t.Errorf("ref = %+v after a rollover, want the continuation", got)
	}
}

// The ingress classifies a permanent platform refusal as 404/403 and a
// retryable one as 502, so the status has to survive into the error — it is the
// difference between "this channel is gone, stop" and "try again".
func TestAPermanentFailureKeepsItsStatus(t *testing.T) {
	c := &capture{status: http.StatusNotFound, body: `{"error":"channel_not_found"}`}
	s := serve(t, c)
	err := s.Notify(context.Background(), digest())
	if err == nil {
		t.Fatal("a 404 was reported as success")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error loses the status or the reason: %v", err)
	}
}

// Refusing at construction rather than degrading to a no-op. A monitoring loop
// whose notifications silently go nowhere is the worst failure here: no alerts
// is what an operator reads as good news.
func TestNewRefusesAnUnusableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no base url", Config{Conversation: "C0", Token: "t"}, "BaseURL"},
		{"no conversation", Config{BaseURL: "http://x", Token: "t"}, "Conversation"},
		{"no token", Config{BaseURL: "http://x", Conversation: "C0"}, EnvToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvToken, "")
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("accepted a config with no %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// The header is what somebody scanning a channel reads to decide whether to
// read the rest, so it must not contradict its own body.
//
// Found by running the demo: the bounded pass reported `ok` and the escalation
// it triggered reported `warning`, and the digest went out headed OK. The
// bounded pass is a ten-category scan and an escalation is a full agent
// assessment — when they disagree, the one that looked harder wins.
func TestTheHeadlineTakesTheWorstSeverityInTheDigest(t *testing.T) {
	d := digest()
	d.Report = &schema.HealthReport{OverallSeverity: schema.OverallOK, Summary: "scan clean"}
	d.Escalations = []scheduler.Escalation{{
		Namespace: "shop",
		Report:    &schema.HealthReport{OverallSeverity: schema.OverallWarning},
	}}
	got := Render(d)
	if !strings.Contains(got, "— WARNING —") {
		t.Errorf("header does not reflect the escalation's severity:\n%s", got)
	}

	// And the bounded pass still wins when it is the more severe of the two,
	// so this is a max rather than a preference for the escalation.
	d.Report.OverallSeverity = schema.OverallCritical
	if got := Render(d); !strings.Contains(got, "— CRITICAL —") {
		t.Errorf("header dropped the bounded pass's higher severity:\n%s", got)
	}
}

// An escalation costs a full agent run, and the only thing that buys is the
// diagnosis. So the digest has to carry it.
//
// Found by reading one in Slack: the message said
// `critical Deployment/recommendationservice ImagePullBackOff` and stopped
// there — no cause, no remedy — because the renderer printed only the identity
// triple internal/monitor fingerprints on. Kind, name and reason are what makes
// a finding diffable; Summary, Title, Detail and RecommendedActions are what
// makes it worth waking somebody for.
func TestAnEscalationCarriesItsDiagnosisAndRemedy(t *testing.T) {
	d := digest()
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Elapsed:   63 * time.Second,
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallCritical,
			Summary: "recommendationservice cannot pull\ngcr.io/google-samples/does-not-exist:v0-demo-break " +
				"and has no ready pods; the previous ReplicaSet is still serving.",
			Findings: []schema.Finding{{
				Severity: schema.SeverityCritical,
				Title:    "Image tag does not exist in the registry",
				Detail:   "The registry answers NotFound, so the ReplicaSet cannot start a pod.",
				Kind:     "Deployment", ResourceName: "recommendationservice",
				Reason: "ImagePullBackOff",
			}},
			RecommendedActions: []string{"Roll the image tag back to the previous revision."},
		},
	}}

	got := Render(d)
	for _, want := range []string{
		"recommendationservice cannot pull",        // the report's own summary
		"does-not-exist:v0-demo-break",             // the cause, from that summary
		"Image tag does not exist in the registry", // the finding's title
		"Roll the image tag back",                  // the remedy
		"`Deployment/recommendationservice`",       // and still the identity triple
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest omits %q:\n%s", want, got)
		}
	}
	// A newline inside a prose field must not escape its bullet, or the
	// indentation stops saying which finding a line belongs to.
	if strings.Contains(got, "pull\ngcr.io") {
		t.Errorf("a multi-line summary was not folded:\n%s", got)
	}
	// And the Detail is the layer that goes. It restates the summary a third
	// time — after the summary and the title — which is what made a four-finding
	// assessment of a healthy namespace forty lines long.
	if strings.Contains(got, "The registry answers NotFound") {
		t.Errorf("the detail was printed alongside a summary that already says it:\n%s", got)
	}
}

// The fallback is the argument for dropping Detail: a digest needs one
// paragraph of prose, and Summary is optional in the contract. Without this an
// assessment that filled only the per-finding fields would render as the
// identity-only message the whole section exists to prevent.
func TestAnEscalationWithNoSummaryFallsBackToADetail(t *testing.T) {
	d := digest()
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallCritical,
			Findings: []schema.Finding{
				{Severity: schema.SeverityCritical, Kind: "Pod", ResourceName: "p", Reason: "OOMKilled",
					Detail: "The container asked for 400Mi against a 256Mi limit."},
				{Severity: schema.SeverityWarning, Kind: "Deployment", ResourceName: "d", Reason: "RolloutIncomplete",
					Detail: "should not be reached — only the leading detail stands in"},
			},
		},
	}}
	got := Render(d)
	if !strings.Contains(got, "400Mi against a 256Mi limit") {
		t.Errorf("an escalation with no summary printed no prose at all:\n%s", got)
	}
	if strings.Contains(got, "only the leading detail") {
		t.Errorf("the fallback printed every detail rather than one:\n%s", got)
	}
}

// One prose layer per cycle, not just per escalation.
//
// Both producers write a Summary against the same contract, so a cycle that
// escalated has two. Reading the real messages, they were never complementary:
// the scan restates the fault the agent diagnosed, or — worse — a bounded `ok`
// sits directly under a WARNING header as the first line of body.
func TestTheScanSummaryYieldsToAnEscalation(t *testing.T) {
	d := digest()
	d.Report = &schema.HealthReport{
		OverallSeverity: schema.OverallOK,
		Summary:         "OK: Cluster is healthy. Control-plane metrics unavailable.",
	}
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallWarning,
			Summary:         "storefront-web serves traffic but has no PodDisruptionBudget.",
		},
	}}
	got := Render(d)
	if strings.Contains(got, "Cluster is healthy") {
		t.Errorf("a bounded `ok` printed under a WARNING header:\n%s", got)
	}
	if !strings.Contains(got, "no PodDisruptionBudget") {
		t.Errorf("the escalation's prose went missing with it:\n%s", got)
	}

	// A quiet cycle is the case the scan summary was written for, and there it
	// is the only prose in the message.
	d.Escalations = nil
	if got := Render(d); !strings.Contains(got, "Cluster is healthy") {
		t.Errorf("a cycle with no escalation printed no prose at all:\n%s", got)
	}

	// An escalation that produced no prose supersedes nothing — otherwise a
	// summary-less assessment costs the digest both layers at once.
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Report:    &schema.HealthReport{OverallSeverity: schema.OverallWarning},
	}}
	if got := Render(d); !strings.Contains(got, "Cluster is healthy") {
		t.Errorf("a prose-less escalation suppressed the only prose in the digest:\n%s", got)
	}
}

// The prose fields are optional and the bounded pass fills them unevenly, so
// every one of them has to be safe to be empty. The failure this guards is
// cosmetic and specific: "Kind/Name" on a finding with neither prints a bare
// slash, which reads as a broken renderer rather than as a finding that names
// nothing.
func TestAFindingWithNoProseStillRenders(t *testing.T) {
	d := digest()
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallWarning,
			Findings:        []schema.Finding{{Severity: schema.SeverityWarning, Reason: "SlowWebhookRisk"}},
		},
	}}
	got := Render(d)
	if strings.Contains(got, "`/`") {
		t.Errorf("an unidentified finding rendered as a bare separator:\n%s", got)
	}
	if !strings.Contains(got, "SlowWebhookRisk") {
		t.Errorf("digest dropped the finding entirely:\n%s", got)
	}
}

// Nothing is truncated on the way to Slack — switchboard chunks a long message
// into ordered in-thread posts — so the clip is about a single finding not
// running to a page. It has to say that it clipped: a detail that ends
// mid-sentence with no marker reads as a model that stopped talking.
func TestALongSummaryIsClippedVisibly(t *testing.T) {
	d := digest()
	d.Escalations = []scheduler.Escalation{{
		Namespace: "storefront",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallWarning,
			Summary:         strings.Repeat("x", summaryLimit+50),
			Findings: []schema.Finding{{
				Severity: schema.SeverityWarning, Title: "wordy",
				Kind: "Pod", ResourceName: "p", Reason: "Whatever",
			}},
		},
	}}
	got := Render(d)
	if !strings.Contains(got, "…") {
		t.Errorf("a clipped summary did not say so:\n%s", got)
	}
	if strings.Contains(got, strings.Repeat("x", summaryLimit+1)) {
		t.Errorf("summary was not clipped:\n%s", got)
	}
}

// The caps exist for tier 3's fourteen-workload namespace, and the no-silent-caps
// rule applies to a digest as much as to a run: an elided finding must leave a
// mark, or the message reads like a shorter assessment than the one that ran.
func TestACappedListSaysWhatItDropped(t *testing.T) {
	var findings []schema.Finding
	for i := range maxFindings + 4 {
		findings = append(findings, schema.Finding{
			Severity: schema.SeverityWarning, Kind: "Pod",
			ResourceName: fmt.Sprintf("p%d", i), Reason: "MissingProbes",
		})
	}
	var actions []string
	for i := range maxActions + 3 {
		actions = append(actions, fmt.Sprintf("do thing %d", i))
	}
	d := digest()
	d.Escalations = []scheduler.Escalation{{
		Namespace: "boutique",
		Report: &schema.HealthReport{
			OverallSeverity: schema.OverallWarning, Summary: "lots going on",
			Findings: findings, RecommendedActions: actions,
		},
	}}
	got := Render(d)
	for _, want := range []string{"and 4 more finding(s)", "and 3 more recommended action(s)", "p0", "do thing 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("digest omits %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, fmt.Sprintf("p%d", maxFindings+3)) {
		t.Errorf("the cap did not bind:\n%s", got)
	}

	// One over the cap is not worth eliding: "…and 1 more" costs the line it
	// saves, and it costs the operator the finding.
	d.Escalations[0].Report.Findings = findings[:maxFindings+1]
	if got := Render(d); strings.Contains(got, "more finding(s)") {
		t.Errorf("a single finding over the cap was elided behind a placeholder:\n%s", got)
	}
}

// A title that only restates the identity triple is dropped, and one that adds
// half a clause is not. The second half is the one that matters: eating a
// headline to save a line is the worse of the two failures.
func TestARedundantTitleIsDroppedAndAnInformativeOneIsNot(t *testing.T) {
	render := func(title string) string {
		d := digest()
		d.Escalations = []scheduler.Escalation{{
			Namespace: "storefront",
			Report: &schema.HealthReport{
				OverallSeverity: schema.OverallCritical,
				Findings: []schema.Finding{{
					Severity: schema.SeverityCritical, Title: title,
					Kind: "Deployment", ResourceName: "recommendationservice",
					Reason: "ImagePullBackOff",
				}},
			},
		}}
		return Render(d)
	}

	got := render("ImagePullBackOff on recommendationservice")
	if strings.Contains(got, "ImagePullBackOff — ImagePullBackOff") {
		t.Errorf("the title restated the reason and was printed anyway:\n%s", got)
	}
	if !strings.Contains(got, "ImagePullBackOff") {
		t.Errorf("dropping the title took the reason with it:\n%s", got)
	}

	got = render("ImagePullBackOff on recommendationservice: the tag was never pushed")
	if !strings.Contains(got, "the tag was never pushed") {
		t.Errorf("a title that adds something was dropped:\n%s", got)
	}
}

// A cycle whose only changes are resolutions is a recovery, and the header is
// the only line most people read.
//
// Found by reading one in Slack: the fault-cleared cycle went out headed
// `INFO — changes`, where INFO was an unrelated control-plane advisory from the
// bounded scan and the entire body was two subjects closing. Nothing in that
// header says the thing you were paged about is over.
func TestACycleThatOnlyClosesThingsReadsAsARecovery(t *testing.T) {
	d := digest()
	d.Trigger = "changes"
	d.Transitions = []scheduler.Transition{
		{Class: "resolved", Namespace: "storefront", Kind: "Pod", Name: "web-x", Reason: "ImagePullBackOff",
			Severity: "critical", FirstSeen: at.Add(-88 * time.Second).Format(time.RFC3339)},
	}
	got := Render(d)
	if !strings.Contains(got, "— recovered (") {
		t.Errorf("a resolution-only cycle did not read as a recovery:\n%s", got)
	}
	// The severity is the bounded pass's statement about the cluster now, and a
	// namespace recovering does not retire an unrelated advisory.
	if !strings.Contains(got, "— CRITICAL —") {
		t.Errorf("the recovery word ate the severity:\n%s", got)
	}

	// Anything still open is not a recovery, whichever way it arrived.
	d.Transitions = append(d.Transitions, scheduler.Transition{
		Class: "new", Namespace: "shop", Kind: "Pod", Name: "p", Reason: "OOMKilled", Severity: "warning",
	})
	if got := Render(d); strings.Contains(got, "recovered") {
		t.Errorf("a cycle with a new subject in it claimed recovery:\n%s", got)
	}
}

// How long a subject was open is what a person wants from a closing line —
// blip or week — and an RFC3339 stamp is the same fact in the form that takes
// longest to read.
func TestAClosedSubjectSaysHowLongItWasOpen(t *testing.T) {
	tr := func(class, firstSeen string) string {
		d := digest()
		d.Transitions = []scheduler.Transition{{
			Class: class, Namespace: "storefront", Kind: "Pod", Name: "web-x",
			Reason: "ImagePullBackOff", Severity: "critical", FirstSeen: firstSeen,
		}}
		return Render(d)
	}

	if got := tr("resolved", at.Add(-88*time.Second).Format(time.RFC3339)); !strings.Contains(got, "open 1m") {
		t.Errorf("a resolution did not say how long it was open:\n%s", got)
	}
	if got := tr("resolved", at.Add(-14*24*time.Hour).Format(time.RFC3339)); !strings.Contains(got, "open 14d") {
		t.Errorf("a fortnight-old subject rendered badly:\n%s", got)
	}
	// A `new` subject has no age by definition, and a stamp we cannot parse or
	// that is in the future must print nothing rather than a negative duration.
	for _, bad := range []string{"", "not-a-timestamp", at.Add(time.Hour).Format(time.RFC3339)} {
		if got := tr("resolved", bad); strings.Contains(got, "open ") {
			t.Errorf("FirstSeen %q produced an age:\n%s", bad, got)
		}
	}
	if got := tr("new", at.Add(-time.Hour).Format(time.RFC3339)); strings.Contains(got, "open ") {
		t.Errorf("a new subject was given an age:\n%s", got)
	}
}

// A protest is a rejection written to the model, and most of it is an
// instruction only the model can act on. The digest keeps the fact and drops
// the imperative — but it must still print something, because a report accepted
// under protest must not read like a clean one.
func TestAProtestPrintsTheViolationAndNotTheInstruction(t *testing.T) {
	d := digest()
	d.Protests = []string{
		`finding 1 ("Control plane health check unavailable") is missing resource_name — name the ` +
			`object the finding is about and give a terse stable condition word; these fields identify ` +
			`the finding across monitoring runs and the report is not usable without them`,
		`finding 2 ("Service has no endpoints"): reason "PodsNotReady" is a pod-level condition, but ` +
			`this finding names a Service. Name this object's own failure mode`,
	}
	got := Render(d)
	for _, want := range []string{
		`finding 1 ("Control plane health check unavailable") is missing resource_name`,
		`is a pod-level condition, but this finding names a Service`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest omits the violation %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"name the object the finding is about", "Name this object's own failure mode"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("digest carried an instruction addressed to the model (%q):\n%s", unwanted, got)
		}
	}

	// A protest in a shape neither separator matches survives whole. Losing one
	// is worse than printing an awkward one.
	d.Protests = []string{"something went wrong in a way nobody anticipated"}
	if got := Render(d); !strings.Contains(got, "nobody anticipated") {
		t.Errorf("an unparseable protest was dropped:\n%s", got)
	}
}
