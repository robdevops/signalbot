package llm

import (
	"context"
	"errors"
	"testing"
)

// script is a Model that answers with its replies in turn and records each
// turn it was given.
type script struct {
	replies []Completion
	turns   []Turn
}

func (s *script) Chat(_ context.Context, t Turn) (Completion, error) {
	s.turns = append(s.turns, t)
	if len(s.turns) > len(s.replies) {
		return Completion{}, errors.New("no more replies")
	}
	return s.replies[len(s.turns)-1], nil
}

func call(name, args string) ToolCall {
	var tc ToolCall
	tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments = "c1", "function", name, args
	return tc
}

var echo = Tool{Name: "echo", Label: "echo", Def: "echo-def", Run: func(_ context.Context, args string) (Result, error) {
	return Result{Text: "echo " + args, Images: []Image{{Data: []byte{1}, Caption: "pic"}}}, nil
}}

// A tool call runs the tool and sends the result back; usage is summed, the
// tool's pictures come back, and a forced tool is required on the first
// request only.
func TestLoopRunsTools(t *testing.T) {
	m := &script{replies: []Completion{
		{ToolCalls: []ToolCall{call("echo", "x")}, InputTokens: 10, Cost: 0.001},
		{Text: "done", InputTokens: 20, Cost: 0.002},
	}}
	res, err := Loop{m}.Complete(context.Background(), Request{System: "s", Tools: []Tool{echo}, Force: "echo"})
	if err != nil || res.Text != "done" || res.InputTokens != 30 || len(res.Images) != 1 || len(res.Used) != 1 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if m.turns[0].Force != "echo" || m.turns[1].Force != "" || m.turns[0].Tools[0] != "echo-def" {
		t.Fatalf("turns %+v", m.turns)
	}
	last := m.turns[1].Messages[len(m.turns[1].Messages)-1]
	if last.Role != "tool" || last.Content != "echo x" || m.turns[0].Messages[0].Content != "s" {
		t.Fatalf("messages %+v", m.turns[1].Messages)
	}
}

// A call to a tool that wasn't offered, or to a provider tool, gets an
// error result, not a crash.
func TestLoopUnknownTool(t *testing.T) {
	m := &script{replies: []Completion{{ToolCalls: []ToolCall{call("nope", "")}}, {Text: "ok"}}}
	provider := Tool{Name: "nope", Def: "server"} // no Run
	if _, err := (Loop{m}).Complete(context.Background(), Request{Tools: []Tool{provider}}); err != nil {
		t.Fatal(err)
	}
	if got := m.turns[1].Messages[len(m.turns[1].Messages)-1].Content; got != "error: there is no tool named nope" {
		t.Fatalf("tool result %q", got)
	}
}

func TestLoopRoundCap(t *testing.T) {
	m := &script{}
	for range MaxRounds + 2 {
		m.replies = append(m.replies, Completion{ToolCalls: []ToolCall{call("echo", "")}})
	}
	res, err := Loop{m}.Complete(context.Background(), Request{Tools: []Tool{echo}})
	if err != nil || res.Text != "Sorry, I couldn't finish that." || len(m.turns) != MaxRounds {
		t.Fatalf("got %q after %d requests, %v", res.Text, len(m.turns), err)
	}
}
