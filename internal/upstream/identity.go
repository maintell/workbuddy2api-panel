// identity.go 出站请求体的客户端身份词改写（与 sanitize.go 并列的第三层）。
//
// 背景：第三方 coding agent 客户端（OpenCode / MiniMax Code / Cline …）会在 system
// prompt 里声明自己的身份（"You are opencode, ..."），并把这套说法一路带进历史消息。
// 上游对「请求是否来自官方客户端」的判定除 header/UA 外还会看提示词里的身份词，
// 命中后表现为 11-128 通道校验失败或内容策略拦截——与用户真实意图无关。
//
// 策略：把已知客户端身份词在**指令块开头的前 N 个字符**里按规则改写为目标词
// （默认 WorkBuddy），让出站请求与官方客户端同形；对话内容、工具入参与思维链一律
// 不动——它们承载真实业务，改写等于篡改数据。
//
// 与另两层的关系（三者互不替代）：
//   - prompt（internal/prompt）：custom/append 模式整体替换/追加 system；
//   - sanitize.go：黑名单**逐字指纹**（Claude Code / Codex 模板句、header 键名）；
//   - 本文件：客户端**身份词**（产品名 + 厂商名），面向"这些客户端名会触发通道校验"。
package upstream

import (
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// DefaultClientIdentityTarget 身份词改写的缺省目标词（官方产品名）。
const DefaultClientIdentityTarget = "WorkBuddy"

// defaultClientIdentityWords 内置身份词表（大小写不敏感）。
//
// 产品名收录口径（三条都是踩过坑之后的边界，不是随手列的）：
//  1. 只收**唯一性够强**的产品名：cursor / amp / goose / droid / continue / crush 这些
//     英文常用词不进内置表（"set the cursor"、"continue reading" 会被误改写），
//     需要时由 features.client_identity_words 显式追加；
//  2. 厂商名另表（见 defaultClientIdentityVendors）：服务端检测不只看产品名，
//     "a workspace developed by MiniMax" 里的厂商名同样是身份信号；
//  3. Claude Code / Codex 不进内置表：它们的逐字指纹已由 sanitize.go 精细改写
//     （只改一个词、保留可读性），在这里再整词替换会把上游"认得、只需微调"的句子
//     抹掉，也会和 sanitize 的既有断言打架（见 TestChatStreamWireBodyCodexInstructionsSanitized）。
//     同理不收 "claude"/"gpt"/"gemini"/"qwen" 这类**模型名**：它们出现在
//     sanitize 改写的句子里（"official CLI tool for Claude"），整词替换会让该层失效。
var defaultClientIdentityWords = []string{
	"opencode",
	"minimax code",
	"mcode",
	"qwen code",
	"gemini cli",
	"copilot cli",
	"github copilot",
	"roo code",
	"kilo code",
	"cline",
	"aider",
	"windsurf",
	"kiro",
	"iflow",
	"qoder",
	"trae",
	"lingma",
	"comate",
	"codegeex",
	"marscode",
	"通义灵码",
	"文心快码",
}

// defaultClientIdentityVendors 内置**厂商名**表（大小写不敏感，与产品名共用目标词）。
//
// 为什么单独成表：服务端检测"非官方客户端"不只看产品名——"You run inside MiniMax
// Code, a workspace developed by MiniMax." 里产品名被改写后，剩下的 "MiniMax" 仍是
// 身份信号；Anthropic / OpenAI / Google 同理（官方 prompt 里 "developed by X"、
// "led by X" 是固定句式）。
//
// 风险与逃生门：厂商名在用户对话里也可能是**正常讨论对象**（"OpenAI 兼容接口怎么配"），
// 因此这一组必然比产品名更激进——用 features.client_identity_words 里的 "!词"
// 可以单个关掉（如 "!google"），不必整套关闭。路径/域名形态（api.openai.com、
// google.com）由词边界规则保护，不会被改。
//
// 刻意不收的厂商名：tencent / codebuddy（上游自家产品线，官方客户端就该长这样）、
// meta / x / apple（英文常用词或歧义太大）、以及所有中文厂商名（"百度一下" 之类
// 日常表达误伤面远大于收益）。
var defaultClientIdentityVendors = []string{
	"minimax",
	"anthropic",
	"openai",
	"google",
	"deepseek",
	"moonshot",
	"alibaba",
	"bytedance",
	"baidu",
	"mistral",
}

// identityRule 单条改写规则：from 已 ASCII 小写，to 原样输出。
type identityRule struct {
	from string
	to   string
}

// identityRules 编译后的规则集（构建后只读，可被 atomic.Pointer 安全共享）。
type identityRules struct {
	// byFirst 首字节（ASCII 小写）→ 规则桶。桶内按 from 长度降序，
	// 保证 "minimax code cli" 先于 "minimax code" 命中。
	byFirst [256][]identityRule
	// targets 全部目标词（用于折叠 "WorkBuddy (WorkBuddy)" 这类重复）。
	targets []string
	count   int
	// prefixChars 只改写指令块前 N 个字符（0 = 不限）。见 Config.ClientIdentityPrefixChars。
	prefixChars int
}

// clientIdentity 进程级规则集：nil = 关闭（库的零值即关闭，cmd/server 按配置安装）。
var clientIdentity atomic.Pointer[identityRules]

// DefaultClientIdentityPrefixChars 身份词默认只扫指令块前 1000 个字符。
//
// 依据：客户端身份声明（"You run inside X" / "You are X, ..."）总在 system prompt
// 开头；指令块后半段更可能是客户端注入的上下文、系统提醒、甚至被引用的用户原文，
// 改写它们等于篡改真实业务内容。0 = 不限（整段 system/developer 都改写）。
const DefaultClientIdentityPrefixChars = 1000

// SetClientIdentity 安装出站身份词改写规则（进程级、热生效）。
//
//   - enabled=false 或有效规则为空 → 关闭（置 nil，管线零成本退出）；
//   - words 为空 = 只用内置表（产品名 + 厂商名）；非空 = 内置表 ∪ 自定义词
//     （面板追客户端名不必等发版）；
//   - 单条 words 支持三种写法：
//     "opencode"       → 追加一个身份词，目标 = target；
//     "cursor=WorkBuddy" / "cursor=>WorkBuddy" → 指定该词的目标；
//     "!cline"         → 从内置表移除（内置词误伤时的自救口，产品名/厂商名都适用，
//     如 "!minimax" 只关掉厂商名 MiniMax，"!google" 关掉 Google）。
//   - target 为空 → 回落 DefaultClientIdentityTarget；
//   - prefixChars 是**指令块扫描上限**（字符数，0 = 不限）：只改写请求最前面连续
//     system/developer 块的前 N 个字符，块外内容（user/assistant/tool 消息）永不改写。
func SetClientIdentity(enabled bool, words []string, target string, prefixChars int) {
	if !enabled {
		clientIdentity.Store(nil)
		return
	}
	clientIdentity.Store(buildIdentityRules(target, words, prefixChars))
}

// buildIdentityRules 编译规则集；无有效规则返回 nil。
func buildIdentityRules(target string, words []string, prefixChars int) *identityRules {
	target = strings.TrimSpace(target)
	if target == "" {
		target = DefaultClientIdentityTarget
	}
	pairs := make(map[string]string, len(defaultClientIdentityWords)+len(defaultClientIdentityVendors)+len(words))
	for _, w := range defaultClientIdentityWords {
		pairs[asciiLower(w)] = target
	}
	// 厂商名默认同样并入（服务端检测把它们也算身份信号）；"!词" 可单独移除。
	for _, w := range defaultClientIdentityVendors {
		pairs[asciiLower(w)] = target
	}
	for _, raw := range words {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "!") { // 移除内置词（误伤自救）
			delete(pairs, asciiLower(strings.TrimSpace(entry[1:])))
			continue
		}
		from, to := entry, target
		if i := strings.Index(entry, "="); i > 0 {
			from = strings.TrimSpace(entry[:i])
			to = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(entry[i+1:]), ">"))
			if to == "" {
				to = target
			}
		}
		from = asciiLower(strings.TrimSpace(from))
		if from == "" {
			continue
		}
		pairs[from] = to
	}
	r := &identityRules{}
	targetSeen := make(map[string]struct{}, len(pairs))
	for from, to := range pairs {
		if from == "" || asciiLower(from) == asciiLower(to) {
			continue // 目标词自身不建规则，避免自环与无意义改写
		}
		b := from[0]
		r.byFirst[b] = append(r.byFirst[b], identityRule{from: from, to: to})
		r.count++
		if _, ok := targetSeen[to]; !ok {
			targetSeen[to] = struct{}{}
			r.targets = append(r.targets, to)
		}
	}
	if r.count == 0 {
		return nil
	}
	// 负数无意义：回落默认上限（不静默变成"不限"——那会把保护关掉）。
	if prefixChars < 0 {
		prefixChars = DefaultClientIdentityPrefixChars
	}
	r.prefixChars = prefixChars
	for i := range r.byFirst {
		bucket := r.byFirst[i]
		if len(bucket) < 2 {
			continue
		}
		sort.Slice(bucket, func(a, b int) bool { return len(bucket[a].from) > len(bucket[b].from) })
	}
	return r
}

