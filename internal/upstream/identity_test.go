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
func setIdentityForTest(t *testing.T, enabled bool, words []string, target string) {
	t.Helper()
	SetClientIdentity(enabled, words, target)
	t.Cleanup(func() { SetClientIdentity(false, nil, "") })
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
	obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": in}}}
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

// 出站 body 覆盖面：content（字符串/多模态 part）与思维链字段改写；
// tool_calls 参数是用户数据，保持原样。
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
		},
	}
	rewriteClientIdentity(obj)

	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"]
	if sys != "You are WorkBuddy." {
		t.Errorf("system content not rewritten: %q", sys)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != "用 WorkBuddy 跑" {
		t.Errorf("text part not rewritten: %q", got)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if img != "opencode.png" {
		t.Errorf("image part must not be touched: %q", img)
	}
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "thinking about WorkBuddy" {
		t.Errorf("reasoning_content not rewritten: %q", asst["reasoning_content"])
	}
	if asst["reasoning"] != "WorkBuddy plan" {
		t.Errorf("reasoning not rewritten: %q", asst["reasoning"])
	}
	args := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if args != `{"cmd":"cat opencode.json"}` {
		t.Errorf("tool_call arguments must not be touched: %q", args)
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
	if got := msgs[1].(map[string]any)["content"]; got != "用 WorkBuddy 跑一下" {
		t.Errorf("user content not rewritten on wire: %q", got)
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
	if user != "用 WorkBuddy 跑一下" {
		t.Errorf("wire user content not rewritten: %q", user)
	}
}
