package main

import (
	"sort"
	"sync"
	"time"
)

// exitTracker 记录每个出口的近期表现（首字节延迟、失败连击、空流截断），
// 供对冲竞速的出发排序与坐板凳决策使用。手动节点也参与记账：连续失败只会
// 被暂停参与竞速（坐板凳），永远不会被删除；探活通过或竞速胜出即恢复在场。
type exitTracker struct {
	mu    sync.Mutex
	stats map[string]*exitStat
}

type exitStat struct {
	latency     time.Duration // 首字节 EWMA（探活与真实胜出共同贡献）
	seen        bool          // 是否已有延迟样本
	failStreak  int           // 连续真实失败（传输错误/空流）；限流 429 不计入
	benchLevel  int           // 坐板凳时长阶梯索引
	benchUntil  time.Time     // 坐板凳截止时间；零值表示在场
	truncations int           // 累计空流/中途断流次数
}

// 坐板凳时长阶梯：连续失败次数越多，暂停越久，死节点占用逐步趋近于零。
var benchDurations = [...]time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

const (
	benchFailLimit = 3   // 连续真实失败达到该次数触发坐板凳
	latencyAlpha   = 0.5 // EWMA 平滑系数
	statsHardCap   = 8192
)

func newExitTracker() *exitTracker {
	return &exitTracker{stats: make(map[string]*exitStat)}
}

func (t *exitTracker) statLocked(addr string) *exitStat {
	s := t.stats[addr]
	if s == nil {
		s = &exitStat{}
		t.stats[addr] = s
	}
	return s
}

// observeWin 记录一次成功（探活通过或竞速胜出）：更新延迟 EWMA、清零失败
// 连击并立即解除坐板凳——成功本身就是最好的健康证明。
func (t *exitTracker) observeWin(addr string, latency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.statLocked(addr)
	if !s.seen {
		s.latency = latency
		s.seen = true
	} else {
		s.latency += time.Duration(latencyAlpha * float64(latency-s.latency))
	}
	s.failStreak = 0
	s.benchUntil = time.Time{}
	s.benchLevel = 0
}

// observeSuccess 记录一次流完整结束（见到终止标记）：只清失败连击，不采样延迟。
func (t *exitTracker) observeSuccess(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statLocked(addr).failStreak = 0
}

// observeFail 记录一次真实失败（传输错误/探活不通过）；限流 429 说明节点
// 连通只是被限，不进此函数。连续 benchFailLimit 次触发坐板凳。
func (t *exitTracker) observeFail(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failLocked(t.statLocked(addr))
}

// observeTruncation 记一次空流/中途断流：截断是最差信号，计入截断计数
// 并按失败连击处理。
func (t *exitTracker) observeTruncation(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.statLocked(addr)
	s.truncations++
	t.failLocked(s)
}

func (t *exitTracker) failLocked(s *exitStat) {
	s.failStreak++
	if s.failStreak >= benchFailLimit {
		if s.benchLevel < len(benchDurations) {
			s.benchLevel++
		}
		s.benchUntil = time.Now().Add(benchDurations[min(s.benchLevel, len(benchDurations))-1])
		s.failStreak = 0
	}
}

// observeProbe 汇报一次探活结果。
func (t *exitTracker) observeProbe(addr string, latency time.Duration, ok bool) {
	if ok {
		t.observeWin(addr, latency)
		return
	}
	t.observeFail(addr)
}

func (t *exitTracker) benched(addr string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stats[addr]
	return s != nil && time.Now().Before(s.benchUntil)
}

// filterBenched 剔除坐板凳的出口；全部坐板凳时原样返回——宁可带伤上场
// 也不能让竞速一个出口都没有。
func (t *exitTracker) filterBenched(exits []slot) []slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	kept := make([]slot, 0, len(exits))
	for _, candidate := range exits {
		s := t.stats[candidate.addr]
		if s == nil || now.After(s.benchUntil) {
			kept = append(kept, candidate)
		}
	}
	if len(kept) == 0 {
		return exits
	}
	return kept
}

// rank 把出口按近期表现排序，排序结果即对冲竞速的出发顺序：
// 截断少者优先 > 首字节快者优先 > 未知样本者居后（稳定排序保留原顺序）。
func (t *exitTracker) rank(exits []slot) []slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	// 防止节点池长期轮换导致记账无限增长：超限时只保留本次在场的出口。
	if len(t.stats) > statsHardCap {
		fresh := make(map[string]*exitStat, len(exits))
		for _, candidate := range exits {
			if s := t.stats[candidate.addr]; s != nil {
				fresh[candidate.addr] = s
			}
		}
		t.stats = fresh
	}
	type item struct {
		exit   slot
		trunc  int
		lat    time.Duration
		known  bool
	}
	items := make([]item, 0, len(exits))
	for _, candidate := range exits {
		s := t.stats[candidate.addr]
		if s == nil {
			items = append(items, item{exit: candidate})
			continue
		}
		items = append(items, item{exit: candidate, trunc: s.truncations, lat: s.latency, known: s.seen})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].trunc != items[j].trunc {
			return items[i].trunc < items[j].trunc
		}
		if items[i].known != items[j].known {
			return items[i].known
		}
		return items[i].known && items[i].lat < items[j].lat
	})
	out := make([]slot, 0, len(items))
	for _, it := range items {
		out = append(out, it.exit)
	}
	return out
}

// trackableExit 判断标识是否为可记账的代理出口（排除直连/本地等伪出口名）。
func trackableExit(addr string) bool {
	switch addr {
	case "", "direct", "直连", "ZenProxy", "local", "unknown":
		return false
	}
	return true
}