// rewrite 对一段文本做身份词改写；无命中返回原串（零分配）。
//
// 匹配语义：
//   - ASCII 大小写不敏感（非 ASCII 字节原样比较，UTF-8 多字节字符天然不会被折半）；
//   - 词边界 = 前后都不是「词内字节」（见 identityIdentByte），因此
//     opencode.json / @opencode/plugin / minimax-code 这类**路径与包名**保持原样——
//     改写它们会改到用户真实要读写的文件名，风险远大于收益；
//   - 长词优先（"minimax code" 先于 "mcode"、先于 "minimax"）；
//   - 连续命中互不重叠：改写结果不会被二次扫描（不存在规则套规则的自激）。
func (r *identityRules) rewrite(text string) (string, bool) {
	if r == nil || r.count == 0 || text == "" {
		return text, false
	}
	// 第一遍只定位首个命中：无命中的普通请求零分配返回（逐字节查表，无正则、无 ToLower）。
	start, end := -1, 0
	var first identityRule
	for i := 0; i < len(text); {
		if rule, e, hit := r.match(text, i); hit {
			start, end, first = i, e, rule
			break
		}
		i++
	}
	if start < 0 {
		return text, false
	}
	var b strings.Builder
	b.Grow(len(text))
	b.WriteString(text[:start])
	b.WriteString(first.to)
	last := end
	for i := end; i < len(text); {
		rule, e, hit := r.match(text, i)
		if !hit {
			i++
			continue
		}
		b.WriteString(text[last:i])
		b.WriteString(rule.to)
		last, i = e, e
	}
	b.WriteString(text[last:])
	return r.collapseDuplicates(b.String()), true
}

