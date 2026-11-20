// checkin_window.go 签到窗口错峰。
//
// 背景（风控视角）：原实现是「整点全池同步 sweep」——所有账号在同一秒开始、
// 每天同一时刻、每天同一顺序（pool.List 按 UID 排序），再加每天固定两轮
// （第二轮 100% 被上游幂等拒绝）。这三条零方差比请求头指纹更容易被判定为
// 机器行为，而头指纹还能靠对齐官方客户端掩盖，时间特征不能。
//
// 本文件做四件事，且不改动其它任务族的整点排程：
//
//	① 窗口内每号一个偏移：由 (uid, 日期, 窗口小时) 确定性派生，重启不重抽；
//	② 顺序由哈希决定，不跟 UID 序（连续多天同一个排列本身是机器指纹）；
//	③ 账号间节流 2-9s，且任何时刻只有一个签到请求在途；
//	④ 已签账号不进计划——21 点窗口因此只补失败号，请求量从 2N 降到约 1N。
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 账号间节流下限/上限：签到是空 body 的短 RPC，节流只为打断「同一秒密集到达」
// 这个同源特征；不像旅行/活跃那样需要 800ms 固定间隔，随机区间本身就是去指纹。
// 变量而非常量：测试置 0 避免白等。
var (
	checkinAccountGapMin = 2 * time.Second
	checkinAccountGapMax = 9 * time.Second
)

// checkinDayCap 窗口的当日硬上限：留出末位账号与失败重试的余量，绝不跨零点
// （跨零点 = 当天漏签，连登会断）。
const checkinDayCapHour, checkinDayCapMinute = 23, 45

// checkinSlot 单账号在一个窗口内的计划时点。
type checkinSlot struct {
	uid   string
	nick  string
	at    time.Time
	order uint64 // 同一时刻的破平局键：哈希序，不是 UID 序
	// balanceOnly 企业版无签到体系（上游 400 code 10001），只保留余额查询——
	// 与 RunCheckinNow 的既有口径一致（企业额度是 credit_floor 判定的唯一数据源）。
	balanceOnly bool
}

// checkinGap 下一次发送前应等待的随机间隔。
func checkinGap() time.Duration {
	if checkinAccountGapMax <= checkinAccountGapMin {
		return checkinAccountGapMin
	}
	return checkinAccountGapMin + time.Duration(rand.Int63n(int64(checkinAccountGapMax-checkinAccountGapMin)))
}

// checkinKey 由 (uid, 日期, 窗口小时) 派生两个确定性数值：偏移与排序键。
// 用哈希而不是 math/rand：进程重启、多实例、单测都必须得到同一份计划，
// 否则重启会重抽时点、把「尚未签到」的号再打一遍上游。
func checkinKey(uid string, day time.Time, hour int) (off, ord uint64) {
	h := sha256.New()
	h.Write([]byte("wb2api/checkin-spread"))
	h.Write([]byte{0})
	h.Write([]byte(uid))
	h.Write([]byte{0})
	h.Write([]byte(day.Format("2006-01-02")))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(hour)))
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16])
}

// checkinSpread 当前窗口分散时长（热配置读，Reconfigure 后下一窗口生效）。
func (s *Scheduler) checkinSpread() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.CheckinSpread
}

// SetCheckinSpread 热更新签到窗口分散时长（面板保存 schedule.checkin_spread_minutes）。
// 只影响下一个窗口：正在执行的窗口按已算好的计划跑完，避免中途改计划造成漏签。
func (s *Scheduler) SetCheckinSpread(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	s.cfg.CheckinSpread = d
	s.schedMu.Unlock()
}

