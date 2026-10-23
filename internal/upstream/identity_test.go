package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// setIdentityForTest 安装测试用规则集并在用例结束后复位（进程级 atomic，无并行）。
// prefixChars=0（不限）——规则本身的行为用例不受预算干扰；预算有专门用例覆盖。
func setIdentityForTest(t *testing.T, enabled bool, words []string, target string) {
	t.Helper()
	setIdentityPrefixForTest(t, enabled, words, target, 0)
}

// setIdentityPrefixForTest 同上，但显式指定指令块扫描上限（0 = 不限）。
func setIdentityPrefixForTest(t *testing.T, enabled bool, words []string, target string, prefixChars int) {
	t.Helper()
	SetClientIdentity(enabled, words, target, prefixChars)
	t.Cleanup(func() { SetClientIdentity(false, nil, "", 0) })
}

// 内置词表：第三方客户端身份词改写为目标词，语义不变、大小写不敏感。
func TestClientIdentityRewriteBuiltinWords(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	r := clientIdentity.Load()
	cases := []struct{ in, want string }{
		{"You are opencode, an AI coding agent.", "You are WorkBuddy, an AI coding agent."},
		{"You are OpenCode, an AI coding agent.", "You are WorkBuddy, an AI coding agent."},
		{"You are OpEnCoDe.", "You are WorkBuddy."},
		// 「身份词 + 缩写」结构：两个内置词同时命中后折叠成一次。
		{"You are MiniMax Code (MCode), a CLI tool.", "You are WorkBuddy, a CLI tool."},
		{"You are MiniMax Code（MCode）.", "You are WorkBuddy."},
		// 中文无空格夹用（多字节边界必须能命中）。
		{"用opencode写代码", "用WorkBuddy写代码"},
		// 连续命中：改写的中间段不能被吞掉。
		{"opencode minimax code cline", "WorkBuddy WorkBuddy WorkBuddy"},
		// 句末标点/括号都是边界。
		{"(opencode), [cline].", "(WorkBuddy), [WorkBuddy]."},
		{"没有身份词的普通请求", "没有身份词的普通请求"},
		{"", ""},
	}
	for _, c := range cases {
		got, _ := r.rewrite(c.in)
		if got != c.want {
			t.Errorf("rewrite(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 路径 / 包名 / 标识符里的同名串不能被改写：改写它们等于改用户真实要读写的文件。
func TestClientIdentitySkipsPathAndIdentifiers(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	r := clientIdentity.Load()
	untouched := []string{
		"opencode.json",
		"@opencode/plugin",
		`C:\opencode\bin\opencode.exe`,
		"opencode_plugin",
		"minimax-code",
		"opencode.ai",
		"myopencode",   // 前缀粘连
		"opencodev2",   // 后缀粘连
		"notopencode ", // 前缀是字母
	}
	for _, in := range untouched {
		if got, changed := r.rewrite(in); changed || got != in {
			t.Errorf("rewrite(%q) = %q (changed=%v)，路径/标识符不应被改写", in, got, changed)
		}
	}
}

// 长词优先：minimax code 整词命中，不会先被更短的规则切成两段。
func TestClientIdentityLongestMatchFirst(t *testing.T) {
	setIdentityForTest(t, true, []string{"minimax", "minimax code cli"}, "")
	r := clientIdentity.Load()
	if got, _ := r.rewrite("run minimax code cli now"); got != "run WorkBuddy now" {
		t.Errorf("longest rule not applied: %q", got)
	}
	if got, _ := r.rewrite("run minimax now"); got != "run WorkBuddy now" {
		t.Errorf("shorter rule should still apply: %q", got)
	}
}

// 自定义词三种写法：追加、指定目标、从内置表移除。
func TestClientIdentityCustomWordsAndRemoval(t *testing.T) {
	setIdentityForTest(t, true, []string{"cursor=Claude Code", "!cline"}, "")
	r := clientIdentity.Load()
	if got, _ := r.rewrite("open Cursor and Cline"); got != "open Claude Code and Cline" {
		t.Errorf("custom word/removal not applied: %q", got)
	}
	if got, _ := r.rewrite("opencode"); got != "WorkBuddy" {
		t.Errorf("builtin word lost after custom merge: %q", got)
	}
	// "=>" 形态与 "=" 等价。
	setIdentityForTest(t, true, []string{"qoder=>WorkBuddy Pro"}, "")
	r = clientIdentity.Load()
	if got, _ := r.rewrite("use qoder"); got != "use WorkBuddy Pro" {
		t.Errorf("a=>b 形态未生效: %q", got)
	}
}

// 目标词留空回落内置 WorkBuddy；目标词与身份词同形时不建自环规则。
func TestClientIdentityTargetDefault(t *testing.T) {
	setIdentityForTest(t, true, []string{"opencode=opencode"}, "")
	r := clientIdentity.Load()
	// opencode=opencode：该词被从规则里剔除，但其它内置词照常改写到默认目标。
	if got, _ := r.rewrite("opencode"); got != "opencode" {
		t.Errorf("self-target rule should be dropped: %q", got)
	}
	if got, _ := r.rewrite("cline"); got != "WorkBuddy" {
		t.Errorf("default target not applied: %q", got)
	}
}

// 关闭开关：管线与文本级都不动（nil 规则集全路径零成本退出）。
func TestClientIdentityDisabledNoop(t *testing.T) {
	setIdentityForTest(t, false, nil, "")
	if r := clientIdentity.Load(); r != nil {
		t.Fatalf("rules should be nil when disabled, got %+v", r)
	}
	in := "You are opencode."
	if got, changed := (*identityRules)(nil).rewrite(in); changed || got != in {
		t.Errorf("nil rules must be a no-op: %q changed=%v", got, changed)
	}
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": in}}}
	rewriteClientIdentity(obj)
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != in {
		t.Errorf("body rewritten while disabled: %q", got)
	}
}

// 幂等：改写结果再跑一次不变（不存在规则套规则的自激）。
func TestClientIdentityIdempotent(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	r := clientIdentity.Load()
	first, _ := r.rewrite("You are opencode (MiniMax Code).")
	second, changed := r.rewrite(first)
	if changed || second != first {
		t.Errorf("rewrite not idempotent: %q -> %q (changed=%v)", first, second, changed)
	}
}

// 出站 body 覆盖面：只改最前面连续 system/developer 指令块里的 content。
// user/assistant 消息、思维链、tool_calls 参数都是真实业务内容，必须原样保留。
func TestRewriteClientIdentityBodyScope(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are opencode."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "用 minimax code 跑"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "opencode.png"}},
			}},
			map[string]any{
				"role":              "assistant",
				"content":           "ok",
				"reasoning_content": "thinking about opencode",
				"reasoning":         "cline plan",
				"tool_calls": []any{map[string]any{"function": map[string]any{
					"name":      "bash",
					"arguments": `{"cmd":"cat opencode.json"}`,
				}}},
			},
			// 历史中段的 system 提醒（不在开头连续块里）同样不动。
			map[string]any{"role": "system", "content": "reminder: opencode state"},
		},
	}
	rewriteClientIdentity(obj)

	msgs := obj["messages"].([]any)
	if sys := msgs[0].(map[string]any)["content"]; sys != "You are WorkBuddy." {
		t.Errorf("system content not rewritten: %q", sys)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != "用 minimax code 跑" {
		t.Errorf("user text part must not be rewritten: %q", got)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if img != "opencode.png" {
		t.Errorf("image part must not be touched: %q", img)
	}
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "thinking about opencode" {
		t.Errorf("reasoning_content must not be rewritten: %q", asst["reasoning_content"])
	}
	if asst["reasoning"] != "cline plan" {
		t.Errorf("reasoning must not be rewritten: %q", asst["reasoning"])
	}
	args := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if args != `{"cmd":"cat opencode.json"}` {
		t.Errorf("tool_call arguments must not be touched: %q", args)
	}
	if sys := msgs[3].(map[string]any)["content"]; sys != "reminder: opencode state" {
		t.Errorf("mid-history system message must not be rewritten: %q", sys)
	}
}

// 指令块扫描上限：身份词落在前 N 个字符内才改写，块内更深的文本保持原样；
// 0 = 不限（整段指令块）。多模态 part 共享同一份预算。
func TestClientIdentityPrefixBudget(t *testing.T) {
	// 默认 1000 字符：第 10 个字符处的身份词命中，第 1200 个字符处的不命中。
	setIdentityPrefixForTest(t, true, nil, "", 1000)
	head := "You are opencode. " + strings.Repeat("x", 1200) + " opencode"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": head},
	}}
	rewriteClientIdentity(obj)
	got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, "You are WorkBuddy. ") {
		t.Errorf("身份词（预算内）未被改写: %q", got[:40])
	}
	if !strings.HasSuffix(got, " opencode") {
		t.Errorf("预算外的身份词不应被改写: %q", got[len(got)-40:])
	}

	// 预算 0 = 不限：两处都改写。
	setIdentityPrefixForTest(t, true, nil, "", 0)
	obj = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": head},
	}}
	rewriteClientIdentity(obj)
	got = obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasSuffix(got, " WorkBuddy") {
		t.Errorf("上限 0 应改写整段: %q", got[len(got)-40:])
	}

	// 多模态 part：第一个 part 用掉一半预算，第二个 part 的头部仍可命中。
	setIdentityPrefixForTest(t, true, nil, "", 30)
	obj = map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "opencode first part"},
			map[string]any{"type": "text", "text": "opencode second part"},
		}},
	}}
	rewriteClientIdentity(obj)
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if p0 := parts[0].(map[string]any)["text"]; p0 != "WorkBuddy first part" {
		t.Errorf("part0 未被改写: %q", p0)
	}
	// 预算 30 已在 part0（19 字符）后剩 11：part1 只能扫前 11 个字符，"opencode" 仍在内。
	if p1 := parts[1].(map[string]any)["text"]; p1 != "WorkBuddy second part" {
		t.Errorf("part1 预算内未被改写: %q", p1)
	}
}

