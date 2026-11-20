package mcpserver

import (
	"strings"
	"testing"
	"unicode/utf8"

	"ant-chrome/backend/internal/launchcode"
)

// 页面错误归一化的契约测试。
//
// 样例都来自真实 Playwright 输出（含 Call log 与 ANSI 颜色），确保归类
// 依赖的字符串不会因为版本升级悄悄失效。

func TestPageFailureClassification(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCode  string
		wantInMsg string
		wantHint  string
		retryable bool
	}{
		{
			name:      "locator timeout",
			raw:       "locator.click: Timeout 30000ms exceeded.\nCall log:\n  - waiting for locator('main > div > div:nth-of-type(2) > section > form > button').first()",
			wantCode:  "locator_timeout",
			wantInMsg: "locator.click: Timeout 30000ms exceeded.",
			wantHint:  "ant_page_snapshot",
			retryable: true,
		},
		{
			name:      "element obstructed",
			raw:       "locator.click: Timeout 1500ms exceeded.\nCall log:\n\x1b[2m  - waiting for locator('#target')\x1b[22m\n\x1b[2m      - <div id=\"cover\"></div> intercepts pointer events\x1b[22m\n\x1b[2m    - retrying click action\x1b[22m",
			wantCode:  "element_obstructed",
			wantInMsg: "locator.click: Timeout 1500ms exceeded.",
			wantHint:  "遮挡",
			retryable: false,
		},
		{
			name:      "element detached",
			raw:       "locator.click: Timeout 1500ms exceeded.\nCall log:\n\x1b[2m    12 × locator resolved to <button id=\"btn\">Churn</button>\x1b[22m\n\x1b[2m     - element was detached from the DOM, retrying\x1b[22m",
			wantCode:  "element_detached",
			wantInMsg: "locator.click: Timeout 1500ms exceeded.",
			wantHint:  "重新 snapshot",
			retryable: false,
		},
		{
			name:      "navigation interrupted",
			raw:       "page.goto: net::ERR_ABORTED at http://127.0.0.1:38917/slow\nCall log:\n\x1b[2m  - navigating to \"http://127.0.0.1:38917/slow\", waiting until \"load\"\x1b[22m",
			wantCode:  "navigation_interrupted",
			wantInMsg: "page.goto: net::ERR_ABORTED",
			wantHint:  "重新 goto",
			retryable: true,
		},
		{
			name:      "navigation failed",
			raw:       "page.goto: net::ERR_NAME_NOT_RESOLVED at http://no-such-host.invalid/\nCall log:\n\x1b[2m  - navigating to \"http://no-such-host.invalid/\", waiting until \"load\"\x1b[22m",
			wantCode:  "navigation_failed",
			wantInMsg: "net::ERR_NAME_NOT_RESOLVED",
			wantHint:  "网络连通性",
			retryable: true,
		},
		{
			name:      "session timeout",
			raw:       "页面会话响应超时（上限 40s）",
			wantCode:  "session_timeout",
			wantInMsg: "页面会话响应超时",
			wantHint:  "ant_page_release",
			retryable: true,
		},
		{
			name:      "session lost",
			raw:       "页面会话已结束：browser disconnected",
			wantCode:  "session_lost",
			wantInMsg: "browser disconnected",
			wantHint:  "重建会话",
			retryable: true,
		},
		{
			name:      "stale ref",
			raw:       "ref not found: e12",
			wantCode:  "ref_stale",
			wantInMsg: "ref not found: e12",
			wantHint:  "ant_page_snapshot",
			retryable: false,
		},
		{
			name:      "js stack is dropped",
			raw:       "page.evaluate: Error: boom\n    at eval (eval at evaluate (:370:30), <anonymous>:1:15)\n    at UtilityScript.evaluate (<anonymous>:372:16)",
			wantCode:  "page_error",
			wantInMsg: "page.evaluate: Error: boom",
			wantHint:  "ant_page_snapshot",
			retryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := newPageFailure(tt.raw)

			if failure.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", failure.Code, tt.wantCode)
			}
			if !strings.Contains(failure.Message, tt.wantInMsg) {
				t.Errorf("Message = %q, want it to contain %q", failure.Message, tt.wantInMsg)
			}
			if strings.Contains(failure.Message, "Call log") {
				t.Errorf("Message = %q, should not contain Call log", failure.Message)
			}
			if strings.Contains(failure.Message, "\x1b") {
				t.Errorf("Message = %q, should not contain ANSI escapes", failure.Message)
			}
			if strings.Contains(failure.Message, "\n") {
				t.Errorf("Message = %q, should be a single line", failure.Message)
			}
			if !strings.Contains(failure.Hint, tt.wantHint) {
				t.Errorf("Hint = %q, want it to contain %q", failure.Hint, tt.wantHint)
			}
			if failure.Retryable != tt.retryable {
				t.Errorf("Retryable = %v, want %v", failure.Retryable, tt.retryable)
			}
			if got := utf8.RuneCountInString(failure.Message); got > pageMessageLimit {
				t.Errorf("Message length = %d, want <= %d", got, pageMessageLimit)
			}
		})
	}
}

