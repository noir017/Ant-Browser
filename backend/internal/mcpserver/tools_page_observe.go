package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 页面观测工具：把「页面发了什么请求、报了什么错」变成一次工具调用。
//
// 采集在常驻会话的 Node 侧完成（见 assets/runner_page_observe.cjs），
// 这里只负责翻译参数、解码结果，与其它 ant_page_* 工具走同一条路径。

func registerPageObserveTools(srv *mcp.Server, p PageProvider) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ant_page_network",
		Description: "列出页面最近的网络请求（方法、URL、状态码、耗时、类型）。" +
			"可按 type、url 关键字、status 过滤；传 id 返回单条详情，含打码的请求头和响应体预览。",
		Annotations: readOnly("网络请求"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in networkInput) (*mcp.CallToolResult, networkOutput, error) {
		args := map[string]any{}
		if in.ID != nil {
			args["id"] = *in.ID
		}
		if resourceType := strings.TrimSpace(in.Type); resourceType != "" {
			args["type"] = resourceType
		}
		if keyword := strings.TrimSpace(in.URL); keyword != "" {
			args["url"] = keyword
		}
		if status := strings.TrimSpace(in.Status); status != "" {
			args["status"] = status
		}
		if in.Limit > 0 {
			args["limit"] = in.Limit
		}

		result, err := pageCall(p, in.Selector, in.TimeoutMs, "network", args)
		if err != nil {
			return toolError(err), networkOutput{}, nil
		}
		payload, stepErr := firstStep(result)
		if stepErr != nil {
			return toolError(stepErr), networkOutput{pageOutput: pageOutput{ProfileID: result.ProfileID}}, nil
		}

		out := networkOutput{
			pageOutput: pageOutput{ProfileID: result.ProfileID, OK: true, Page: pageStateOf(payload)},
			Total:      observeInt(payload, "total"),
			Matched:    observeInt(payload, "matched"),
			Returned:   observeInt(payload, "returned"),
		}
		if items, ok := payload["items"].([]any); ok {
			out.Requests = decodeNetworkRequests(items)
			out.Returned = len(out.Requests)
		}
		if raw, ok := payload["detail"].(map[string]any); ok {
			var detail NetworkRequestDetail
			if decodeObserved(raw, &detail) == nil {
				out.Detail = &detail
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ant_page_console",
		Description: "列出页面最近的控制台消息和未捕获异常，可按 level 过滤。",
		Annotations: readOnly("控制台消息"),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in consoleInput) (*mcp.CallToolResult, consoleOutput, error) {
		args := map[string]any{}
		if level := strings.TrimSpace(in.Level); level != "" {
			args["level"] = level
		}
		if in.Limit > 0 {
			args["limit"] = in.Limit
		}

		result, err := pageCall(p, in.Selector, in.TimeoutMs, "console", args)
		if err != nil {
			return toolError(err), consoleOutput{}, nil
		}
		payload, stepErr := firstStep(result)
		if stepErr != nil {
			return toolError(stepErr), consoleOutput{pageOutput: pageOutput{ProfileID: result.ProfileID}}, nil
		}

		out := consoleOutput{
			pageOutput: pageOutput{ProfileID: result.ProfileID, OK: true, Page: pageStateOf(payload)},
			Total:      observeInt(payload, "total"),
			Matched:    observeInt(payload, "matched"),
			Returned:   observeInt(payload, "returned"),
		}
		if items, ok := payload["items"].([]any); ok {
			out.Messages = decodeConsoleMessages(items)
			out.Returned = len(out.Messages)
		}
		return nil, out, nil
	})
}

// ---------------------------------------------------------------------------
// 入参 / 出参
// ---------------------------------------------------------------------------

