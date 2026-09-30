package main

import (
	"encoding/json"
	"testing"
)

// TestResponsesToChatPreservesToolHistory guards against the agent-loop bug:
// when the host hands the plugin a responses-form payload (as pi via
// /backend-api/codex/responses does), function_call and function_call_output
// items carry no "role" field. responsesToChat used to map them to
// {"role": nil}, and sanitizeChatMessages then silently dropped them — the
// upstream model never saw its own tool calls or any tool results and
// re-issued the same calls every turn (agent loops).
func TestResponsesToChatPreservesToolHistory(t *testing.T) {
	payload := []byte(`{
  "model": "mimo-v2.6-flash-free",
  "stream": true,
  "instructions": "You are a coding assistant.",
  "input": [
    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "list the project files"}]},
    {"type": "function_call", "call_id": "call_001", "name": "bash", "arguments": "{\"command\":\"ls -la\"}"},
    {"type": "function_call_output", "call_id": "call_001", "output": "main.go main_test.go README.md"},
    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "now audit the code"}]}
  ]
}`)

	route := modelRoute{Model: "mimo-v2.6-flash-free", Endpoint: "chat"}
	body, err := prepareUpstreamBody(executorRequest{Payload: payload}, route)
	if err != nil {
		t.Fatalf("prepareUpstreamBody: %v", err)
	}

	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var roles []string
	var toolCallMsgs, toolResultMsgs int
	for _, m := range out.Messages {
		role, _ := m["role"].(string)
		roles = append(roles, role)
		if role == "assistant" && m["tool_calls"] != nil {
			toolCallMsgs++
		}
		if role == "tool" {
			toolResultMsgs++
		}
	}

	if toolCallMsgs != 1 {
		t.Errorf("assistant tool_call message = %d, want 1 (roles: %v)", toolCallMsgs, roles)
	}
	if toolResultMsgs != 1 {
		t.Errorf("tool result message = %d, want 1 (roles: %v)", toolResultMsgs, roles)
	}
	// The tool result must carry the matching tool_call_id.
	for _, m := range out.Messages {
		if role, _ := m["role"].(string); role == "tool" {
			if id, _ := m["tool_call_id"].(string); id != "call_001" {
				t.Errorf("tool_call_id = %q, want call_001", id)
			}
			if c, _ := m["content"].(string); c != "main.go main_test.go README.md" {
				t.Errorf("tool content = %q, want the original output text", c)
			}
		}
	}
}

