// checkin_window_test.go 钉住「签到窗口内错峰」的四条行为：
//
//	① 当日已签的账号不进窗口计划（21 点窗口因此天然只补失败号，不再全池空跑一轮）；
//	② 窗口内偏移由 (uid, 日期, 窗口小时) 确定性派生——重启不重抽、跨天必须变；
//	③ 偏移落在窗口内且不跨自然日；
//	④ 执行顺序不跟随 UID 序（否则连续多天同一个排列就是机器指纹）。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fastCheckinGap 关闭账号间节流，避免测试白等 2-9s。
func fastCheckinGap(t *testing.T) {
	t.Helper()
	oldMin, oldMax := checkinAccountGapMin, checkinAccountGapMax
	checkinAccountGapMin, checkinAccountGapMax = 0, 0
	t.Cleanup(func() { checkinAccountGapMin, checkinAccountGapMax = oldMin, oldMax })
}

func addCheckinAuth(p *pool.Pool, uid string) {
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
}

// TestCheckinPlanSkipsDoneAccounts 已签账号不进计划：21 点窗口只补失败号。
func TestCheckinPlanSkipsDoneAccounts(t *testing.T) {
	p := pool.New("")
	addCheckinAuth(p, "u1")
	addCheckinAuth(p, "u2")
	p.NoteCheckinDone("u1")
	s := New(Config{Pool: p})

	plan := s.checkinPlan(time.Date(2026, 11, 20, 21, 0, 0, 0, time.Local))
	if len(plan) != 1 || plan[0].uid != "u2" {
		t.Fatalf("plan=%+v want only u2（已签号不得再打上游）", plan)
	}
}

// TestCheckinPlanDeterministicPerDay 同一 (uid, 日期, 窗口) 恒得同一时点；
// 跨天必须变化——固定时刻正是要消灭的旧特征。
func TestCheckinPlanDeterministicPerDay(t *testing.T) {
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		addCheckinAuth(p, uid)
	}
	s := New(Config{Pool: p, CheckinSpread: 2 * time.Hour})
	start := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)

	a := s.checkinPlan(start)
	b := s.checkinPlan(start)
	if len(a) != len(b) || len(a) != 3 {
		t.Fatalf("plan sizes a=%d b=%d want 3", len(a), len(b))
	}
	for i := range a {
		if a[i].uid != b[i].uid || !a[i].at.Equal(b[i].at) {
			t.Fatalf("重启后计划漂移：%v vs %v", a[i], b[i])
		}
	}

	c := s.checkinPlan(start.AddDate(0, 0, 1))
	same := len(c) == len(a)
	if same {
		for i := range c {
			if !c[i].at.Equal(a[i].at) {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatalf("次日计划与当日完全相同，缺少日间抖动: %+v", c)
	}
}

// TestCheckinPlanOffsetsWithinSpread 偏移落在 [窗口起点, 窗口起点+spread)。
func TestCheckinPlanOffsetsWithinSpread(t *testing.T) {
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		addCheckinAuth(p, uid)
	}
	const spread = 45 * time.Minute
	s := New(Config{Pool: p, CheckinSpread: spread})
	start := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)

	plan := s.checkinPlan(start)
	if len(plan) != 5 {
		t.Fatalf("plan size=%d want 5", len(plan))
	}
	for _, sl := range plan {
		if sl.at.Before(start) || !sl.at.Before(start.Add(spread)) {
			t.Fatalf("slot %v 越出窗口 [%v, %v)", sl.at, start, start.Add(spread))
		}
	}
}

// TestCheckinPlanClampsToSameDay 大 spread 不得把签到推过当日（跨零点即漏签）。
func TestCheckinPlanClampsToSameDay(t *testing.T) {
	p := pool.New("")
	addCheckinAuth(p, "u1")
	s := New(Config{Pool: p, CheckinSpread: 6 * time.Hour})
	start := time.Date(2026, 11, 20, 23, 30, 0, 0, time.Local)

	got := s.checkinPlan(start)[0].at
	if got.Day() != start.Day() || got.Month() != start.Month() {
		t.Fatalf("slot=%v 跨出当日（start=%v）", got, start)
	}
	if limit := start.Add(15 * time.Minute); got.After(limit) {
		t.Fatalf("slot=%v 晚于当日硬上限 23:45（%v）", got, limit)
	}
}

// TestCheckinPlanOrderNotUID 同一时点时顺序由哈希决定，不跟 UID 序。
func TestCheckinPlanOrderNotUID(t *testing.T) {
	uids := []string{"u01", "u02", "u03", "u04", "u05", "u06", "u07", "u08"}
	p := pool.New("")
	for _, uid := range uids {
		addCheckinAuth(p, uid)
	}
	s := New(Config{Pool: p}) // spread=0：全部同一时刻，只剩排序可区分

	plan := s.checkinPlan(time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local))
	var got []string
	for _, sl := range plan {
		got = append(got, sl.uid)
	}
	want := append([]string(nil), uids...)
	sort.Strings(want)
	if reflect.DeepEqual(got, want) {
		t.Fatalf("签到顺序等于 UID 序（跨天可识别）: %v", got)
	}
}

