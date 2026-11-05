// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// responsesRequest 是 OpenAI Responses API 的请求体。
// 仅覆盖当前 chat.completions 已支持的常用字段。
type responsesRequest struct {
	Model           string            `json:"model"`
	Input           json.RawMessage   `json:"input"`
	Instructions    string            `json:"instructions,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Tools           []any             `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	// 透传但当前上游可能忽略的其他字段
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Stop             any      `json:"stop,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
}

// responsesInputItem 是 input 数组中的单条记录。
type responsesInputItem struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "parse body: "+err.Error())
		return
	}

	rt, model, err := h.runtimeForModel(req.Model)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}

	chatBody, err := convertResponsesToChat(req)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	chatBody, err = rewriteModel(chatBody, model)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	rc, chatResp, upstreamErr, err := h.doChatRoundTrip(rt, chatBody, req.Stream)
	if err != nil {
		writeResponsesError(w, http.StatusServiceUnavailable, "no_healthy_account", err.Error())
		return
	}
	if upstreamErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamErr.status)
		_, _ = w.Write(upstreamErr.body)
		return
	}

	if req.Stream {
		defer rc.Close()
		responsesStream(w, rc, req.Model)
		return
	}

	resp, err := convertChatCompletionToResponse(chatResp, req.Model)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// convertResponsesToChat 把 Responses 请求转换为 chat.completions 请求体。
func convertResponsesToChat(req responsesRequest) ([]byte, error) {
	messages, err := convertInputToMessages(req.Input, req.Instructions)
	if err != nil {
		return nil, err
	}

	obj := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		obj["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
	if req.ReasoningEffort != "" {
		obj["reasoning_effort"] = req.ReasoningEffort
	}
	if len(req.Tools) > 0 {
		obj["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		obj["tool_choice"] = req.ToolChoice
	}
	if req.PresencePenalty != nil {
		obj["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		obj["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Stop != nil {
		obj["stop"] = req.Stop
	}
	if req.Seed != nil {
		obj["seed"] = *req.Seed
	}
	return json.Marshal(obj)
}

// convertInputToMessages 把 Responses 的 input 字段转换为 chat.completions 的 messages。
func convertInputToMessages(input json.RawMessage, instructions string) ([]map[string]any, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("input is required")
	}

	var messages []map[string]any

	// 先尝试解析为字符串
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		messages = append(messages, map[string]any{"role": "user", "content": s})
	} else {
		// 尝试解析为 input item 数组
		var items []responsesInputItem
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, fmt.Errorf("input must be a string or an array of input items")
		}
		for _, it := range items {
			role := normalizeRole(it.Role)
			messages = append(messages, map[string]any{"role": role, "content": it.Content})
		}
	}

	if instructions != "" {
		messages = append([]map[string]any{{"role": "system", "content": instructions}}, messages...)
	}

	return messages, nil
}

// normalizeRole 把 Responses 的 developer 角色改写为 system（与上游 PrepareBody 保持一致）。
func normalizeRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "developer" {
		return "system"
	}
	return role
}

// convertChatCompletionToResponse 把 chat.completion 聚合结果转换为 Responses 响应。
func convertChatCompletionToResponse(chat map[string]any, originalModel string) (map[string]any, error) {
	id, _ := chat["id"].(string)
	if id == "" {
		id = fmt.Sprintf("resp-%d", time.Now().UnixNano())
	} else {
		id = "resp-" + id
	}
	created, _ := chat["created"].(int64)
	if created == 0 {
		created = time.Now().Unix()
	}

	choices, _ := chat["choices"].([]any)
	var message map[string]any
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ = choice["message"].(map[string]any)
	}
	if message == nil {
		message = map[string]any{"role": "assistant", "content": ""}
	}

	contentParts := []any{}
	if text, ok := message["content"].(string); ok && text != "" {
		contentParts = append(contentParts, map[string]any{
			"type": "output_text",
			"text": text,
		})
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		contentParts = append(contentParts, map[string]any{
			"type":      "reasoning",
			"reasoning": reasoning,
		})
	}
	if len(contentParts) == 0 {
		contentParts = append(contentParts, map[string]any{
			"type": "output_text",
			"text": "",
		})
	}

	outputItem := map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": contentParts,
		"status":  "completed",
	}
	if tcs, ok := message["tool_calls"]; ok {
		outputItem["tool_calls"] = tcs
	}

	resp := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": created,
		"model":      originalModel,
		"status":     "completed",
		"output": []any{outputItem},
	}

	if usage, ok := chat["usage"].(map[string]any); ok {
		resp["usage"] = usage
	}

	return resp, nil
}

// responsesStream 把 chat.completions SSE 转换为 Responses API stream events。
func responsesStream(w http.ResponseWriter, r io.ReadCloser, originalModel string) {
	defer r.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	responseID := fmt.Sprintf("resp-%d", time.Now().UnixNano())
	createdAt := time.Now().Unix()
	itemID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	contentIndex := 0
	started := false

	writeEvent := func(event string, data map[string]any) error {
		raw, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					// 收尾事件统一在循环之外写一次，这里只结束读取。
					break
				}

				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if !started {
						started = true
						if err := writeEvent("response.created", map[string]any{
							"type":       "response.created",
							"response":   responseStub(responseID, createdAt, originalModel),
							"created_at": createdAt,
						}); err != nil {
							return
						}
						if err := writeEvent("response.output_item.added", map[string]any{
							"type":          "response.output_item.added",
							"output_index":  0,
							"item":          map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress"},
							"created_at":    createdAt,
						}); err != nil {
							return
						}
						if err := writeEvent("response.content_part.added", map[string]any{
							"type":           "response.content_part.added",
							"item_id":        itemID,
							"output_index":   0,
							"content_index":  contentIndex,
							"part":           map[string]any{"type": "output_text"},
							"created_at":     createdAt,
						}); err != nil {
							return
						}
					}

					choices, _ := chunk["choices"].([]any)
					for _, ci := range choices {
						choice, _ := ci.(map[string]any)
						if choice == nil {
							continue
						}
						delta, _ := choice["delta"].(map[string]any)
						if delta == nil {
							continue
						}
						if text, ok := delta["content"].(string); ok && text != "" {
							if err := writeEvent("response.output_text.delta", map[string]any{
								"type":          "response.output_text.delta",
								"item_id":       itemID,
								"output_index":  0,
								"content_index": contentIndex,
								"delta":         text,
								"created_at":    createdAt,
							}); err != nil {
								return
							}
						}
						if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
							if err := writeEvent("response.reasoning.delta", map[string]any{
								"type":          "response.reasoning.delta",
								"item_id":       itemID,
								"output_index":  0,
								"content_index": contentIndex + 1,
								"delta":         reasoning,
								"created_at":    createdAt,
							}); err != nil {
								return
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("responsesStream error: %v", err)
			return
		}
	}

	if started {
		_ = writeEvent("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       itemID,
			"output_index":  0,
			"content_index": contentIndex,
			"created_at":    createdAt,
		})
		_ = writeEvent("response.output_item.done", map[string]any{
			"type":          "response.output_item.done",
			"item_id":       itemID,
			"output_index":  0,
			"created_at":    createdAt,
		})
	}
	_ = writeEvent("response.completed", map[string]any{
		"type":       "response.completed",
		"response":   responseStub(responseID, createdAt, originalModel),
		"created_at": createdAt,
	})
}

// responseStub 返回一个简化的 response 对象骨架，用于 stream events。
func responseStub(id string, createdAt int64, model string) map[string]any {
	return map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": createdAt,
		"model":      model,
		"status":     "completed",
		"output":     []any{},
	}
}

// writeResponsesError 返回 OpenAI 风格的错误响应。
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

// upstreamErr 是内层 chat 端点返回的**错误响应**（status>=400）。
// 调用方按原状态码、原响应体透传，不做任何包装（不变量 8）。
type upstreamErr struct {
	status int
	body   []byte
}

// doChatRoundTrip 在进程内把一次对话请求交给 /v1/chat/completions 处理。
//
// 为什么不在本文件里直接调 rt.Upstream：轮转选号、账号级惩罚、模型降级、
// 粘性路由、token/积分流水与运行统计插桩全都长在 chatCompletions/serveRotation
// 里。在这里重写一套等于造出第二条"半套语义"的链路——它不报错、只是悄悄不遵守
// R29/R54/R147 等既定规则。故这里用 io.Pipe 把内层 handler 当作 http.Handler
// 在进程内调用，语义与真实 /v1/chat/completions 请求逐位一致。
//
// 返回：
//   - stream=true：rc 非空且可流式读取（调用方负责 Close）；chatResp/upErr 为 nil。
//   - stream=false：读完整响应；成功时 chatResp 为解析后的 chat.completion，
//     失败时 upErr 非空（status/body 为内层原样输出）。
//   - err 仅表示"内层调用本身无法完成"（构造请求失败/响应体不是 JSON）。
//
// 注意：gateway 包里有同样结构的 pipeRW，但 import 方向是 gateway → server，
// 反向引用会成环，故管道样板在小范围内各留一份，语义必须保持一致
// （Flush 空操作、状态码 Once 回传、未写内容兜底 200）。
func (h *Handler) doChatRoundTrip(rt *Runtime, body []byte, stream bool) (io.ReadCloser, map[string]any, *upstreamErr, error) {
	pr, pw := io.Pipe()
	p := newResponsePipe(pw)

	req, err := http.NewRequest(http.MethodPost, "http://wild-work/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	req.RemoteAddr = "127.0.0.1:0"
	if key := h.CurrentAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	go func() {
		// 兜底：内层未写任何内容就返回时补 200，避免调用方永远阻塞在状态码握手。
		defer func() {
			p.setStatus(http.StatusOK)
			_ = pw.Close()
		}()
		h.ServeHTTP(p, req)
	}()

	status := <-p.statusCh
	if status >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(pr, MaxRequestBody))
		_ = pr.Close()
		return nil, nil, &upstreamErr{status: status, body: raw}, nil
	}
	if stream {
		return pr, nil, nil, nil
	}

	raw, rerr := io.ReadAll(io.LimitReader(pr, MaxRequestBody))
	_ = pr.Close()
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	var chatResp map[string]any
	if jerr := json.Unmarshal(raw, &chatResp); jerr != nil {
		return nil, nil, nil, fmt.Errorf("parse chat response: %w", jerr)
	}
	return nil, chatResp, nil, nil
}

// responsePipe 是把内层 handler 的响应写进 io.Pipe 的 ResponseWriter。
// 与 gateway.pipeRW 同构（见 doChatRoundTrip 的说明）。
type responsePipe struct {
	hdr      http.Header
	pw       *io.PipeWriter
	statusCh chan int
	once     sync.Once
}

func newResponsePipe(pw *io.PipeWriter) *responsePipe {
	return &responsePipe{hdr: http.Header{}, pw: pw, statusCh: make(chan int, 1)}
}

func (p *responsePipe) Header() http.Header { return p.hdr }

func (p *responsePipe) setStatus(code int) {
	p.once.Do(func() { p.statusCh <- code })
}

func (p *responsePipe) WriteHeader(code int) { p.setStatus(code) }

func (p *responsePipe) Write(b []byte) (int, error) {
	p.setStatus(http.StatusOK)
	return p.pw.Write(b)
}

// Flush 空实现：io.Pipe 每次 Write 即为一次同步交付。
func (p *responsePipe) Flush() {}
