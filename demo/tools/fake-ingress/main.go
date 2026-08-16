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

// Command fake-ingress stands in for switchboard's outbound ingress.
//
// It speaks the same contract — POST/PATCH /v1/messages, bearer auth,
// {conversation,id} responses — and prints what it receives instead of posting
// to Slack. That makes the whole demo runnable with no Slack workspace, no app
// tokens and no Socket Mode connection, which is most of the setup cost of the
// real thing (see go-steer/switchboard#23).
//
// It is a demo aid and not a test double for switchboard: it does not implement
// idempotency replay, the remembered-text map behind append, or the platform
// error classification. What it does faithfully is the wire shape, so a caller
// that works against this is sending something the real ingress will accept.
//
//	fake-ingress -addr 127.0.0.1:8099 -token demo-ingress-token
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type request struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id,omitempty"`
	Text         string `json:"text,omitempty"`
	Append       string `json:"append,omitempty"`
}

type response struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	token := flag.String("token", "", "bearer token callers must present (also read from FAKE_INGRESS_TOKEN)")
	flag.Parse()

	tok := *token
	if tok == "" {
		tok = os.Getenv("FAKE_INGRESS_TOKEN")
	}
	if tok == "" {
		log.Fatal("fake-ingress: -token or $FAKE_INGRESS_TOKEN is required; an unauthenticated " +
			"ingress is not the shape the real one has, and testing against it would prove less")
	}

	srv := &server{token: tok, posted: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", srv.handle)

	log.Printf("fake-ingress listening on %s", *addr)
	log.Fatal((&http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}).ListenAndServe())
}

type server struct {
	token string

	mu     sync.Mutex
	seq    int
	posted map[string]string // id → current text, so append can print the whole thing
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"malformed json"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.post(w, r, req)
	case http.MethodPatch:
		s.patch(w, req)
	default:
		w.Header().Set("Allow", "POST, PATCH")
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *server) post(w http.ResponseWriter, r *http.Request, req request) {
	if req.Conversation == "" || req.Text == "" {
		http.Error(w, `{"error":"conversation and text are required"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("1700000000.%06d", s.seq)
	s.posted[id] = req.Text
	s.mu.Unlock()

	render("POSTED to "+req.Conversation, r.Header.Get("Idempotency-Key"), req.Text)
	writeJSON(w, http.StatusOK, response{Conversation: req.Conversation, ID: id})
}

func (s *server) patch(w http.ResponseWriter, req request) {
	if req.ID == "" {
		http.Error(w, `{"error":"id is required"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	cur, known := s.posted[req.ID]
	switch {
	case req.Text != "":
		s.posted[req.ID] = req.Text
	case req.Append != "" && known:
		cur += "\n" + req.Append
		s.posted[req.ID] = cur
	}
	text := s.posted[req.ID]
	s.mu.Unlock()

	// The real ingress answers 409 when it no longer holds the message's text,
	// and a caller is expected to fall back to sending the full body. Worth
	// reproducing: it is the branch a switchboard restart takes, and a demo
	// that never exercises it hides a case the caller has to handle.
	if req.Append != "" && !known {
		http.Error(w, `{"error":"no remembered text for that message; send the full text"}`,
			http.StatusConflict)
		return
	}
	render("EDITED "+req.ID, "", text)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// render prints the message the way a chat client would show it, so a demo
// audience sees the digest rather than a JSON blob.
func render(heading, idempotencyKey, text string) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n┌─ %s", heading)
	if idempotencyKey != "" {
		fmt.Fprintf(&b, "  (Idempotency-Key: %s)", idempotencyKey)
	}
	b.WriteString("\n")
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintf(&b, "│ %s\n", line)
	}
	b.WriteString("└─\n")
	fmt.Print(b.String())
}