// 预算按字符（rune）计，中文占 1 个字符：1000 字符预算内的中文身份词照样命中。
func TestClientIdentityPrefixBudgetRunes(t *testing.T) {
	setIdentityPrefixForTest(t, true, nil, "", 100)
	head := strings.Repeat("中", 80) + " 用 opencode 写"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": head},
	}}
	rewriteClientIdentity(obj)
	got := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasSuffix(got, "用 WorkBuddy 写") {
		t.Errorf("中文场景预算计算错误: %q", got[len(got)-30:])
	}
}

// developer 角色同样属于指令块；出现非指令消息即停止。
func TestClientIdentityScopeDeveloperRole(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "developer", "content": "You are cline."},
		map[string]any{"role": "system", "content": "You are windsurf."},
		map[string]any{"role": "user", "content": "You are aider."},
	}}
	rewriteClientIdentity(obj)
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != "You are WorkBuddy." {
		t.Errorf("developer content not rewritten: %q", got)
	}
	if got := msgs[1].(map[string]any)["content"]; got != "You are WorkBuddy." {
		t.Errorf("连续 system 块内的第二条未改写: %q", got)
	}
	if got := msgs[2].(map[string]any)["content"]; got != "You are aider." {
		t.Errorf("user 消息被改写: %q", got)
	}
}

