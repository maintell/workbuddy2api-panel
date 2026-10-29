// wire_fingerprint_test.go 出站指纹对齐测试（2026-10-29 桌面端 TLS 中间人实抓，5.9.3 / CLI 2.165.0）。
//
// 覆盖三件事：
//  1. chat 头族新增 X-Agent-Runtime=local-agent 与 baggage；
//  2. chat body 恒发 gzip（Content-Encoding: gzip），解压后才是业务 JSON；
//  3. body 缺省补 extra_vars.initiator，显式带则原样保留。
package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersCarryAgentRuntimeAndBaggage 实测桌面端主对话必带这两个头。
func TestChatHeadersCarryAgentRuntimeAndBaggage(t *testing.T) {
	c := New()
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{
		ConversationID:        "0123a914-7838-4604-bed8-342512efa315",
		ConversationRequestID: "01a1858511b27bbaa452cc8ba1787451",
	})

	if got := req.Header.Get("X-Agent-Runtime"); got != "local-agent" {
		t.Errorf("X-Agent-Runtime = %q, want local-agent（桌面托管形态）", got)
	}
	wantBaggage := "buddyId=,buddyName=,codebuddy.session_id=0123a914-7838-4604-bed8-342512efa315," +
		"codebuddy.conversation_request_id=01a1858511b27bbaa452cc8ba1787451"
	if got := req.Header.Get("baggage"); got != wantBaggage {
		t.Errorf("baggage = %q, want %q", got, wantBaggage)
	}
}

// TestChatStreamGzipsBody 出站 chat body 必须是 gzip 体且带 Content-Encoding。
func TestChatStreamGzipsBody(t *testing.T) {
	var (
		wireBody []byte
		gotEnc   string
		gotLen   string
	)
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Content-Encoding")
		gotLen = r.Header.Get("Content-Length")
		wireBody = readWireBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "tok", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("status %d: %s", status, respBody)
	}

	if gotEnc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", gotEnc)
	}
	// Content-Length 必须是压缩体长度（而非明文长度）。
	if gotLen == "" {
		t.Error("Content-Length missing")
	}
	var obj map[string]any
	if err := json.Unmarshal(wireBody, &obj); err != nil {
		t.Fatalf("decompressed wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced after gunzip")
	}
	if obj["model"] != "hy3" {
		t.Errorf("model = %v, want hy3", obj["model"])
	}
}

// TestPrepareBodyExtraVars 缺省补 initiator，显式带则不动。
func TestPrepareBodyExtraVars(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"hy3","messages":[]}`), false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := got["extra_vars"].(map[string]any)
	if !ok {
		t.Fatalf("extra_vars missing: %s", out)
	}
	if ev["initiator"] != "user" {
		t.Errorf("extra_vars.initiator = %v, want user", ev["initiator"])
	}
	if _, has := ev["growthEvent"]; has {
		t.Error("growthEvent 属逐请求埋点，不应被伪造")
	}

	// 显式携带时原样透传（含 growthEvent）。
	src := `{"model":"hy3","messages":[],"extra_vars":{"initiator":"agent","growthEvent":"[{}]"}}`
	out = PrepareBodyOpt([]byte(src), false)
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev = got["extra_vars"].(map[string]any)
	if ev["initiator"] != "agent" || ev["growthEvent"] != "[{}]" {
		t.Errorf("explicit extra_vars overwritten: %v", ev)
	}
}

// TestGzipBodyFallsBackOnEmpty 空 body 不压缩（避免给无 body 的请求加编码头）。
func TestGzipBodyFallsBackOnEmpty(t *testing.T) {
	out, ok := gzipBody(nil)
	if ok {
		t.Error("empty body should not be gzipped")
	}
	if len(out) != 0 {
		t.Errorf("out = %v, want empty", out)
	}
}

// TestGzipBodyRoundTrip 压缩体解压后逐字节等于原文。
func TestGzipBodyRoundTrip(t *testing.T) {
	src := []byte(strings.Repeat(`{"model":"hy3"}`, 50))
	out, ok := gzipBody(src)
	if !ok {
		t.Fatal("expected gzip")
	}
	if len(out) == 0 || len(out) >= len(src) {
		t.Errorf("suspicious compressed length: %d (src %d)", len(out), len(src))
	}
}