// match 在 text[i:] 起点尝试匹配一条规则（桶内长词优先，命中即返回）。
func (r *identityRules) match(text string, i int) (identityRule, int, bool) {
	for _, rule := range r.byFirst[foldASCII(text[i])] {
		if !hasPrefixFold(text[i:], rule.from) {
			continue
		}
		end := i + len(rule.from)
		if !identityBoundary(text, i, end) {
			continue
		}
		return rule, end, true
	}
	return identityRule{}, 0, false
}

// collapseDuplicates 折叠 "WorkBuddy (WorkBuddy)" / "WorkBuddy（WorkBuddy）" 这类
// 「身份词 + 缩写」结构被同时命中的重复（"MiniMax Code (MCode)" 的典型形态）。
// 只折叠本规则集自己的目标词，不碰用户文本里的其他重复。
func (r *identityRules) collapseDuplicates(text string) string {
	for _, t := range r.targets {
		if t == "" {
			continue
		}
		for _, pair := range [][2]string{
			{t + " (" + t + ")", t},
			{t + "(" + t + ")", t},
			{t + "（" + t + "）", t},
		} {
			text = strings.ReplaceAll(text, pair[0], pair[1])
		}
	}
	return text
}

// identityBoundary 判定 [start,end) 两侧是否都不粘连标识符/路径。
// 非 ASCII 字节（中文等）天然满足边界——"用opencode写" 这种无空格中文夹用要能命中。
func identityBoundary(s string, start, end int) bool {
	if start > 0 && identityIdentByte(s[start-1]) {
		return false
	}
	if end >= len(s) {
		return true
	}
	// 右侧的 '.' 单独判：句末句号（"You are opencode."）是边界，点后跟字母数字
	// 才是扩展名/域名的粘连形态（opencode.json / opencode.ai 一律不改）。
	if s[end] == '.' {
		return end+1 >= len(s) || !isAlnumASCII(s[end+1])
	}
	return !identityIdentByte(s[end])
}