// 管线出口：PrepareBodyOpt 在 sanitize=false 时也做身份词改写（开关相互独立）。
func TestPrepareBodyRewritesClientIdentity(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	src := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"You are opencode, the coding agent."},` +
		`{"role":"user","content":"用 minimax code 跑一下"}]}`)
	out := PrepareBodyOpt(src, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("prepared body not json: %v", err)
	}
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != "You are WorkBuddy, the coding agent." {
		t.Errorf("system not rewritten on wire: %q", got)
	}
	// 用户业务内容不参与改写（这是本层的安全边界，不是遗漏）。
	if got := msgs[1].(map[string]any)["content"]; got != "用 minimax code 跑一下" {
		t.Errorf("user content must not be rewritten on wire: %q", got)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
}

// 默认词表本身的口径守卫：常用英文词不进内置表（误伤面太大）。
func TestDefaultClientIdentityWordsExcludeCommonWords(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	r := clientIdentity.Load()
	for _, common := range []string{"cursor", "continue", "amp", "goose", "droid", "crush", "zed"} {
		if len(r.byFirst[common[0]]) == 0 {
			continue
		}
		if got, changed := r.rewrite("move the " + common + " here"); changed || got != "move the "+common+" here" {
			t.Errorf("常用词 %q 不应进内置表: %q", common, got)
		}
	}
	// Claude Code / Codex 由 sanitize.go 精细处理，内置表不得整词替换。
	for _, reserved := range []string{"You are Claude Code.", "Codex CLI is an open source project."} {
		if got, changed := r.rewrite(reserved); changed || got != reserved {
			t.Errorf("sanitize 保留词被身份词改写: %q -> %q", reserved, got)
		}
	}
}

// 目标词里带空格/中文时也要原样输出（不做二次折大小写）。
func TestClientIdentityTargetRoundTrip(t *testing.T) {
	setIdentityForTest(t, true, nil, "WorkBuddy 桌面端")
	r := clientIdentity.Load()
	got, _ := r.rewrite("You are opencode.")
	if !strings.HasSuffix(got, "WorkBuddy 桌面端.") {
		t.Errorf("target not emitted verbatim: %q", got)
	}
}

// 厂商名同样改写：服务端检测不只看产品名，"developed by MiniMax" / "led by OpenAI"
// 这类固定句式里的厂商名也是身份信号（用户实测 MiniMax Code 的 persona 提示词就长这样）。
func TestClientIdentityRewriteVendors(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	r := clientIdentity.Load()
	cases := []struct{ in, want string }{
		{
			"You run inside MiniMax Code, a workspace developed by MiniMax.",
			"You run inside WorkBuddy, a workspace developed by WorkBuddy.",
		},
		{"Anthropic's official CLI tool for Claude.", "WorkBuddy's official CLI tool for Claude."},
		{"open source project led by OpenAI.", "open source project led by WorkBuddy."},
		{"powered by Google DeepMind", "powered by WorkBuddy DeepMind"},
	}
	for _, c := range cases {
		if got, _ := r.rewrite(c.in); got != c.want {
			t.Errorf("rewrite(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 域名/包名形态仍然受保护（厂商名更容易出现在 URL 里）。
	for _, in := range []string{"api.openai.com", "google.com", "docs.anthropic.com", "openai-node"} {
		if got, changed := r.rewrite(in); changed || got != in {
			t.Errorf("rewrite(%q) = %q，域名/包名不应被改写", in, got)
		}
	}
}

// "!词" 可单独关掉某个厂商名：产品名规则仍独立生效（"minimax code" 不受影响）。
func TestClientIdentityVendorOptOut(t *testing.T) {
	setIdentityForTest(t, true, []string{"!minimax"}, "")
	r := clientIdentity.Load()
	if got, _ := r.rewrite("developed by MiniMax"); got != "developed by MiniMax" {
		t.Errorf("!minimax 未移除厂商名: %q", got)
	}
	// 产品名 "minimax code" 是另一条规则，仍按最长匹配命中。
	if got, _ := r.rewrite("run inside MiniMax Code"); got != "run inside WorkBuddy" {
		t.Errorf("!minimax 误伤了产品名规则: %q", got)
	}
	// 其他厂商名不受影响。
	if got, _ := r.rewrite("led by OpenAI"); got != "led by WorkBuddy" {
		t.Errorf("其它厂商名被连带移除: %q", got)
	}

	setIdentityForTest(t, true, []string{"!google"}, "")
	r = clientIdentity.Load()
	if got, _ := r.rewrite("search on Google"); got != "search on Google" {
		t.Errorf("!google 未移除厂商名: %q", got)
	}
	if got, _ := r.rewrite("developed by MiniMax"); got != "developed by WorkBuddy" {
		t.Errorf("!google 误伤了其他厂商: %q", got)
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 里，第三方客户端身份词必须已改写。
// 与 sanitize 的 wire 测试同口径——真实客户端（opencode 类）的 system prompt 就长这样。
func TestChatStreamWireBodyClientIdentityRewritten(t *testing.T) {
	setIdentityForTest(t, true, nil, "")
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"You are opencode, an AI coding agent. Config: opencode.json"},` +
		`{"role":"user","content":"用 minimax code 跑一下"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "You are WorkBuddy, an AI coding agent.") {
		t.Errorf("wire system identity not rewritten: %q", sys)
	}
	// 路径形态保持原样：改写它会改到客户端真实读的配置文件名。
	if !strings.Contains(sys, "opencode.json") {
		t.Errorf("wire system lost the config path: %q", sys)
	}
	user, _ := msgs[1].(map[string]any)["content"].(string)
	if user != "用 minimax code 跑一下" {
		t.Errorf("wire user content must not be rewritten: %q", user)
	}
}
