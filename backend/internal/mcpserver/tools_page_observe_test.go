package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"ant-chrome/backend/internal/launchcode"
)

// 观测工具的契约测试：入参 → 指令 args，以及 Node 回传的 map → 结构化输出。
// 采集本身由 assets/runner_page_observe.cjs 负责，这里只锁翻译层。

func TestPageNetworkTranslatesFilters(t *testing.T) {
	provider := &fakeProvider{}
	result := callPageTool(t, provider, "ant_page_network", map[string]any{
		"type":   "xhr,fetch",
		"url":    "/api/register",
		"status": ">=400",
		"limit":  float64(5),
	})
	if result.IsError {
		t.Fatalf("network failed: %v", result.Content)
	}

	steps := provider.lastPageRequest.Steps
	if len(steps) != 1 || steps[0].Action != "network" {
		t.Fatalf("steps = %+v, want one network step", steps)
	}
	args := steps[0].Args
	if args["type"] != "xhr,fetch" || args["url"] != "/api/register" || args["status"] != ">=400" {
		t.Errorf("args = %v", args)
	}
	if args["limit"] != 5 {
		t.Errorf("limit = %v, want 5", args["limit"])
	}
	if _, ok := args["id"]; ok {
		t.Errorf("id should be absent without it, got %v", args["id"])
	}
}

// 空过滤条件不能下发空串，否则 Node 侧会把它们当成过滤值。
func TestPageNetworkOmitsEmptyFilters(t *testing.T) {
	provider := &fakeProvider{}
	if result := callPageTool(t, provider, "ant_page_network", map[string]any{}); result.IsError {
		t.Fatalf("network failed: %v", result.Content)
	}

	args := provider.lastPageRequest.Steps[0].Args
	for _, key := range []string{"id", "type", "url", "status", "limit"} {
		if value, ok := args[key]; ok {
			t.Errorf("args[%q] = %v, want it absent", key, value)
		}
	}
}

func TestPageNetworkDecodesList(t *testing.T) {
	provider := &fakeProvider{
		pageResult: &launchcode.PageResult{
			ProfileID: "p1",
			OK:        true,
			Steps: []launchcode.PageStepOutcome{{
				Action: "network",
				OK:     true,
				Result: map[string]any{
					"total":    float64(12),
					"matched":  float64(2),
					"returned": float64(2),
					"items": []any{
						map[string]any{
							"id": float64(7), "method": "POST", "url": "https://a.test/api/register",
							"status": float64(500), "resourceType": "fetch", "durationMs": float64(132),
							"pageUrl": "https://a.test/",
						},
						map[string]any{
							"id": float64(9), "method": "GET", "url": "https://a.test/api/captcha",
							"status": float64(0), "resourceType": "xhr", "failed": "net::ERR_ABORTED",
						},
					},
				},
			}},
		},
	}

	result := callPageTool(t, provider, "ant_page_network", map[string]any{})
	if result.IsError {
		t.Fatalf("network failed: %v", result.Content)
	}

	var out networkOutput
	decodeStructured(t, result, &out)
	if out.Total != 12 || out.Matched != 2 || out.Returned != 2 {
		t.Errorf("counts = %+v", out)
	}
	if len(out.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(out.Requests))
	}
	first := out.Requests[0]
	if first.ID != 7 || first.Method != "POST" || first.Status != 500 || first.ResourceType != "fetch" {
		t.Errorf("first request = %+v", first)
	}
	if out.Requests[1].Failed != "net::ERR_ABORTED" {
		t.Errorf("failed = %q", out.Requests[1].Failed)
	}
}

func TestPageNetworkDecodesDetail(t *testing.T) {
	provider := &fakeProvider{
		pageResult: &launchcode.PageResult{
			ProfileID: "p1",
			OK:        true,
			Steps: []launchcode.PageStepOutcome{{
				Action: "network",
				OK:     true,
				Result: map[string]any{
					"detail": map[string]any{
						"id":         float64(3),
						"method":     "POST",
						"url":        "https://a.test/api/register",
						"status":     float64(500),
						"statusText": "Internal Server Error",
						"durationMs": float64(88),
						"requestHeaders": map[string]any{
							"authorization": "***",
							"content-type":  "application/json",
						},
						"requestBody":  `{"password":"***","user":"alice"}`,
						"responseBody": `{"msg":"captcha required"}`,
						"responseType": "application/json",
						"startedAt":    "2026-11-20T10:00:00.000Z",
					},
				},
			}},
		},
	}

	result := callPageTool(t, provider, "ant_page_network", map[string]any{"id": float64(3)})
	if result.IsError {
		t.Fatalf("network detail failed: %v", result.Content)
	}
	if got := provider.lastPageRequest.Steps[0].Args["id"]; got != 3 {
		t.Errorf("sent id = %v, want 3", got)
	}

	var out networkOutput
	decodeStructured(t, result, &out)
	if out.Detail == nil {
		t.Fatal("detail is nil")
	}
	if out.Detail.ID != 3 || out.Detail.Status != 500 {
		t.Errorf("detail = %+v", out.Detail)
	}
	if out.Detail.RequestHeaders["authorization"] != "***" {
		t.Errorf("authorization header not masked: %v", out.Detail.RequestHeaders)
	}
	if !strings.Contains(out.Detail.RequestBody, `"password":"***"`) {
		t.Errorf("request body = %q, want the password masked", out.Detail.RequestBody)
	}
	// 详情没有计数语义，返回 total: 0 会让人以为缓冲区是空的。
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if strings.Contains(string(raw), `"total"`) {
		t.Errorf("detail output = %s, want no total field", raw)
	}
}

func TestPageConsoleTranslatesLevel(t *testing.T) {
	provider := &fakeProvider{}
	result := callPageTool(t, provider, "ant_page_console", map[string]any{
		"level": "error,warning",
		"limit": float64(10),
	})
	if result.IsError {
		t.Fatalf("console failed: %v", result.Content)
	}

	steps := provider.lastPageRequest.Steps
	if len(steps) != 1 || steps[0].Action != "console" {
		t.Fatalf("steps = %+v, want one console step", steps)
	}
	if steps[0].Args["level"] != "error,warning" || steps[0].Args["limit"] != 10 {
		t.Errorf("args = %v", steps[0].Args)
	}
}

func TestPageConsoleDecodesMessages(t *testing.T) {
	provider := &fakeProvider{
		pageResult: &launchcode.PageResult{
			ProfileID: "p1",
			OK:        true,
			Steps: []launchcode.PageStepOutcome{{
				Action: "console",
				OK:     true,
				Result: map[string]any{
					"total":    float64(4),
					"matched":  float64(2),
					"returned": float64(2),
					"items": []any{
						map[string]any{
							"id": float64(2), "type": "error", "text": "register failed",
							"source": "https://a.test/app.js:10:3", "pageUrl": "https://a.test/",
						},
						map[string]any{"id": float64(4), "type": "pageerror", "text": "boom"},
					},
				},
			}},
		},
	}

	result := callPageTool(t, provider, "ant_page_console", map[string]any{})
	if result.IsError {
		t.Fatalf("console failed: %v", result.Content)
	}

	var out consoleOutput
	decodeStructured(t, result, &out)
	if out.Total != 4 || out.Returned != 2 || len(out.Messages) != 2 {
		t.Fatalf("output = %+v", out)
	}
	if out.Messages[0].Type != "error" || out.Messages[0].Source != "https://a.test/app.js:10:3" {
		t.Errorf("first message = %+v", out.Messages[0])
	}
	if out.Messages[1].Type != "pageerror" {
		t.Errorf("second message = %+v", out.Messages[1])
	}
}