// isAlnumASCII 判定 ASCII 字母或数字。
func isAlnumASCII(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// identityIdentByte 词内字节：字母/数字/_/-/./ / \ @。
// 这些字符两侧出现说明命中处是**标识符、路径或包名**（opencode.json、opencode-ai、
// @opencode/plugin、C:\opencode\bin），不是独立的身份词。
func identityIdentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '-', '.', '/', '\\', '@':
		return true
	}
	return false
}

// hasPrefixFold 判断 s 是否以已小写的 lower 开头（仅 ASCII 折大小写）。
func hasPrefixFold(s, lower string) bool {
	if len(s) < len(lower) {
		return false
	}
	for i := 0; i < len(lower); i++ {
		if foldASCII(s[i]) != lower[i] {
			return false
		}
	}
	return true
}

// foldASCII ASCII 大写 → 小写；其他字节原样返回。
func foldASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// asciiLower 只折 ASCII 大小写（中文/其他多字节字符逐字节保留）。
func asciiLower(s string) string {
	upper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			upper = true
			break
		}
	}
	if !upper {
		return s
	}
	b := []byte(s)
	for i := range b {
		b[i] = foldASCII(b[i])
	}
	return string(b)
}

// rewriteClientIdentity 对整份出站请求体做身份词改写。
//
// 覆盖面刻意收得很窄——它对齐的是「上游看到的客户端身份」，不是内容脱敏：
//   - **只改写请求最前面连续的 system/developer 指令块**：身份声明只出现在这里
//     （"You run inside X" / "You are X, ..."）；从第一条非 system/developer 消息
//     起即停止，历史中的 system 提醒、user/assistant/tool 消息一律不动。
//   - **块内也只扫前 prefixChars 个字符**（默认 1000，0 = 不限）：指令块后半段常是
//     注入的上下文 / 系统提醒 / 被引用的用户原文，改写它们会篡改真实业务内容。
//   - **不动** tool_calls[].function.arguments 与思维链字段：前者是模型产出的工具
//     入参（要写入磁盘的文件内容、命令参数），后者是模型自己的推理；两者都不是
//     身份声明，改写只会有静默的数据破坏风险。
func rewriteClientIdentity(obj map[string]any) {
	r := clientIdentity.Load()
	if r == nil {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	budget := r.prefixChars // 0 = 不限
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			break
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			break // 指令块结束：之后的一切都是对话内容
		}
		if c, ok := m["content"]; ok {
			nc, changed, used := identityPrefixContent(r, c, budget)
			if changed {
				m["content"] = nc
			}
			if budget > 0 {
				budget -= used
				if budget <= 0 {
					return // 上限用尽：再往后不做任何改写
				}
			}
		}
	}
}

// identityPrefixContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回改写后的值、是否变化、以及本次实际扫描的字符数（用于跨消息扣预算）。
func identityPrefixContent(r *identityRules, v any, budget int) (any, bool, int) {
	switch c := v.(type) {
	case string:
		out, changed, used := r.rewritePrefix(c, budget)
		return out, changed, used
	case []any:
		changed, used := false, 0
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			left := budget
			if budget > 0 {
				left = budget - used
				if left <= 0 {
					break // 预算用尽，后续 part 不动
				}
			}
			out, ch, u := r.rewritePrefix(text, left)
			used += u
			if ch {
				m["text"] = out
				changed = true
			}
		}
		return c, changed, used
	}
	return v, false, 0
}

// rewritePrefix 只改写 text 的前 limit 个字符（limit<=0 = 不限）。
// 切分点落在 UTF-8 字符边界上，跨边界的词不匹配（预算边界优先于改写完整性）。
func (r *identityRules) rewritePrefix(text string, limit int) (string, bool, int) {
	if limit <= 0 {
		out, changed := r.rewrite(text)
		return out, changed, utf8.RuneCountInString(text)
	}
	cut := runeCutOffset(text, limit)
	head, tail := text[:cut], text[cut:]
	out, changed := r.rewrite(head)
	if !changed {
		return text, false, utf8.RuneCountInString(head)
	}
	return out + tail, true, utf8.RuneCountInString(head)
}

// runeCutOffset 返回前 n 个字符（rune）之后的字节下标；不足 n 个字符返回 len(s)。
func runeCutOffset(s string, n int) int {
	if n <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}
