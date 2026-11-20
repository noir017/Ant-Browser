package mcpserver

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"ant-chrome/backend/internal/launchcode"
)

// 页面工具的错误归一化。
//
// Playwright 的原始报错动辄几十行：Call log 是每次重试的细节，JS 异常还带
// 调用栈。这些内容对模型没有价值，只会挤占上下文。这里把一次失败压缩成
// code / message / hint / retryable 四项：message 保留能辨认出问题的原文并
// 限长，hint 直接给出下一步，retryable 说明同样的调用是否值得再试一次。
//
// 归一化放在 Go 侧，因为错误来源不止 Playwright——会话超时、元素编号过期
// 都在 Go 才看得到。

// pageMessageLimit 是回给模型的错误原文上限（字符数）。
const pageMessageLimit = 240

// pageFailure 是页面工具失败的结构化描述，实现 error 以便复用 toolError。
type pageFailure struct {
	Code      string
	Message   string
	Hint      string
	Retryable bool
}

func (f *pageFailure) Error() string {
	return f.Code + ": " + f.Message
}

// text 是工具错误的正文：三行，模型扫一眼就知道错在哪、下一步做什么。
func (f *pageFailure) text() string {
	retryable := "false"
	if f.Retryable {
		retryable = "true"
	}
	return f.Code + ": " + f.Message + "\nhint: " + f.Hint + "\nretryable: " + retryable
}

var (
	// JS 调用栈从一行以 at 开头的缩进行开始，后面的内容对模型没用。
	pageStackPattern = regexp.MustCompile(`(?m)^\s+at\s`)
	// Playwright 给 Call log 加了 ANSI 颜色；即使被裁掉，原文里也可能残留。
	pageANSIPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// newPageFailure 把原始错误文本归类并压缩。
func newPageFailure(raw string) *pageFailure {
	message := clipPageMessage(raw)
	if message == "" {
		message = "页面操作失败"
	}
	code := classifyPageError(raw)
	return &pageFailure{
		Code:      code,
		Message:   message,
		Hint:      pageFailureHints[code],
		Retryable: pageFailureRetryable[code],
	}
}

// asPageFailure 把页面执行期的服务错误转成归一化错误。
//
// 400/404/409/503 是调用方自己能纠正的问题（参数、目标实例、能力未注入），
// 保持原样，继续由 describeError 给出针对性提示；其余按页面失败处理。
func asPageFailure(err error) error {
	if err == nil {
		return nil
	}
	var svcErr *launchcode.ServiceError
	if errors.As(err, &svcErr) && svcErr.Status != http.StatusInternalServerError {
		return err
	}
	return newPageFailure(err.Error())
}

// clipPageMessage 去掉 Call log 和调用栈，压成一行并限长。
func clipPageMessage(raw string) string {
	text := pageANSIPattern.ReplaceAllString(strings.TrimSpace(raw), "")
	if index := strings.Index(text, "Call log:"); index >= 0 {
		text = text[:index]
	}
	if loc := pageStackPattern.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	// 换行和缩进在工具错误里只是噪音，统一压成单空格。
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > pageMessageLimit {
		text = string(runes[:pageMessageLimit-1]) + "…"
	}
	return text
}

// classifyPageError 按错误原文判断类别。
//
// 判断用未裁剪的原文：Playwright 把「元素被遮挡」「元素已脱离 DOM」写在
// Call log 里，裁掉之后就只剩一句 Timeout，靠它分不出具体原因。
func classifyPageError(raw string) string {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "ref not found:"):
		return "ref_stale"
	case strings.Contains(raw, "页面会话响应超时"):
		return "session_timeout"
	case strings.Contains(raw, "页面会话已断开"),
		strings.Contains(raw, "页面会话已结束"),
		strings.Contains(raw, "页面会话已关闭"),
		strings.Contains(lower, "target page, context or browser has been closed"):
		return "session_lost"
	case strings.Contains(lower, "intercepts pointer events"):
		return "element_obstructed"
	case strings.Contains(lower, "not attached to the dom"),
		strings.Contains(lower, "element was detached"),
		strings.Contains(lower, "execution context was destroyed"):
		return "element_detached"
	case strings.Contains(lower, "net::err_aborted"),
		strings.Contains(lower, "interrupted by another navigation"),
		strings.Contains(lower, "navigation interrupted"),
		strings.Contains(lower, "navigation failed because page was closed"):
		return "navigation_interrupted"
	case strings.Contains(lower, "net::err_"):
		return "navigation_failed"
	case strings.Contains(lower, "waiting for locator"),
		strings.Contains(lower, "waiting for selector"),
		strings.Contains(lower, "waiting for element"),
		strings.Contains(lower, "no element found"),
		strings.Contains(lower, "element not found"):
		return "locator_timeout"
	case strings.Contains(lower, "timeout") && (strings.Contains(lower, "exceeded") || strings.Contains(lower, "timed out")):
		return "timeout"
	default:
		return "page_error"
	}
}

// pageFailureHints 是每个类别对应的下一步动作。写给模型看，动词开头。
var pageFailureHints = map[string]string{
	"ref_stale":              "元素编号已过期；重新调用 ant_page_snapshot 取最新编号或 selector 再操作",
	"locator_timeout":        "重新调用 ant_page_snapshot 确认元素是否存在；改用 text= 或 role= 定位，不要重复同一个 CSS",
	"element_obstructed":     "元素被浮层或其他元素遮挡；先关闭弹窗/遮罩，或用 ant_page_snapshot 重新确认可点目标",
	"element_detached":       "页面已重新渲染，旧元素已脱离 DOM；重新 snapshot 后使用新的 selector",
	"navigation_interrupted": "导航被新的跳转或页面关闭打断；等页面稳定后重试，必要时重新 goto",
	"navigation_failed":      "导航没有完成；检查 URL、实例代理和网络连通性，确认后重试",
	"session_timeout":        "页面在超时内没有响应；稍后重试，持续出现时用 ant_page_release 释放会话后重连",
	"session_lost":           "浏览器连接已断开；确认实例仍在运行，重新调用页面工具会自动重建会话",
	"timeout":                "操作在超时内没有完成；确认页面已加载，必要时增大 timeoutMs 后重试",
	"page_error":             "页面操作失败；先用 ant_page_snapshot 确认当前页面状态，再决定下一步",
}

// pageFailureRetryable 表示原样重试是否可能成功。
// 需要先换 selector 或重新观察页面的失败不在此列，避免模型原地打转。
var pageFailureRetryable = map[string]bool{
	"locator_timeout":        true,
	"navigation_interrupted": true,
	"navigation_failed":      true,
	"session_timeout":        true,
	"session_lost":           true,
	"timeout":                true,
}
