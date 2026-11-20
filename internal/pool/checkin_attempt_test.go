// checkin_attempt_test.go 钉住补签尝试台账：次数累计、下一次允许时刻、
// 跨零点自然重置、持久化往返不丢。
//
// 为什么需要这份台账：补签循环每 30 分钟扫一次未签号，凭证已死的号若无限重试
// 会变成对上游的持续轰炸；次数 + 退避 + 每日重置是「保底」与「风控」的分界线。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestNoteCheckinAttemptCountsAndNextAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.CheckinAttempts != 0 || !st.CheckinNextAt.IsZero() {
		t.Fatalf("初始不应有尝试记录: %+v", st)
	}
	next := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	if n := p.NoteCheckinAttempt("u1", next); n != 1 {
		t.Fatalf("attempts=%d want 1", n)
	}
	st, _ := p.Status("u1")
	if st.CheckinAttempts != 1 || !st.CheckinNextAt.Equal(next) {
		t.Fatalf("status=%+v want attempts=1 next=%v", st, next)
	}
	next2 := next.Add(time.Hour)
	if n := p.NoteCheckinAttempt("u1", next2); n != 2 {
		t.Fatalf("attempts=%d want 2", n)
	}
	// 未知 uid 不 panic、不计数。
	if n := p.NoteCheckinAttempt("no-such", next); n != 0 {
		t.Fatalf("未知 uid attempts=%d want 0", n)
	}
}

func TestCheckinAttemptPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	next := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	p.NoteCheckinAttempt("u1", next)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"checkin_attempts"`) {
		t.Fatalf("state.json 缺少 checkin_attempts:\n%s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, _ := p2.Status("u1")
	if st.CheckinAttempts != 1 || !st.CheckinNextAt.Equal(next) {
		t.Fatalf("重启后台账丢失: %+v", st)
	}
}

func TestCheckinAttemptResetsNextDay(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinAttempt("u1", time.Now().Add(time.Hour))
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	body := strings.Replace(string(raw), time.Now().Format("2006-01-02"), yesterday, 1)
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, _ := p2.Status("u1")
	if st.CheckinAttempts != 0 || !st.CheckinNextAt.IsZero() {
		t.Fatalf("跨天后应清零: %+v", st)
	}
}
