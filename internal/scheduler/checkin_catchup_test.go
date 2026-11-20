// checkin_catchup_test.go 钉住每日兜底的四条行为：
//
//	① 只补未签号（已签不再打上游）；
//	② 尝试台账生效：退避期内不重试、次数达上限后停手；
//	③ 23 点「最后一班车」忽略退避与上限（当日最后一次补救机会）；
//	④ 补签在 00:05–23:30 之外不自动触发，且 ctx 取消即收尾。
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func newCatchupScheduler(t *testing.T, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		addCheckinAuth(p, uid)
	}
	return New(Config{Pool: p}), p
}

// TestCatchupPlanSkipsDoneAccounts 已签号不进补签计划。
func TestCatchupPlanSkipsDoneAccounts(t *testing.T) {
	s, p := newCatchupScheduler(t, "u1", "u2")
	p.NoteCheckinDone("u1")

	plan := s.catchupPlan(time.Now(), false)
	if len(plan) != 1 || plan[0].uid != "u2" {
		t.Fatalf("plan=%+v want only u2", plan)
	}
}

// TestCatchupPlanHonorsBackoff 退避未到期不进计划；到期后恢复。
func TestCatchupPlanHonorsBackoff(t *testing.T) {
	s, p := newCatchupScheduler(t, "u1")
	now := time.Now()
	p.NoteCheckinAttempt("u1", now.Add(30*time.Minute))

	if plan := s.catchupPlan(now, false); len(plan) != 0 {
		t.Fatalf("退避期内不应补签: %+v", plan)
	}
	if plan := s.catchupPlan(now.Add(31*time.Minute), false); len(plan) != 1 {
		t.Fatalf("退避到期后应恢复补签: %+v", plan)
	}
}

// TestCatchupPlanCapsAttempts 次数达上限后普通轮次停手，最后一班车仍尝试一次。
func TestCatchupPlanCapsAttempts(t *testing.T) {
	s, p := newCatchupScheduler(t, "u1")
	now := time.Now()
	for i := 0; i < checkinCatchupMaxAttempts; i++ {
		p.NoteCheckinAttempt("u1", now.Add(-time.Minute))
	}

	if plan := s.catchupPlan(now, false); len(plan) != 0 {
		t.Fatalf("达到上限后普通轮次不应再试: %+v", plan)
	}
	if plan := s.catchupPlan(now, true); len(plan) != 1 {
		t.Fatalf("最后一班车应无视上限再试一次: %+v", plan)
	}
}

// TestCatchupPlanRespectsCheckinDisabled 显式关闭自动签到时，补签也不许补。
func TestCatchupPlanRespectsCheckinDisabled(t *testing.T) {
	s, _ := newCatchupScheduler(t, "u1")
	s.cfg.CheckinDisabled = true
	if plan := s.catchupPlan(time.Now(), false); len(plan) != 0 {
		t.Fatalf("关闭签到后不应补签: %+v", plan)
	}
}

// TestCatchupPlanSkipsEnterpriseAndDisabled 企业版无签到体系；禁用号默认跳过。
func TestCatchupPlanSkipsEnterpriseAndDisabled(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "ent", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "e1"})
	addCheckinAuth(p, "dis")
	addCheckinAuth(p, "ok")
	p.Disable("dis", "manual")
	s := New(Config{Pool: p})

	plan := s.catchupPlan(time.Now(), false)
	if len(plan) != 1 || plan[0].uid != "ok" {
		t.Fatalf("plan=%+v want only ok", plan)
	}
}

