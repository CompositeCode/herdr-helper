package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type response struct {
	args string
	out  string
	err  error
}

type scriptedRunner struct {
	t         *testing.T
	responses []response
	called    []string
}

func (s *scriptedRunner) Run(_ context.Context, args ...string) (string, error) {
	s.t.Helper()
	got := strings.Join(args, " ")
	s.called = append(s.called, got)
	if len(s.responses) == 0 {
		s.t.Fatalf("unexpected call: %s", got)
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	if got != r.args {
		s.t.Fatalf("call = %q, want %q", got, r.args)
	}
	return r.out, r.err
}

const codexAgent = `{"id":"cli:agent:list","result":{"agents":[{"agent":"codex","agent_status":"idle","pane_id":"w1:p1","agent_session":{"kind":"id","value":"01a13c1d-73ec-72b3-8159-5eb85647d384"}}]}}`
const noAgents = `{"id":"cli:agent:list","result":{"agents":[]}}`

func TestDryRunUsesNativeSession(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{{args: "agent list", out: codexAgent}}}
	var out, errOut bytes.Buffer
	code := run([]string{"--dry-run"}, &out, &errOut, s)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "- codex [w1:p1]: codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384") {
		t.Fatalf("output: %q", out.String())
	}
	if len(s.responses) != 0 {
		t.Fatalf("unused responses: %d", len(s.responses))
	}
}

func TestClearAgentSequence(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{
		{args: "agent list", out: codexAgent},
		{args: "pane run w1:p1 /clear"},
		{args: "pane run w1:p1 /exit"},
		{args: "agent list", out: noAgents},
		{args: "pane read w1:p1 --source recent-unwrapped --lines 120", out: "codex resume other-id"},
		{args: "pane run w1:p1 clear"},
	}}
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut, s)
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384") {
		t.Fatalf("original session lost: %q", out.String())
	}
}

func TestMissingResumeIsErrorAfterClearingPane(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{
		{args: "agent list", out: `{"result":{"agents":[{"agent":"codex","agent_status":"idle","pane_id":"w1:p1"}]}}`},
		{args: "pane run w1:p1 /clear"},
		{args: "pane run w1:p1 /exit"},
		{args: "agent list", out: noAgents},
		{args: "pane read w1:p1 --source recent-unwrapped --lines 120", out: "no resume line here"},
		{args: "pane run w1:p1 clear"},
	}}
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut, s)
	if code != 1 || !strings.Contains(errOut.String(), "no resume command was provided") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if len(s.responses) != 0 {
		t.Fatalf("terminal was not cleared")
	}
}

func TestWorkingAgentWaitFailureDoesNotInterrupt(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{
		{args: "agent list", out: `{"result":{"agents":[{"agent":"claude","agent_status":"working","pane_id":"w1:p2"}]}}`},
		{args: "agent wait w1:p2 --until idle --until done --timeout 15000", err: errors.New("timed out")},
	}}
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut, s)
	if code != 1 || !strings.Contains(errOut.String(), "timed out") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if len(s.called) != 2 {
		t.Fatalf("unexpected calls: %v", s.called)
	}
}

func TestCommandFailureKeepsKnownResume(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{
		{args: "agent list", out: codexAgent},
		{args: "pane run w1:p1 /clear", err: errors.New("socket closed")},
	}}
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut, s)
	if code != 1 || !strings.Contains(errOut.String(), "socket closed") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384") {
		t.Fatalf("lost resume command: %q", out.String())
	}
}

func TestResumeOutputPatterns(t *testing.T) {
	cases := []struct{ kind, output, want string }{
		{"codex", "To continue: codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384", "codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384"},
		{"claude", "claude --resume abc-123", "claude --resume abc-123"},
		{"cursor", "agent --resume=chat-123", "agent --resume=chat-123"},
		{"codex", "no command", ""},
	}
	for _, tc := range cases {
		if got := resumeFromOutput(tc.output, tc.kind); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestResumeCommandQuotesSessionID(t *testing.T) {
	a := agent{Session: &struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}{Kind: "id", Value: "id with ' quote"}}
	got := resumeFromSession(a, profile{Resume: []string{"custom", "--resume", "{id}"}})
	if got != `custom --resume 'id with '\'' quote'` {
		t.Fatalf("got %q", got)
	}
}

func TestExitTimeoutReturnsResume(t *testing.T) {
	s := &scriptedRunner{t: t, responses: []response{
		{args: "pane run w1:p1 /clear"},
		{args: "pane run w1:p1 /exit"},
		{args: "agent list", out: codexAgent},
	}}
	cmd, err := clearAgent(context.Background(), s, agent{Kind: "codex", PaneID: "w1:p1"}, profile{Clear: "/clear", Exit: "/exit"}, "codex resume old", time.Nanosecond)
	if cmd != "codex resume old" || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("cmd %q, err %v", cmd, err)
	}
}