type networkInput struct {
	targetInput
	ID     *int   `json:"id,omitempty" jsonschema:"请求编号（见列表的 id）；给出时只返回该条详情，忽略其它过滤条件"`
	Type   string `json:"type,omitempty" jsonschema:"资源类型：xhr / fetch / document / script / image / stylesheet 等，逗号分隔多个"`
	URL    string `json:"url,omitempty" jsonschema:"URL 关键字，不区分大小写"`
	Status string `json:"status,omitempty" jsonschema:"状态码过滤：404、400-499、5xx，或 >= 400 这类下限"`
	Limit  int    `json:"limit,omitempty" jsonschema:"返回条数上限，默认 20，最多 100"`
}

// NetworkRequest 是请求列表里的一条。
type NetworkRequest struct {
	ID           int    `json:"id"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	Status       int    `json:"status"`
	ResourceType string `json:"resourceType,omitempty"`
	DurationMs   int    `json:"durationMs,omitempty"`
	Pending      bool   `json:"pending,omitempty" jsonschema:"请求尚未结束，耗时和状态码还不可信"`
	Failed       string `json:"failed,omitempty"`
	PageURL      string `json:"pageUrl,omitempty"`
}

// NetworkRequestDetail 是单条请求的详情，头和体都已打码。
type NetworkRequestDetail struct {
	ID              int               `json:"id"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Status          int               `json:"status"`
	StatusText      string            `json:"statusText,omitempty"`
	ResourceType    string            `json:"resourceType,omitempty"`
	DurationMs      int               `json:"durationMs,omitempty"`
	Pending         bool              `json:"pending,omitempty"`
	Failed          string            `json:"failed,omitempty"`
	PageURL         string            `json:"pageUrl,omitempty"`
	StartedAt       string            `json:"startedAt,omitempty"`
	RequestHeaders  map[string]string `json:"requestHeaders,omitempty"`
	RequestBody     string            `json:"requestBody,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResponseType    string            `json:"responseType,omitempty"`
	ResponseBody    string            `json:"responseBody,omitempty"`
}

type networkOutput struct {
	pageOutput
	Total    int                   `json:"total" jsonschema:"缓冲区里的请求总数"`
	Matched  int                   `json:"matched,omitempty" jsonschema:"命中过滤条件的条数"`
	Returned int                   `json:"returned,omitempty"`
	Requests []NetworkRequest      `json:"requests,omitempty"`
	Detail   *NetworkRequestDetail `json:"detail,omitempty" jsonschema:"按 id 查询时返回的单条详情"`
}

type consoleInput struct {
	targetInput
	Level string `json:"level,omitempty" jsonschema:"级别：error / warning / log / info / debug，逗号分隔多个；error 含页面未捕获异常"`
	Limit int    `json:"limit,omitempty" jsonschema:"返回条数上限，默认 20，最多 100"`
}

// ConsoleMessage 是一条控制台消息或页面异常。
type ConsoleMessage struct {
	ID      int    `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Source  string `json:"source,omitempty" jsonschema:"消息来源 url:line:column"`
	PageURL string `json:"pageUrl,omitempty"`
}

type consoleOutput struct {
	pageOutput
	Total    int              `json:"total" jsonschema:"缓冲区里的消息总数"`
	Matched  int              `json:"matched,omitempty" jsonschema:"命中过滤条件的条数"`
	Returned int              `json:"returned,omitempty"`
	Messages []ConsoleMessage `json:"messages,omitempty"`
}

// ---------------------------------------------------------------------------
// 解码辅助
// ---------------------------------------------------------------------------

func observeInt(payload map[string]any, key string) int {
	if value, ok := payload[key].(float64); ok {
		return int(value)
	}
	return 0
}

func decodeObserved(raw map[string]any, target any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func decodeNetworkRequests(items []any) []NetworkRequest {
	out := make([]NetworkRequest, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var request NetworkRequest
		if err := json.Unmarshal(data, &request); err != nil {
			continue
		}
		out = append(out, request)
	}
	return out
}

func decodeConsoleMessages(items []any) []ConsoleMessage {
	out := make([]ConsoleMessage, 0, len(items))
	for _, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var message ConsoleMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		out = append(out, message)
	}
	return out
}
