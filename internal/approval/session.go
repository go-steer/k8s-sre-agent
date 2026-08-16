package approval

import (
	"context"
	"errors"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// DefaultMaxRequests bounds how many approval prompts one Send may raise.
//
// It is a loop bound, not a policy: an agent that keeps re-requesting a write
// it was denied would otherwise prompt forever, and the person being prompted
// is the one resource in this system that cannot be scaled. Twenty is far above
// any real remediation — the write specialist is told to batch, so a twelve
// object cleanup is one prompt — and far below "someone stopped reading".
const DefaultMaxRequests = 20

// ErrTooManyRequests is recorded on the decisions denied by the bound.
var ErrTooManyRequests = errors.New("approval: too many approval requests in one turn")

// Session drives a conversation with an agent that may pause for approval.
//
// Send handles the full interrupt/resume cycle, so a caller writes one message
// and gets back one completed turn regardless of how many writes happened
// inside it. That is the point: the approval protocol is a property of the
// runtime, and every entry point to this agent — CLI, scheduler, Slack — would
// otherwise have to reimplement it, differently, and get the fail-closed
// details right each time.
type Session struct {
	Runner    *runner.Runner
	UserID    string
	SessionID string

	// Approver decides each pending write. Nil denies everything.
	Approver Approver

	// MaxRequests overrides DefaultMaxRequests. Negative disables the bound.
	MaxRequests int

	RunConfig adkagent.RunConfig
}

// Turn is everything that happened in one Send.
type Turn struct {
	// Events are the events of every leg, concatenated in order — the first
	// run plus one resume per round of approvals. A caller extracting the
	// health report should read the last one it finds.
	Events []*session.Event

	// Decisions are the writes that were put to the Approver, in order.
	Decisions []Decision

	// Resumes are the messages sent back to unblock the agent, one per round
	// of approvals. Kept because the runner does not echo an incoming message
	// as an event, so this is the only record that the answers to one round
	// travelled together — the property Answer exists to preserve.
	Resumes []*genai.Content
}

// Approved counts the writes that were allowed to run.
func (t *Turn) Approved() int {
	n := 0
	for _, d := range t.Decisions {
		if d.Approved {
			n++
		}
	}
	return n
}

// Send delivers a message and runs until the agent stops asking for approval.
//
// The returned Turn is complete whether or not the error is nil. An error here
// means either the run itself failed, or one or more approvals could not be
// taken — and in the second case the run still finished, with those writes
// denied, so that the transcript stays well-formed and the agent gets to report
// the outcome rather than dying mid-write.
func (s *Session) Send(ctx context.Context, msg *genai.Content) (*Turn, error) {
	turn := &Turn{}
	limit := s.MaxRequests
	if limit == 0 {
		limit = DefaultMaxRequests
	}
	approver := s.Approver
	if approver == nil {
		approver = DenyAll()
	}

	next := msg
	var failures []error
	for {
		events, err := s.leg(ctx, next)
		turn.Events = append(turn.Events, events...)
		if err != nil {
			return turn, err
		}
		if unknown := Unanswerable(events); len(unknown) > 0 {
			return turn, fmt.Errorf("approval: the agent is waiting on %v, which this "+
				"package cannot answer; the run is stuck", unknown)
		}

		pending := Pending(events)
		if len(pending) == 0 {
			return turn, errors.Join(failures...)
		}

		decisions := make([]Decision, 0, len(pending))
		for _, r := range pending {
			d := Decision{Request: r}
			switch {
			case limit >= 0 && len(turn.Decisions)+len(decisions) >= limit:
				d.Err = fmt.Errorf("%w (limit %d): %s", ErrTooManyRequests, limit, r.Tool)
			default:
				ok, err := approver.Approve(ctx, r)
				// A failed decision is not an approval. Recording the error and
				// denying keeps one broken channel from becoming a blanket yes.
				d.Approved, d.Err = ok && err == nil, err
			}
			if d.Err != nil {
				failures = append(failures, d.Err)
			}
			decisions = append(decisions, d)
		}
		turn.Decisions = append(turn.Decisions, decisions...)
		next = Answer(decisions)
		turn.Resumes = append(turn.Resumes, next)
	}
}

// leg runs the runner once and drains its events.
func (s *Session) leg(ctx context.Context, msg *genai.Content) ([]*session.Event, error) {
	var events []*session.Event
	for ev, err := range s.Runner.Run(ctx, s.UserID, s.SessionID, msg, s.RunConfig) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}
