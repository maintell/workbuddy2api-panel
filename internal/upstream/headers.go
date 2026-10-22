// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersionCN / defaultClientVersionGlobal 出站 WorkBuddy 客户端版本段
	// （UA 的 `WorkBuddy/<ver>` 与 X-IDE-Version），按 realm 取本机实测的最新桌面端：
	// CN = WorkBuddy Desktop 5.8.3，国际版 = WorkBuddy AI 5.8.4（app.asar package.json，
	// 取证 2026-10-21/22）。config upstream.client_version 可覆盖（空 = 按 realm 默认）。
	defaultClientVersionCN     = "5.8.3"
	defaultClientVersionGlobal = "5.8.4"
	// defaultSlugCN / defaultSlugGlobal UA 第二段的品牌 slug（实测：CN 桌面端
	// `workbuddy`，国际版 `workbuddy-ai`，取自 product.json productName/urlProtocol）。
	defaultSlugCN     = "workbuddy"
	defaultSlugGlobal = "workbuddy-ai"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。两个桌面端自带 CLI 均为 2.161.0
	// （resources/app.asar.unpacked/cli `--version`）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.161.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本（无 realm 上下文时按 CN 默认）。
func (c *Client) clientVersion() string { return c.clientVersionFor(nil) }

// clientVersionFor 按 realm 取生效版本：显式 config > global 5.8.4 > CN 5.8.3。
func (c *Client) clientVersionFor(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<ver> WorkBuddy/<ver> [CLI/<cli>]`。
// 平台段恒为 `WorkBuddy`：国际版 WorkBuddy AI 5.8.4 的 buildVlmGatewayHeaders
// 同样硬编码 `WorkBuddy/<ver>` 两段（其 app.asar 内 "WorkBuddy AI" 字符串 0 命中），
// 国际差异只体现在版本号（5.8.4）与 endpoint（www.workbuddy.ai）。
// 显式 config cli_version 为空时不追加 CLI 段（官方仅在 platformVersion 存在时追加）。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	v := c.clientVersionFor(a)
	slug := defaultSlugCN
	if a != nil && a.IsGlobal() {
		slug = defaultSlugGlobal
	}
	ua := "WorkBuddy/" + v + " " + slug + "/" + v
	if cli := c.cliVersion(); cli != "" {
		ua += " CLI/" + cli
	}
	return ua
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（平台段恒 WorkBuddy，版本按 realm）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA() string { return c.billingUAFor(nil) }

// billingUAFor 按 realm 取版本的 billing UA。
func (c *Client) billingUAFor(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersionFor(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置所有 API 共享的请求头。
//
// 注意：X-Machine-ID / X-Session-ID 已移除——对 WorkBuddy Desktop 5.8.3 的
// app.asar（96 MB 全量检索）与官方 CLI 2.161.0/2.171.0 双向取证，两者都不发送
// 这两个头；反向「官方会发」的旧注释已被实证推翻。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 路径保持同一值（官方桌面端流式请求也只发 application/json）。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：透传入站值，空则生成请求级兜底
	ConversationRequestID string // X-Conversation-Request-ID / acp-connection-id 派生源：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 身份/企业/域头缺失时直接省略，不发 X-No-* 占位（官方 chat 不出现这类头）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// 对齐桌面端实测 chat 出站：Accept 恒 application/json；Accept-Encoding 交给
	// net/http 默认（gzip，且由 Transport 自动解压），不手写 identity。
	// 桌面端转发前按 denylist 剥掉 origin，Node 侧也不产生 Referer / Accept-Language，
	// 故 chat 路径一并删除这三个头。
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	req.Header.Del("Accept-Language")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	// 身份头按「有值才发」：官方 chat 不发送 X-No-* 占位头（那组仅用于登录轮询，
	// 且值为 "true"），缺失时直接省略。
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 原样透传 EnterpriseID/Domain，缺失则省略；
	// global 由 injectGlobalChatHeaders 只声明国际版域 X-Domain=www.workbuddy.ai，
	// 不回退到登录会话原值——对齐 intl 出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
			req.Header.Set("X-Tenant-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备令牌：实测桌面端 chat 也带 X-Device-Token（登录时上游签发，非每号必配）。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
}

// injectGlobalChatHeaders global 账号的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：只声明国际版域 X-Domain=www.workbuddy.ai。
// 仅 global realm 注入；CN 账号不受影响。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	// X-No-* 占位头不再发送：桌面端只在登录轮询用它们（且值 "true"），chat 不出现。
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：优先透传调用方的会话 ID；缺失时生成请求级兜底。
//     国际版渠道校验要求三个 X-Conversation-* 头齐全，缺任一个会报
//     400 code=11128 "conversation id missing"（issue #187 / #188）。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//     与官方 CLI 2.161.0 实抓一致（messageId 去横线后同时写这两个头）。
//   - acp-connection-id：桌面端每会话一个 UUID（metadata.acpConnectionId），
//     这里由 conversationRequestID 稳定派生，同一对话内恒同值。
//   - traceparent / b3 / X-B3-TraceId / X-B3-ParentSpanId / X-B3-SpanId /
//     X-B3-Sampled / X-Trace-ID：桌面端 applyTraceHeaders 一次生成 trace/span/
//     parent 三件后同时下发 W3C + B3 两族；X-Trace-ID = trace id。
//   - X-Root-Request-ID 已移除：桌面端 0 命中（仅 CLI 在 meta.rootRequestId 时发）。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	// 会话头三元组必须齐全：国际端点收紧通道校验后，缺 X-Conversation-ID 的
	// 请求会直接 400 / 11128（conversation id missing）。入站没带会话 ID 时
	// 本级补一个 32 hex（与 X-Conversation-Request-ID 的零值兜底同口径），
	// 不再因为「透传优先」而漏发该头（issue #190）。
	conversationID := meta.ConversationID
	if conversationID == "" {
		conversationID = session.NewMessageID()
	}
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	// acp-connection-id 实测与 conversation-request-id **不同值**，按会话稳定派生。
	req.Header.Set("acp-connection-id", asUUID(deriveHex("acp", convReqID)))
	traceID := meta.TraceID
	if len(traceID) != 32 || !validTraceID(traceID) {
		traceID = session.NewMessageID()
	}
	spanID := session.NewMessageID()[:16]
	parentID := session.NewMessageID()[:16]
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-ParentSpanId", parentID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", traceID)
}

// deriveHex 由固定盐 + 输入派生 32 位 hex（用于 acp-connection-id 等需要与会话 ID
// 不同的稳定标识）。
func deriveHex(purpose, input string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + input))
	return hex.EncodeToString(sum[:16])
}

// asUUID 把 32 位 hex 渲染成 8-4-4-4-12 的 UUID 形态；其他形状原样返回。
// 用于 acp-connection-id 等桌面端为 UUID 的头，避免出现「全 hex 无连字符」的怪形状。
func asUUID(s string) string {
	if len(s) != 32 || !validTraceID(s) {
		return s
	}
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入归属 / 智能体头，对齐桌面端 5.8.3 applyGatewayHeaders：
// X-Agent-Intent=craft、X-Agent-Purpose=conversation、X-Private-Data=true、
// X-IDE-Type/Name=WorkBuddy、X-IDE-Version=<client_version>、
// X-Product=SaaS（deploymentType，**不是**品牌名）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
// 显式 ClientName="SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-* / agent 头）；
// 配其他值则 IDE 两段跟随该值。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Private-Data", "true")
	// OpenAI SDK（stainless）指纹：实测桌面端 chat 同样携带，取实抓常量。
	req.Header.Set("X-Stainless-Arch", "x64")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-Os", "Windows")
	req.Header.Set("X-Stainless-Package-Version", "6.25.0")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", "v22.21.1")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersionFor(a))
	req.Header.Set("X-Product", "SaaS")
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUAFor(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
