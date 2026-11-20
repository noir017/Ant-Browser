package mcpserver

import (
	"strings"

	"ant-chrome/backend/internal/launchcode"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 页面操作工具：让模型直接驱动浏览器，而不是只能执行预先写好的脚本。
//
// 每个工具在协议层是独立的一次调用，但底层复用同一个常驻会话——CDP 握手只在
// 首次发生，后续调用只是往管道里写一行指令。因此工具粒度可以按「模型好理解」
// 来切，不必为了省启动开销把动作揉在一起。
//
// 所有工具的 selector 都可省略：省略时作用于当前挂在统一 CDP 入口上的实例，
// 这也是连续操作同一个页面时的常态。

// PageState 是每次页面操作后回传的页面状态。
// 统一带上 url/title，模型不必为了确认「现在在哪」再多调一次快照。
// 改页面的动作（goto/click/fill 等）在 Node 侧等页面稳定后才取这两个值。
type PageState struct {
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type pageOutput struct {
	ProfileID string    `json:"profileId"`
	OK        bool      `json:"ok"`
	Page      PageState `json:"page"`
	// 以下仅改页面的动作返回：动作后已等页面稳定。
	Navigated bool     `json:"navigated,omitempty" jsonschema:"动作引发了跳转，page.url 是新地址"`
	Changed   *bool    `json:"changed,omitempty" jsonschema:"动作后 DOM 是否有变动"`
	Busy      bool     `json:"busy,omitempty" jsonschema:"等到上限页面仍在加载或变化"`
	NewTabs   []string `json:"newTabs,omitempty" jsonschema:"动作新开的标签页 URL"`
	Snapshot  string   `json:"snapshot,omitempty" jsonschema:"动作后的快照（传了 snapshot 参数时）"`
	Error     string   `json:"error,omitempty"`
}

// pageCall 是所有页面工具的公共执行路径。
//
// 把 selector 解析、单步指令下发、结果拆包收敛到一处，各工具只负责
// 把自己的入参翻译成一条指令的 args。
func pageCall(p PageProvider, selector *Selector, timeoutMs int, action string, args map[string]any) (*launchcode.PageResult, error) {
	req := launchcode.PageRequest{
		Steps:     []launchcode.PageStep{{Action: action, Args: args}},
		TimeoutMs: timeoutMs,
	}
	if selector != nil {
		req.Selector = selector.toLaunchSelector()
	}
	result, err := p.RunPageSteps(req)
	if err != nil {
		return nil, asPageFailure(err)
	}
	return result, nil
}

// firstStep 取出单步调用的结果。
//
// 会话级失败（连不上、进程断了）和指令级失败（选择器没命中）对模型来说
// 都是「这一步没成」，但错误文案来源不同，这里统一成一个 error。
func firstStep(result *launchcode.PageResult) (map[string]any, error) {
	if result == nil {
		return nil, newPageFailure("页面操作没有返回结果")
	}
	if len(result.Steps) == 0 {
		message := strings.TrimSpace(result.Error)
		if message == "" {
			message = "页面操作没有返回结果"
		}
		return nil, newPageFailure(message)
	}

	step := result.Steps[0]
	if !step.OK {
		message := strings.TrimSpace(step.Error)
		if message == "" {
			message = "页面操作失败"
		}
		return step.Result, newPageFailure(message)
	}
	return step.Result, nil
}

func pageStateOf(payload map[string]any) PageState {
	if payload == nil {
		return PageState{}
	}
	url, _ := payload["url"].(string)
	title, _ := payload["title"].(string)
	return PageState{URL: url, Title: title}
}

// runPageAction 执行一条指令并组装标准的 pageOutput。
func runPageAction(p PageProvider, selector *Selector, timeoutMs int, action string, args map[string]any) (*mcp.CallToolResult, pageOutput, map[string]any) {
	result, err := pageCall(p, selector, timeoutMs, action, args)
	if err != nil {
		return toolError(err), pageOutput{}, nil
	}

	payload, stepErr := firstStep(result)
	if stepErr != nil {
		return toolError(stepErr), pageOutput{ProfileID: result.ProfileID, Error: stepErr.Error()}, payload
	}

	out := pageOutput{ProfileID: result.ProfileID, OK: true, Page: pageStateOf(payload)}
	applyEffect(&out, payload)
	return nil, out, payload
}

// applyEffect 取出 Node 侧动作后的变化摘要。
func applyEffect(out *pageOutput, payload map[string]any) {
	out.Navigated, _ = payload["navigated"].(bool)
	if changed, ok := payload["changed"].(bool); ok {
		out.Changed = &changed
	}
	out.Busy, _ = payload["busy"].(bool)
	if tabs, ok := payload["newTabs"].([]any); ok {
		out.NewTabs = decodeStrings(tabs)
	}
	out.Snapshot, _ = payload["snapshot"].(string)
}

// ---------------------------------------------------------------------------
// 入参 / 出参
// ---------------------------------------------------------------------------

// targetInput 是所有页面工具共享的目标与超时参数。
type targetInput struct {
	Selector  *Selector `json:"selector,omitempty" jsonschema:"目标实例；省略则作用于当前活动实例"`
	TimeoutMs int       `json:"timeoutMs,omitempty" jsonschema:"单步超时毫秒数，默认 30000，上限 300000"`
}

// elementInput 是需要定位元素的工具共享的参数。
type elementInput struct {
	targetInput
	Element       string `json:"element" jsonschema:"快照编号如 e12，或 CSS / text= / role= 选择器"`
	FrameSelector string `json:"frameSelector,omitempty" jsonschema:"CSS 选择器指向 iframe 内元素时，该 iframe 的选择器；快照编号不需要"`
	Index         *int   `json:"index,omitempty" jsonschema:"选择器命中多个元素时取第几个，从 0 开始"`
}

// settleInput 是改页面的动作共享的参数。
type settleInput struct {
	Snapshot bool `json:"snapshot,omitempty" jsonschema:"动作后附带新快照，省一次 ant_page_snapshot"`
}

func (in settleInput) apply(args map[string]any) map[string]any {
	if in.Snapshot {
		args["snapshot"] = true
	}
	return args
}

func (in elementInput) args(extra map[string]any) map[string]any {
	args := map[string]any{"selector": strings.TrimSpace(in.Element)}
	if frame := strings.TrimSpace(in.FrameSelector); frame != "" {
		args["frameSelector"] = frame
	}
	if in.Index != nil {
		args["index"] = *in.Index
	}
	for key, value := range extra {
		args[key] = value
	}
	return args
}

type gotoInput struct {
	targetInput
	settleInput
	URL       string `json:"url" jsonschema:"要打开的完整 URL"`
	WaitUntil string `json:"waitUntil,omitempty" jsonschema:"等待时机：domcontentloaded（默认）/ load / networkidle / commit"`
}

type gotoOutput struct {
	pageOutput
	Status int `json:"status,omitempty" jsonschema:"导航响应的 HTTP 状态码"`
}

type snapshotInput struct {
	targetInput
	Query       string `json:"query,omitempty" jsonschema:"只留含这些词的行，不分大小写；空格表示且，| 表示或，如 \"submit|login\""`
	Limit       int    `json:"limit,omitempty" jsonschema:"最多列出多少个元素，默认 200，上限 1000；超出部分只报数量"`
	IncludeText bool   `json:"includeText,omitempty" jsonschema:"附带页面正文（也按 query 过滤），默认否"`
}

type screenshotInput struct {
	targetInput
	Element  string `json:"element,omitempty" jsonschema:"只截取该元素；省略则截整个视口"`
	FullPage bool   `json:"fullPage,omitempty" jsonschema:"截取完整页面而非视口，默认否；长页面会显著变大"`
	Type     string `json:"type,omitempty" jsonschema:"图片格式：jpeg（默认，体积小）或 png"`
	Quality  int    `json:"quality,omitempty" jsonschema:"jpeg 质量 1-100，默认 60"`
}

type clickInput struct {
	elementInput
	settleInput
	Button     string `json:"button,omitempty" jsonschema:"鼠标键：left（默认）/ right / middle"`
	ClickCount int    `json:"clickCount,omitempty" jsonschema:"点击次数，默认 1"`
	Force      bool   `json:"force,omitempty" jsonschema:"跳过可点击性检查，默认否"`
}

// FillField 是一个待填字段。
type FillField struct {
	Element       string `json:"element" jsonschema:"快照编号如 e12，或 CSS / text= / role= 选择器"`
	Value         string `json:"value" jsonschema:"要填入的值"`
	FrameSelector string `json:"frameSelector,omitempty" jsonschema:"CSS 选择器指向 iframe 内元素时，该 iframe 的选择器"`
}

type fillInput struct {
	targetInput
	settleInput
	Fields []FillField `json:"fields" jsonschema:"要填写的字段列表；一次传多个可以在一轮里填完整张表单"`
}

type fillOutput struct {
	pageOutput
	Filled []string `json:"filled" jsonschema:"实际填写的元素"`
}

type pressInput struct {
	targetInput
	settleInput
	Key           string `json:"key" jsonschema:"按键名，例如 Enter、Escape、Tab、Control+A"`
	Element       string `json:"element,omitempty" jsonschema:"先聚焦到该元素再按键（快照编号或选择器）；省略则作用于当前焦点"`
	FrameSelector string `json:"frameSelector,omitempty"`
}

type selectInput struct {
	elementInput
	settleInput
	Values []string `json:"values" jsonschema:"要选中的选项，value 或可见文字均可；单选传一个元素的数组"`
}

type selectOutput struct {
	pageOutput
	Selected []string `json:"selected" jsonschema:"实际选中的选项值"`
}

type hoverInput struct {
	elementInput
	settleInput
}

type checkInput struct {
	elementInput
	settleInput
	Checked *bool `json:"checked,omitempty" jsonschema:"true 勾选（默认），false 取消勾选"`
}

type waitInput struct {
	targetInput
	Element   string `json:"element,omitempty" jsonschema:"等待该元素达到目标状态（快照编号或选择器）"`
	State     string `json:"state,omitempty" jsonschema:"元素目标状态：visible（默认）/ hidden / attached / detached"`
	URL       string `json:"url,omitempty" jsonschema:"等待 URL 匹配该值，支持 glob 通配"`
	LoadState string `json:"loadState,omitempty" jsonschema:"等待加载态：load / domcontentloaded / networkidle"`
}

type extractInput struct {
	elementInput
	Mode      string `json:"mode,omitempty" jsonschema:"抽取内容：text（默认）/ html / attribute"`
	Attribute string `json:"attribute,omitempty" jsonschema:"要读取的属性名，仅在 mode 为 attribute 时使用"`
	All       bool   `json:"all,omitempty" jsonschema:"抽取所有命中元素而非第一个，默认否"`
}

type extractOutput struct {
	ProfileID string   `json:"profileId"`
	OK        bool     `json:"ok"`
	Mode      string   `json:"mode"`
	Value     string   `json:"value,omitempty" jsonschema:"单个结果，仅在 all 为 false 时返回"`
	Values    []string `json:"values,omitempty" jsonschema:"全部结果，仅在 all 为 true 时返回"`
	Count     int      `json:"count,omitempty"`
}

type evaluateInput struct {
	targetInput
	Expression string `json:"expression" jsonschema:"在页面上下文里执行的 JS：表达式或箭头函数，例如 () => document.title"`
	Arg        any    `json:"arg,omitempty" jsonschema:"传给箭头函数的参数，必须可 JSON 序列化"`
}

type evaluateOutput struct {
	ProfileID string `json:"profileId"`
	OK        bool   `json:"ok"`
	Value     any    `json:"value" jsonschema:"表达式的返回值；不可序列化的部分会被转成字符串描述"`
}

type tabsInput struct {
	targetInput
	Op    string `json:"op,omitempty" jsonschema:"操作：list（默认）/ new / select / close"`
	Index *int   `json:"index,omitempty" jsonschema:"select / close 的目标标签页序号，从 0 开始"`
	URL   string `json:"url,omitempty" jsonschema:"新标签页要打开的 URL，仅在 op 为 new 时使用"`
}

// PageTab 是一个标签页。
type PageTab struct {
	Index  int    `json:"index"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
	Active bool   `json:"active,omitempty"`
}

type tabsOutput struct {
	ProfileID string    `json:"profileId"`
	OK        bool      `json:"ok"`
	Count     int       `json:"count"`
	Items     []PageTab `json:"items,omitempty"`
	Page      PageState `json:"page,omitempty"`
}

type releaseInput struct {
	Selector *Selector `json:"selector,omitempty" jsonschema:"目标实例；省略则释放当前活动实例的会话"`
}

type releaseOutput struct {
	ProfileID string `json:"profileId"`
	Released  bool   `json:"released"`
}
