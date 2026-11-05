package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertResponsesToChatSimple(t *testing.T) {
	req := responsesRequest{
		Model: "workbuddy/auto",
		Input: json.RawMessage(`"hello"`),
	}
	body, err := convertResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "workbuddy/auto" {
		t.Errorf("model=%v", m["model"])
	}
	if m["stream"] != false {
		t.Errorf("stream=%v", m["stream"])
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages=%v", m["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "hello" {
		t.Errorf("msg=%v", msg)
	}
}

func TestConvertResponsesToChatWithInstructions(t *testing.T) {
	req := responsesRequest{
		Model:        "workbuddy/auto",
		Input:        json.RawMessage(`[{"role":"user","content":"hi"}]`),
		Instructions: "Be helpful",
	}
	body, err := convertResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "Be helpful" {
		t.Errorf("first msg=%v", msgs[0])
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("second msg=%v", msgs[1])
	}
}

func TestConvertResponsesToChatDeveloperRole(t *testing.T) {
	req := responsesRequest{
		Model: "workbuddy/auto",
		Input: json.RawMessage(`[{"role":"developer","content":"sys"}]`),
	}
	body, err := convertResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	msgs, _ := m["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("developer should become system, got %v", msgs[0])
	}
}

func TestConvertResponsesToChatWithTools(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}
	req := responsesRequest{
		Model:      "workbuddy/auto",
		Input:      json.RawMessage(`"weather in beijing"`),
		Tools:      tools,
		ToolChoice: "auto",
	}
	body, err := convertResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	if _, ok := m["tools"]; !ok {
		t.Error("tools missing")
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
}

func TestConvertResponsesToChatReasoningAndParams(t *testing.T) {
	temp := 0.5
	topP := 0.9
	req := responsesRequest{
		Model:           "workbuddy/auto",
		Input:           json.RawMessage(`"hello"`),
		MaxOutputTokens: 1024,
		Temperature:     &temp,
		TopP:            &topP,
		ReasoningEffort: "medium",
	}
	body, err := convertResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	if m["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens=%v", m["max_tokens"])
	}
	if m["temperature"] != 0.5 {
		t.Errorf("temperature=%v", m["temperature"])
	}
	if m["top_p"] != 0.9 {
		t.Errorf("top_p=%v", m["top_p"])
	}
	if m["reasoning_effort"] != "medium" {
		t.Errorf("reasoning_effort=%v", m["reasoning_effort"])
	}
}

func TestConvertChatCompletionToResponse(t *testing.T) {
	chat := map[string]any{
		"id":      "chatcmpl-abc",
		"object":  "chat.completion",
		"created": int64(1753600000),
		"model":   "auto",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "你好",
					"reasoning_content": "思考",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
	}
	resp, err := convertChatCompletionToResponse(chat, "workbuddy/auto")
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "workbuddy/auto" {
		t.Errorf("model=%v", resp["model"])
	}
	if !strings.HasPrefix(resp["id"].(string), "resp-") {
		t.Errorf("id=%v", resp["id"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len=%d", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("item=%v", item)
	}
	content, _ := item["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len=%d", len(content))
	}
	if content[0].(map[string]any)["type"] != "output_text" || content[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content[0]=%v", content[0])
	}
	if content[1].(map[string]any)["type"] != "reasoning" || content[1].(map[string]any)["reasoning"] != "思考" {
		t.Errorf("content[1]=%v", content[1])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["total_tokens"] != 7 {
		t.Errorf("usage=%v", usage)
	}
}

func TestResponsesStreamEventConversion(t *testing.T) {
	chatSSE := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"auto\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"世界\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	responsesStream(rec, &mockReadCloser{Reader: strings.NewReader(chatSSE)}, "workbuddy/auto")

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created") {
		t.Errorf("missing response.created: %s", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta") {
		t.Errorf("missing response.output_text.delta: %s", body)
	}
	if !strings.Contains(body, `"u4f60好"`) && !strings.Contains(body, "你好") {
		t.Errorf("missing content delta: %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Errorf("missing response.completed: %s", body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

func TestResponsesStreamReasoning(t *testing.T) {
	chatSSE := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	responsesStream(rec, &mockReadCloser{Reader: strings.NewReader(chatSSE)}, "workbuddy/auto")
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.reasoning.delta") {
		t.Errorf("missing reasoning delta: %s", body)
	}
}

// mockReadCloser 把 strings.Reader 包装成 io.ReadCloser。
type mockReadCloser struct {
	*strings.Reader
}

func (m *mockReadCloser) Close() error { return nil }