// TestResponsesToChatMergesParallelCalls verifies that consecutive
// function_call items (one turn with several parallel tool calls) collapse
// into a single assistant message with multiple tool_calls, and that each
// function_call_output becomes its own tool message in order.
func TestResponsesToChatMergesParallelCalls(t *testing.T) {
	payload := []byte(`{
  "model": "mimo-v2.6-flash-free",
  "input": [
    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "explore"}]},
    {"type": "function_call", "call_id": "c1", "name": "bash", "arguments": "{\"command\":\"ls\"}"},
    {"type": "function_call", "call_id": "c2", "name": "read", "arguments": "{\"path\":\"main.go\"}"},
    {"type": "function_call_output", "call_id": "c1", "output": "file list"},
    {"type": "function_call_output", "call_id": "c2", "output": "package main"}
  ]
}`)

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := responsesToChat(root); err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	sanitizeChatMessages(root)

	msgs, _ := root["messages"].([]any)
	if len(msgs) != 4 { // user, assistant(2 calls), tool, tool
		t.Fatalf("messages = %d, want 4: %s", len(msgs), mustJSON(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls = %d, want 2 (merged parallel calls): %s", len(calls), mustJSON(msgs))
	}
	for i, want := range []string{"c1", "c2"} {
		call, _ := calls[i].(map[string]any)
		if id, _ := call["id"].(string); id != want {
			t.Errorf("tool_calls[%d].id = %q, want %q", i, id, want)
		}
	}
	for i, want := range []string{"file list", "package main"} {
		tool, _ := msgs[2+i].(map[string]any)
		if role, _ := tool["role"].(string); role != "tool" {
			t.Fatalf("msgs[%d].role = %q, want tool", 2+i, role)
		}
		if c, _ := tool["content"].(string); c != want {
			t.Errorf("msgs[%d].content = %q, want %q", 2+i, c, want)
		}
	}
}

// TestResponsesToChatCustomToolAndObjectOutput covers the custom_tool_call
// variants and function_call_output payloads whose output is an object with
// a content field instead of a plain string.
func TestResponsesToChatCustomToolAndObjectOutput(t *testing.T) {
	payload := []byte(`{
  "model": "mimo-v2.6-flash-free",
  "input": [
    {"type": "custom_tool_call", "call_id": "k1", "name": "bash", "input": "{\"command\":\"ls\"}"},
    {"type": "custom_tool_call_output", "call_id": "k1", "output": {"content": "file list"}}
  ]
}`)

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := responsesToChat(root); err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	sanitizeChatMessages(root)

	msgs, _ := root["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2: %s", len(msgs), mustJSON(msgs))
	}
	callMsg, _ := msgs[0].(map[string]any)
	if role, _ := callMsg["role"].(string); role != "assistant" {
		t.Errorf("call message role = %q, want assistant", role)
	}
	calls, _ := callMsg["tool_calls"].([]any)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if args, _ := fn["arguments"].(string); args != "{\"command\":\"ls\"}" {
		t.Errorf("arguments = %q, want the marshalled input", args)
	}
	toolMsg, _ := msgs[1].(map[string]any)
	if role, _ := toolMsg["role"].(string); role != "tool" {
		t.Errorf("output message role = %q, want tool", role)
	}
	if c, _ := toolMsg["content"].(string); c != "file list" {
		t.Errorf("tool content = %q, want extracted content string", c)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}

// TestResponsesChatStreamConverterMapsToolCalls verifies that function_call
// events in a /responses upstream stream become chat tool_calls deltas —
// without this mapping, agent clients never see muse's tool invocations.
func TestResponsesChatStreamConverterMapsToolCalls(t *testing.T) {
	conv := newResponsesChatStreamConverter("muse-spark-1.3-contributor-free")

	// 1. The function_call item is announced.
	outs, err := conv.convert([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}}`))
	if err != nil {
		t.Fatalf("convert added: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("added: outs = %d, want 1", len(outs))
	}
	head := decodeChunk(t, outs[0])
	calls := toolCallsOf(t, head)
	if calls[0]["id"] != "call_1" || fnNameOf(t, calls[0]) != "bash" {
		t.Errorf("tool call head = id:%v name:%v, want call_1/bash", calls[0]["id"], fnNameOf(t, calls[0]))
	}

	// 2. Argument fragments stream incrementally.
	outs, err = conv.convert([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\""}`))
	if err != nil || len(outs) != 1 {
		t.Fatalf("delta: outs=%d err=%v, want 1", len(outs), err)
	}
	frag := decodeChunk(t, outs[0])
	if got := fnArgsOf(t, toolCallsOf(t, frag)[0]); got != `{"command"` {
		t.Errorf("arguments fragment = %q, want %q", got, `{"command"`)
	}

	// 3. Completion carries finish_reason tool_calls.
	outs, err = conv.convert([]byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`))
	if err != nil || len(outs) != 1 {
		t.Fatalf("completed: outs=%d err=%v, want 1", len(outs), err)
	}
	final := decodeChunk(t, outs[0])
	if got := finishReasonOf(t, final); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
}

// TestResponsesChatStreamConverterToolCallDoneFallback covers upstreams that
// deliver the whole function_call only in output_item.done.
func TestResponsesChatStreamConverterToolCallDoneFallback(t *testing.T) {
	conv := newResponsesChatStreamConverter("muse-spark-1.3-contributor-free")
	outs, err := conv.convert([]byte(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_9","call_id":"call_9","name":"read","arguments":"{\"path\":\"main.go\"}"}}`))
	if err != nil || len(outs) != 1 {
		t.Fatalf("done: outs=%d err=%v, want 1", len(outs), err)
	}
	chunk := decodeChunk(t, outs[0])
	call := toolCallsOf(t, chunk)[0]
	if call["id"] != "call_9" || fnNameOf(t, call) != "read" || fnArgsOf(t, call) != `{"path":"main.go"}` {
		t.Errorf("fallback tool call = %s", mustJSON(call))
	}
}