// TestCatchupChecksPendingOnlyAndBacksOff 端到端：只打未签号、记账、失败后退避、
// 最后一班车补一次。
func TestCatchupChecksPendingOnlyAndBacksOff(t *testing.T) {
	fastCheckinGap(t)
	stub := &checkinStub{}
	s, p := stub.newScheduler(t)
	p.NoteCheckinDone("u1") // u1 今日已签；u2 待签（且上游失败）

	now := time.Now()
	s.runCatchupAt(context.Background(), now, false)
	if got := stub.ok.Load(); got != 0 {
		t.Fatalf("已签号被重复签到：ok=%d want 0", got)
	}
	if got := stub.bad.Load(); got != 1 {
		t.Fatalf("待签号应补一次：bad=%d want 1", got)
	}
	st, _ := p.Status("u2")
	if st.CheckinAttempts != 1 || st.CheckinNextAt.IsZero() {
		t.Fatalf("补签后应记账并设置退避: %+v", st)
	}
	if st.CheckinDone {
		t.Fatal("失败不应标记今日已签")
	}

	// 退避未到期：同一时刻再跑不应重试。
	s.runCatchupAt(context.Background(), now, false)
	if got := stub.bad.Load(); got != 1 {
		t.Fatalf("退避期内重试了：bad=%d want 1", got)
	}
	// 最后一班车：忽略退避再试一次。
	s.runCatchupAt(context.Background(), now, true)
	if got := stub.bad.Load(); got != 2 {
		t.Fatalf("最后一班车应重试一次：bad=%d want 2", got)
	}
}

// TestCatchupStopsOnCancel ctx 取消即收尾，不发起任何签到。
func TestCatchupStopsOnCancel(t *testing.T) {
	fastCheckinGap(t)
	stub := &checkinStub{}
	s, _ := stub.newScheduler(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runCatchupAt(ctx, time.Now(), false)
	if got := stub.ok.Load() + stub.bad.Load(); got != 0 {
		t.Fatalf("已取消仍发起签到：calls=%d want 0", got)
	}
}

// TestStartCheckinCatchupStopsOnCancel 补签循环随 ctx 取消立即退出，不等下一个 tick。
func TestStartCheckinCatchupStopsOnCancel(t *testing.T) {
	old := checkinStartupDelay
	checkinStartupDelay = 0
	t.Cleanup(func() { checkinStartupDelay = old })

	s := New(Config{CheckinCatchup: time.Hour}) // Pool 为 nil：首轮直接返回，不发起请求
	ctx, cancel := context.WithCancel(context.Background())
	s.StartCheckinCatchup(ctx)
	cancel()

	done := make(chan struct{})
	go func() { s.checkinCatchupWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("补签循环未随 ctx 取消退出")
	}
}

// TestCatchupNotStartedWhenIntervalZero interval<=0（=兜底关闭）时不启动循环。
func TestCatchupNotStartedWhenIntervalZero(t *testing.T) {
	s := New(Config{})
	s.StartCheckinCatchup(context.Background())

	// WaitGroup 没有计数读取接口，用「Wait 立即返回」证明没有 goroutine 起过。
	done := make(chan struct{})
	go func() { s.checkinCatchupWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interval=0 不应启动补签循环")
	}
}

// TestCatchupAllowedAt 自动补签只在 00:05–23:30 之间触发，避免跨零点。
func TestCatchupAllowedAt(t *testing.T) {
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local)
	cases := []struct {
		h, m int
		want bool
	}{
		{0, 4, false},
		{0, 5, true},
		{12, 0, true},
		{23, 29, true},
		{23, 30, false},
	}
	for _, c := range cases {
		at := day.Add(time.Duration(c.h)*time.Hour + time.Duration(c.m)*time.Minute)
		if got := catchupAllowedAt(at); got != c.want {
			t.Errorf("%02d:%02d -> %v want %v", c.h, c.m, got, c.want)
		}
	}
}

// TestIsCheckinLastCall 23 点整进入最后一班车，23:30 截止。
func TestIsCheckinLastCall(t *testing.T) {
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.Local)
	for _, c := range []struct {
		h, m int
		want bool
	}{{22, 59, false}, {23, 0, true}, {23, 29, true}, {23, 30, false}} {
		at := day.Add(time.Duration(c.h)*time.Hour + time.Duration(c.m)*time.Minute)
		if got := isCheckinLastCall(at); got != c.want {
			t.Errorf("%02d:%02d -> %v want %v", c.h, c.m, got, c.want)
		}
	}
}
