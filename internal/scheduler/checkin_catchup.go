// checkin_catchup.go 每日补签兜底。
//
// 背景：窗口排程（checkin_window.go）只保证「进程在签到时点活着」时的尝试；
// 下面三种情况仍会漏签——时点已过才启动（容器重启/升级）、最后一轮全部失败、
// 周期内新增账号。补签循环把这些补上：
//
//	① 启动后 60s 先补一轮（覆盖重启窗口错过）；
//	② 之后每 catchup_minutes（默认 30）扫一次未签号，按 2–9s 节流逐个补；
//	③ 23 点「最后一班车」忽略退避与次数上限，再做当日最后一次补救。
//
// 台账（次数 + 下次允许时刻）落在 pool 里并持久化：凭证已死的号不会被无限重试
// 打成上游轰炸，崩溃重启也不会把「每日上限」重置成无限。
//
// 仍然无法覆盖：进程整天不在。那是可用性问题，只能靠进程外触发器
// （POST /panel/api/checkin_all?pending=1，见 README）。
package scheduler

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

const (
	// checkinCatchupMaxAttempts 普通轮次的每日尝试上限（不含配置时点的正常窗口）。
	// 超过后只在 23 点最后一班车再试一次：上限是防轰炸，不是放弃当天。
	checkinCatchupMaxAttempts = 3
	// checkinCatchupStartMinute 自动补签的最早时刻（00:05）：刚过零点就签，
	// 与上游按自然日重置的语义一致，同时避开零点整的重置竞态。
	checkinCatchupStartMinute = 5
	// checkinCatchupStopHour/Minute 自动补签的截止时刻（23:30）：留出执行与
	// 重试余量，绝不跨零点（跨零点 = 当天漏签）。
	checkinCatchupStopHour   = 23
	checkinCatchupStopMinute = 30
	// checkinCatchupLastCallHour 最后一班车：23:00–23:29 之间忽略退避与上限。
	checkinCatchupLastCallHour = 23
)

// checkinCatchupBackoffs 第 n 次尝试后的等待时长（n 从 0 起，超出取末项）。
var checkinCatchupBackoffs = []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour}

// checkinStartupDelay 启动后到第一次补签的等待：留时间让凭证加载、网络/DNS 就绪
// （与 issue #152 的唤醒宽限同因）。测试可置 0。
var checkinStartupDelay = 60 * time.Second

// catchupAllowedAt 自动补签是否允许在当前时刻触发。
func catchupAllowedAt(now time.Time) bool {
	m := now.Hour()*60 + now.Minute()
	return m >= checkinCatchupStartMinute && m < checkinCatchupStopHour*60+checkinCatchupStopMinute
}

// isCheckinLastCall 23:00–23:29：当日最后一班车。
func isCheckinLastCall(now time.Time) bool {
	return catchupAllowedAt(now) && now.Hour() == checkinCatchupLastCallHour
}

// checkinCatchupBackoff 第 prev 次尝试之后应等待多久（prev = 已完成的次数）。
func checkinCatchupBackoff(prev int) time.Duration {
	if prev < 0 {
		prev = 0
	}
	if prev >= len(checkinCatchupBackoffs) {
		return checkinCatchupBackoffs[len(checkinCatchupBackoffs)-1]
	}
	return checkinCatchupBackoffs[prev]
}