// TestResponsesCompletionToChatMapsToolCalls verifies the non-streaming fold
// keeps function_call output items as message.tool_calls.
func TestResponsesCompletionToChatMapsToolCalls(t *testing.T) {
	body := []byte(`{
  "id": "resp_1",
  "model": "muse-spark-1.3-contributor-free",
  "status": "completed",
  "output": [
    {"type": "reasoning", "summary": [{"type": "summary_text", "text": "thinking"}]},
    {"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": "{\"command\":\"ls -la\"}"}
  ],
  "usage": {"input_tokens": 12, "output_tokens": 8, "total_tokens": 20}
}`)
	out, err := responsesCompletionToChat(body, "muse-spark-1.3-contributor-free")
	if err != nil {
		t.Fatalf("responsesCompletionToChat: %v", err)
	}
	var completion struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(completion.Choices))
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(choice.Message.ToolCalls))
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "bash" || call.Function.Arguments != `{"command":"ls -la"}` {
		t.Errorf("tool call = %s", mustJSON(call))
	}
	if choice.Message.ReasoningContent != "thinking" {
		t.Errorf("reasoning_content = %q, want thinking", choice.Message.ReasoningContent)
	}
}

// --- helpers -------------------------------------------------------------

func decodeChunk(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("chunk is not valid JSON: %v (%s)", err, string(raw))
	}
	return m
}

func toolCallsOf(t *testing.T, chunk map[string]any) []map[string]any {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("chunk has no choices: %s", mustJSON(chunk))
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	rawCalls, _ := delta["tool_calls"].([]any)
	calls := make([]map[string]any, 0, len(rawCalls))
	for _, c := range rawCalls {
		call, _ := c.(map[string]any)
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		t.Fatalf("chunk has no tool_calls: %s", mustJSON(chunk))
	}
	return calls
}

func fnNameOf(t *testing.T, call map[string]any) string {
	t.Helper()
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

func fnArgsOf(t *testing.T, call map[string]any) string {
	t.Helper()
	fn, _ := call["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	return args
}

func finishReasonOf(t *testing.T, chunk map[string]any) string {
	t.Helper()
	choices, _ := chunk["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	finish, _ := choice["finish_reason"].(string)
	return finish
}

// TestChatToResponsesPreservesToolHistory guards the reverse direction: chat
// clients (LobeHub/NextChat) calling a responses-endpoint model (muse) must
// have assistant tool_calls and tool result messages converted to
// function_call / function_call_output items instead of being dropped.
func TestChatToResponsesPreservesToolHistory(t *testing.T) {
	root := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "user", "content": "list the files"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "bash",
						"arguments": `{"command":"ls -la"}`,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "main.go README.md"},
			map[string]any{"role": "user", "content": "thanks"},
		},
	}

	if err := chatToResponses(root); err != nil {
		t.Fatalf("chatToResponses: %v", err)
	}
	items, _ := root["input"].([]any)
	if len(items) != 4 {
		t.Fatalf("input items = %d, want 4: %s", len(items), mustJSON(items))
	}

	call, _ := items[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "bash" || call["arguments"] != `{"command":"ls -la"}` {
		t.Errorf("function_call item = %s", mustJSON(call))
	}
	out, _ := items[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call_1" || out["output"] != "main.go README.md" {
		t.Errorf("function_call_output item = %s", mustJSON(out))
	}
	user, _ := items[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Errorf("user item = %s", mustJSON(user))
	}
}
