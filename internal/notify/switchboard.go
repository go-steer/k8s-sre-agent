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

// Package notify sends a scheduler digest somewhere a human will see it.
//
// The only implementation is switchboard, the chat gateway that fronts Slack
// and Google Chat for the go-steer agents. We do not write a Slack client: the
// Python agent this repo ports has a 530-line slack_notifier.py, switchboard
// already speaks both platforms behind one adapter, and a second client here
// would be a second thing to keep working against Slack's API.
//
// # The seam
//
// switchboard gained an outbound ingress (go-steer/switchboard#21, merged as
// #22) for exactly this: every reply it sent before then echoed an inbound
// event's conversation, so it could only speak into a thread chat had already
// created, and a monitoring cycle with an escalation to raise at 3am has no
// such thread. The ingress is off unless started with --ingress-addr, takes a
// bearer token from its own env var, and can be confined to named
// conversations with --ingress-allow.
//
// Deployment note that is easy to skip: --ingress-allow defaults to *any*
// conversation the bot can reach, with only a startup warning. Give the
// scheduler its own channel and name it. switchboard's own review pinned the
// reason — a token holder may edit any message the bot posted in an allowlisted
// conversation, including the router's replies to humans, because the ingress
// does not track which caller posted what.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-steer/k8s-sre-agent/internal/scheduler"
)

// EnvToken is the environment variable holding the ingress bearer token.
//
// An env var rather than a flag, matching how switchboard reads its own: a
// token passed as a flag shows up in a process listing and in a Kubernetes
// manifest's args.
const EnvToken = "SRE_SWITCHBOARD_TOKEN"

const ingressPath = "/v1/messages"

// Config is one switchboard target.
type Config struct {
	// BaseURL is the ingress listener, e.g. http://switchboard:8081.
	BaseURL string

	// Conversation is where digests go. A bare channel ID posts at the top
	// level of that channel; a "<channel>:<thread>" key posts into that thread.
	//
	// The channel form is the one this command wants, and it is the capability
	// the ingress was added for — switchboard's Slack adapter used to reject a
	// key with no thread outright.
	Conversation string

	// Token authenticates to the ingress. Empty reads EnvToken.
	Token string

	// Timeout bounds one request. The ingress talks to Slack synchronously, so
	// this has to cover a platform call, not just the local hop.
	Timeout time.Duration

	// Client is the HTTP client. Nil builds one from Timeout.
	Client *http.Client
}

// Switchboard posts scheduler digests through switchboard's outbound ingress.
type Switchboard struct {
	cfg    Config
	client *http.Client
	token  string
}

// New builds a notifier, or reports why it cannot.
//
// It refuses rather than degrading to a no-op. A monitoring loop whose
// notifications silently go nowhere is the worst failure available here: the
// absence of alerts is exactly what an operator reads as good news, which is
// the same reason the scheduler prints its heartbeat and its zero counts.
func New(cfg Config) (*Switchboard, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("notify: BaseURL is required")
	}
	if strings.TrimSpace(cfg.Conversation) == "" {
		return nil, fmt.Errorf("notify: Conversation is required — name the channel digests go to")
	}
	token := cfg.Token
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	if token == "" {
		return nil, fmt.Errorf("notify: no ingress token in $%s", EnvToken)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Switchboard{cfg: cfg, client: client, token: token}, nil
}

// Notify renders the digest and posts it.
//
// One message per digest, and the scheduler only produces a digest when
// something changed or the heartbeat is due — so a quiet cluster is a quiet
// channel, which is the whole reason there is a differ in front of the agent.
//
// Posting the *complete* digest is deliberate for now and is the smaller half
// of what the ingress offers. switchboard also supports post-then-append, which
// would let an operator see the cycle's header immediately and each escalation
// land under it as it completes — worth having, because a cold cycle spent
// 3m14s escalating with nothing on screen. It needs a change on this side
// rather than that one: scheduler.Notifier is called once, at the end of a
// cycle, with everything already decided. Left until a real digest has been
// looked at, because restructuring the scheduler around a rendering nobody has
// seen is the wrong order.
func (s *Switchboard) Notify(ctx context.Context, d scheduler.Digest) error {
	body := messageRequest{
		Conversation: s.cfg.Conversation,
		Text:         Render(d),
	}
	// Keyed on the cluster and the cycle's own timestamp so a retry after a
	// timeout cannot double-post. switchboard fingerprints the body too, so a
	// key reused with different content is refused rather than silently
	// answering with the first message's ref.
	key := fmt.Sprintf("%s-%d", d.Cluster, d.At.UnixNano())
	_, err := s.post(ctx, body, key)
	return err
}

type messageRequest struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
}

// Ref identifies a posted message, for a later edit.
type Ref struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id"`
}

func (s *Switchboard) post(ctx context.Context, body messageRequest, idempotencyKey string) (Ref, error) {
	var ref Ref
	err := s.do(ctx, http.MethodPost, body, idempotencyKey, &ref)
	return ref, err
}

// Append adds a line to a message already posted.
//
// Not used by Notify yet — see its comment — but it is the half worth having
// and it is one call, so it lives here rather than being rediscovered later.
//
// Two answers other than success are normal and are handled by the caller
// rather than hidden:
//
//   - 409 means switchboard no longer remembers the message's text. Its map is
//     per-process and bounded, so a restart between the post and the append
//     lands here; the caller still holds the full text and should replace
//     instead. ErrForgotten reports it.
//   - 200 with a body means the message filled up and switchboard posted the
//     continuation as a reply in the same thread. The returned ref is the
//     continuation's, and later appends go to that one.
func (s *Switchboard) Append(ctx context.Context, ref Ref, line string) (Ref, error) {
	body := messageRequest{Conversation: ref.Conversation, ID: ref.ID, Append: line}
	var next Ref
	err := s.do(ctx, http.MethodPatch, body, "", &next)
	if err != nil {
		return Ref{}, err
	}
	if next.ID == "" {
		return ref, nil // 204: edited in place, same message
	}
	return next, nil // 200: rolled over into a continuation
}

// Replace rewrites a posted message's whole body. It is the fallback when
// Append reports ErrForgotten.
func (s *Switchboard) Replace(ctx context.Context, ref Ref, text string) error {
	body := messageRequest{Conversation: ref.Conversation, ID: ref.ID, Text: text}
	return s.do(ctx, http.MethodPatch, body, "", nil)
}

// ErrForgotten is a 409 from an append: switchboard no longer holds the
// message's text, so the caller must send the full body instead.
var ErrForgotten = fmt.Errorf("notify: switchboard no longer remembers this message's text")

func (s *Switchboard) do(ctx context.Context, method string, body messageRequest, idempotencyKey string, out *Ref) error {
	blob, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("notify: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(s.cfg.BaseURL, "/")+ingressPath, bytes.NewReader(blob))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: %s %s: %w", method, ingressPath, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusConflict:
		return ErrForgotten
	case resp.StatusCode >= 300:
		// The ingress classifies a permanent platform refusal as 404/403 and a
		// retryable one as 502, so the status is worth keeping in the message:
		// it is the difference between "this channel is gone" and "try again".
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("notify: %s %s: %s: %s", method, ingressPath,
			resp.Status, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out); err != nil {
		return fmt.Errorf("notify: decode response: %w", err)
	}
	return nil
}