// catchupPlan 计算一轮补签计划：只含未签、未被退避挡住、未达次数上限的普通号。
// lastCall=true 时忽略退避与上限（最后一班车），但仍排除已签与企业版/国际版。
func (s *Scheduler) catchupPlan(now time.Time, lastCall bool) []checkinSlot {
	if s.cfg.Pool == nil {
		return nil
	}
	// 用户显式关闭自动签到时，补签也不能签（否则「关闭」形同虚设）。
	s.schedMu.Lock()
	off := s.cfg.CheckinDisabled
	s.schedMu.Unlock()
	if off {
		return nil
	}
	includeDisabled := s.includeDisabledInTasks()
	var out []checkinSlot
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 企业版无签到体系（上游 400 code 10001），余额走 5 分钟后台刷新；
		// global 无 CN 签到体系。两者都不该进补签计划。
		if a.IsGlobal() || a.IsEnterprise() {
			continue
		}
		if st.CheckinDone {
			continue
		}
		if !lastCall {
			if st.CheckinAttempts >= checkinCatchupMaxAttempts {
				continue
			}
			if st.CheckinNextAt.After(now) {
				continue
			}
		}
		// 顺序由哈希决定，不跟 UID 序（与窗口排程同口径）；时点统一为 now，
		// 账号间隔靠执行期的 2–9s 节流。
		_, ord := checkinKey(st.UID, now, now.Hour())
		out = append(out, checkinSlot{uid: st.UID, nick: st.Nickname, at: now, order: ord})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

// runCatchupAt 执行一轮补签。now 与 lastCall 显式传入以便测试钉住时钟。
// 与签到窗口共用 checkinRun 单飞：窗口正在错峰时不插队，下一轮再说。
func (s *Scheduler) runCatchupAt(ctx context.Context, now time.Time, lastCall bool) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	plan := s.catchupPlan(now, lastCall)
	if len(plan) == 0 {
		return
	}
	if !s.checkinRun.TryLock() {
		log.Printf("checkin 补签: 签到窗口执行中，本次跳过（%d 个未签）", len(plan))
		return
	}
	defer s.checkinRun.Unlock()

	tag := ""
	if lastCall {
		tag = "（最后一班车）"
	}
	log.Printf("checkin 补签%s: %d 个未签账号", tag, len(plan))

	var last time.Time
	for _, sl := range plan {
		if !waitUntil(ctx, sl.at) {
			return
		}
		if !last.IsZero() {
			if !waitUntil(ctx, last.Add(checkinGap())) {
				return
			}
		}
		st, ok := s.cfg.Pool.Status(sl.uid)
		if !ok {
			continue
		}
		last = time.Now()
		next := last.Add(checkinCatchupBackoff(st.CheckinAttempts))
		n := s.cfg.Pool.NoteCheckinAttempt(sl.uid, next)
		log.Printf("checkin 补签 %s: 第 %d 次尝试（下次允许 %s）",
			logfmt.Label(sl.uid, sl.nick), n, next.Format("15:04"))
		if s.checkinOne(st) {
			s.streakFor(sl.uid)
		}
	}
}

// RunCheckinCatchupNow 立即补一轮未签号（面板/外部 cron 触发）。
//
// 显式动作不看 00:05–23:30 的自动窗口，但仍受退避与次数上限约束；
// 23 点后按「最后一班车」处理（忽略退避），23:45 之后不再发起——那会把一次
// 签到推到零点之后，上游记账落到次日、本地却记今天，凭空制造一次漏签。
func (s *Scheduler) RunCheckinCatchupNow() {
	now := time.Now()
	if now.Hour() == checkinDayCapHour && now.Minute() >= checkinDayCapMinute {
		log.Printf("checkin 补签: 已过 %02d:%02d，不再发起（避免跨零点记账）", checkinDayCapHour, checkinDayCapMinute)
		return
	}
	s.runCatchupAt(context.Background(), now, isCheckinLastCall(now))
}

// StartCheckinCatchup 启动补签循环（interval<=0 不启动）。启动先等
// checkinStartupDelay 再补第一轮；之后按 interval 轮询，可用
// SetCheckinCatchup 热改（下一轮生效）。
func (s *Scheduler) StartCheckinCatchup(ctx context.Context) {
	if s.catchupEvery() <= 0 {
		return
	}
	s.checkinCatchupWg.Add(1)
	go func() {
		defer s.checkinCatchupWg.Done()
		if !sleepCtx(ctx, checkinStartupDelay) {
			return
		}
		for {
			now := time.Now()
			if catchupAllowedAt(now) {
				s.runCatchupAt(ctx, now, isCheckinLastCall(now))
			}
			every := s.catchupEvery()
			if every <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmCatchup:
					continue
				}
			}
			timer := time.NewTimer(every)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmCatchup:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
			}
		}
	}()
}

// catchupEvery 当前补签轮询间隔（0 = 暂停）。
func (s *Scheduler) catchupEvery() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.CheckinCatchup
}

// SetCheckinCatchup 热更新补签轮询间隔（面板保存 schedule.checkin_catchup_*）。
func (s *Scheduler) SetCheckinCatchup(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	s.cfg.CheckinCatchup = d
	s.schedMu.Unlock()
	poke(s.rearmCatchup)
}