// TestPageFailureTruncatesUnknownErrors 确认认不出来的错误保留原文但截断。
func TestPageFailureTruncatesUnknownErrors(t *testing.T) {
	raw := strings.Repeat("未知错误", 200)
	failure := newPageFailure(raw)

	if failure.Code != "page_error" {
		t.Errorf("Code = %q, want page_error", failure.Code)
	}
	if got := utf8.RuneCountInString(failure.Message); got != pageMessageLimit {
		t.Errorf("Message length = %d, want %d", got, pageMessageLimit)
	}
	if !strings.HasSuffix(failure.Message, "…") {
		t.Errorf("Message = %q, want ellipsis suffix", failure.Message)
	}
}

// TestPageFailureTextLayout 确认工具错误正文是约定的三行格式。
func TestPageFailureTextLayout(t *testing.T) {
	text := newPageFailure("ref not found: e3").text()
	lines := strings.Split(text, "\n")
	if len(lines) != 3 {
		t.Fatalf("text has %d lines, want 3: %q", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "ref_stale: ") {
		t.Errorf("line 1 = %q, want code prefix", lines[0])
	}
	if !strings.HasPrefix(lines[1], "hint: ") {
		t.Errorf("line 2 = %q, want hint prefix", lines[1])
	}
	if lines[2] != "retryable: false" {
		t.Errorf("line 3 = %q, want retryable flag", lines[2])
	}
}

// TestPageToolFailureIsNormalized 验证失败步骤经工具层输出归一化文案。
func TestPageToolFailureIsNormalized(t *testing.T) {
	provider := &fakeProvider{pageResult: &launchcode.PageResult{
		ProfileID: "p1",
		Steps: []launchcode.PageStepOutcome{{
			Action: "click",
			OK:     false,
			Error:  "locator.click: Timeout 30000ms exceeded.\nCall log:\n  - waiting for locator('#submit').first()",
		}},
	}}

	result := callPageTool(t, provider, "ant_page_click", map[string]any{"element": "#submit"})
	if !result.IsError {
		t.Fatal("IsError = false, want true")
	}

	text := contentText(result)
	for _, want := range []string{"locator_timeout: locator.click: Timeout 30000ms exceeded.", "hint: ", "retryable: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool error = %q, want it to contain %q", text, want)
		}
	}
	if strings.Contains(text, "Call log") {
		t.Errorf("tool error = %q, should not contain Call log", text)
	}
}

// TestPageSessionErrorIsNormalized 验证会话级错误（从 provider 直接返回的
// error）也会归一化，而不是只处理步骤内的失败。
func TestPageSessionErrorIsNormalized(t *testing.T) {
	provider := &fakeProvider{failWith: &launchcode.ServiceError{
		Status:  500,
		Message: "页面会话响应超时（上限 40s）",
	}}

	result := callPageTool(t, provider, "ant_page_goto", map[string]any{"url": "https://example.com/"})
	if !result.IsError {
		t.Fatal("IsError = false, want true")
	}

	text := contentText(result)
	if !strings.HasPrefix(text, "session_timeout: ") {
		t.Errorf("tool error = %q, want session_timeout prefix", text)
	}
}

// TestPageTargetErrorKeepsActionableHint 确认定位实例的 404 不被页面错误
// 归一化吞掉，仍给出原有的实例排查建议。
func TestPageTargetErrorKeepsActionableHint(t *testing.T) {
	provider := &fakeProvider{failWith: &launchcode.ServiceError{
		Status:  404,
		Message: "profile not found",
	}}

	result := callPageTool(t, provider, "ant_page_goto", map[string]any{"url": "https://example.com/"})
	if !result.IsError {
		t.Fatal("IsError = false, want true")
	}
	if text := contentText(result); !strings.Contains(text, "ant_instance_list") {
		t.Errorf("tool error = %q, want instance hint preserved", text)
	}
}