// TestCheckinPlanGatesDisabledAndEnterprise 禁用号跳过；企业版只保留余额查询
// （上游对企业号签到恒 400 code 10001）。
func TestCheckinPlanGatesDisabledAndEnterprise(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "ent", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "e1"})
	addCheckinAuth(p, "dis")
	addCheckinAuth(p, "ok")
	p.Disable("dis", "manual")
	s := New(Config{Pool: p})

	plan := s.checkinPlan(time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local))
	byUID := map[string]checkinSlot{}
	for _, sl := range plan {
		byUID[sl.uid] = sl
	}
	if _, ok := byUID["dis"]; ok {
		t.Fatalf("禁用号不应进计划: %+v", plan)
	}
	ent, ok := byUID["ent"]
	if !ok || !ent.balanceOnly {
		t.Fatalf("企业版应只查余额: %+v", byUID)
	}
	if ok2, ok := byUID["ok"]; !ok || ok2.balanceOnly {
		t.Fatalf("普通号应正常签到: %+v", byUID)
	}
}

// checkinStub billing 桩：ok 号成功，bad 号业务失败（403 + 信封 = 不重试）。
type checkinStub struct {
	ok  atomic.Int32
	bad atomic.Int32
}

func (s *checkinStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if strings.Contains(r.Header.Get("Authorization"), "bad") {
				s.bad.Add(1)
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"code":11140,"msg":"request illegal"}`))
				return
			}
			s.ok.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *checkinStub) newScheduler(t *testing.T) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at-ok", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at-bad", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestCheckinWindowSecondPassOnlyRetriesFailed 第二轮只补失败号：
// 已签成功的账号当天不得再出现第二次 daily-checkin 调用。
func TestCheckinWindowSecondPassOnlyRetriesFailed(t *testing.T) {
	fastCheckinGap(t)
	stub := &checkinStub{}
	s, p := stub.newScheduler(t)

	now := time.Now()
	// 两个窗口都从「过去」发起：窗口 runner 按计划时点等待，未来时点会真的等到点
	// （那正是生产行为）。这里要验的是「第二轮只补失败号」，与等待无关。
	start := now.Add(-3 * time.Hour)
	s.runCheckinWindow(context.Background(), start)
	if got := stub.ok.Load(); got != 1 {
		t.Fatalf("首轮成功次数=%d want 1", got)
	}
	if got := stub.bad.Load(); got != 1 {
		t.Fatalf("首轮失败次数=%d want 1", got)
	}
	if st, _ := p.Status("u1"); !st.CheckinDone {
		t.Fatal("成功号应标记今日已签")
	}
	if st, _ := p.Status("u2"); st.CheckinDone {
		t.Fatal("失败号不得标记已签")
	}

	s.runCheckinWindow(context.Background(), start.Add(time.Hour)) // 第二轮窗口（仍在过去）
	if got := stub.ok.Load(); got != 1 {
		t.Fatalf("已签号被重复签到：ok=%d want 1", got)
	}
	if got := stub.bad.Load(); got != 2 {
		t.Fatalf("失败号应重试一次：bad=%d want 2", got)
	}
}

// TestRunBatchDoesNotWaitForCheckinWindow 窗口内错峰会持续很久，
// runBatch 必须立即返回，否则主循环会错过同日的活跃/保活时点。
func TestRunBatchDoesNotWaitForCheckinWindow(t *testing.T) {
	fastCheckinGap(t)
	stub := &checkinStub{}
	s, _ := stub.newScheduler(t)
	s.cfg.CheckinSpread = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runBatch(ctx, time.Now(), []taskKind{taskCheckin})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("runBatch 被签到窗口阻塞（会把后续时点一起拖住）")
	}
	cancel() // 停掉仍在错峰等待的窗口 goroutine
	s.checkinWg.Wait()
}

// TestCheckinWindowStopsOnCancel ctx 取消时窗口立即收尾，不等错峰睡满。
func TestCheckinWindowStopsOnCancel(t *testing.T) {
	fastCheckinGap(t)
	stub := &checkinStub{}
	s, _ := stub.newScheduler(t)
	s.cfg.CheckinSpread = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runCheckinWindow(ctx, time.Now())
	if got := stub.ok.Load() + stub.bad.Load(); got != 0 {
		t.Fatalf("已取消的窗口仍发起签到：calls=%d want 0", got)
	}
}