// checkinPlan 计算某窗口的账号计划（按计划时点升序）。
//
// 门控与 RunCheckinNow 逐条对齐：禁用号（除非 include_disabled_in_tasks）、
// 无凭证、global 跳过；企业版只查余额；当日已签的普通号跳过。
// 偏移截断在 23:45 之前，保证不跨自然日。
func (s *Scheduler) checkinPlan(start time.Time) []checkinSlot {
	if s.cfg.Pool == nil {
		return nil
	}
	spread := s.checkinSpread()
	limit := time.Date(start.Year(), start.Month(), start.Day(), checkinDayCapHour, checkinDayCapMinute, 0, 0, start.Location())
	if start.Add(spread).After(limit) {
		spread = limit.Sub(start)
		if spread < 0 {
			spread = 0
		}
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
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 签到体系，不发起任何上游调用
		}
		sl := checkinSlot{uid: st.UID, nick: st.Nickname}
		hOff, hOrd := checkinKey(st.UID, start, start.Hour())
		sl.order = hOrd
		if a.IsEnterprise() {
			sl.balanceOnly = true
		} else if st.CheckinDone {
			continue // 本日已签：21 点窗口只补失败号，不再全池空跑
		}
		if spread > 0 {
			sl.at = start.Add(time.Duration(hOff % uint64(spread)))
		} else {
			sl.at = start
		}
		out = append(out, sl)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].order < out[j].order
	})
	return out
}

// waitUntil 分段墙钟等待到 at。与 waitSlot 同因：定时器基于单调时钟，
// 睡眠会把它冻结，分段醒来用墙钟重判可把影响限制在一段之内。
// 返回 false 表示 ctx 取消，调用方应放弃本窗口剩余账号。
func waitUntil(ctx context.Context, at time.Time) bool {
	for {
		remain := time.Until(at)
		if remain <= 0 {
			return ctx.Err() == nil
		}
		step := remain
		if step > wallclockCheckStep {
			step = wallclockCheckStep
		}
		timer := time.NewTimer(step)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// runCheckinWindow 执行一个窗口：按计划时点逐个签到，账号间保持随机间隔。
// 同一时刻只允许一个窗口在跑（TryLock）：9 点窗口若因配置过大仍在进行，
// 21 点窗口直接跳过而不是叠加并发——未签号本来就在 9 点计划里。
func (s *Scheduler) runCheckinWindow(ctx context.Context, start time.Time) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	if !s.checkinRun.TryLock() {
		log.Printf("checkin 窗口 %s: 上一轮仍在执行，跳过本轮（未签号等下一时点）", start.Format("15:04"))
		return
	}
	defer s.checkinRun.Unlock()

	plan := s.checkinPlan(start)
	if len(plan) == 0 {
		return
	}
	log.Printf("checkin 窗口 %s: %d 个账号错峰签到，最晚 %s",
		start.Format("15:04"), len(plan), plan[len(plan)-1].at.Format("15:04"))

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
		log.Printf("checkin-plan %s at=%s", logfmt.Label(sl.uid, sl.nick), sl.at.Format("15:04"))
		if sl.balanceOnly {
			s.refreshBalanceFor(sl.uid, "user-resource")
			continue
		}
		if s.checkinOne(st) {
			s.streakFor(sl.uid)
		}
	}
}

// checkinOne 单账号签到 + 余额快照，返回该号当日是否已签（成功或上游幂等拒绝）。
// 企业版跳过签到但照常查余额（与 RunCheckinNow 同口径）。
func (s *Scheduler) checkinOne(st pool.Status) bool {
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil {
		return false
	}
	done := false
	if !a.IsEnterprise() {
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
			if upstream.IsAlreadyCheckin(err) {
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(st.UID, st.Nickname))
				done = true
			} else {
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			s.cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
			done = true
		}
	}
	s.refreshBalanceFor(st.UID, "user-resource")
	return done
}

// streakFor 单账号连登管家（兑换已解锁档位 + 抽完抽奖次数）。
// 门控与 RunStreakBonusNow 一致：禁用号不跑（include_disabled_in_tasks 只覆盖
// 签到/活跃/保活/余额四类），global 与企业切换会话无成长体系。
func (s *Scheduler) streakFor(uid string) {
	if st, ok := s.cfg.Pool.Status(uid); ok && st.Disabled {
		return
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	if a.IsGlobal() || a.IsEnterpriseSession() {
		return
	}
	s.streakBonusAccount(a)
}

// refreshBalanceFor 单账号余额刷新 + 冷却解冻（签到后与后台刷新共用）。
func (s *Scheduler) refreshBalanceFor(uid, tag string) {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return
	}
	expiringSoon := s.ExpiringSoonWindow()
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := s.cfg.Upstream.UserResourceSnapshot(a, expiringSoon)
	if err != nil {
		log.Printf("%s %s: %v", tag, logfmt.Label(uid, a.Nickname), err)
		return
	}
	s.cfg.Pool.ReenableIfCredits(uid, remain, total)
	s.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
}
