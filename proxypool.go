package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	poolFetchTimeout  = 20 * time.Second // 单个节点源链接的拉取超时
	poolProbeWorkers  = 32               // 并发探活协程数
	poolMaxCandidates = 400              // 每轮最多探活的新候选数
	poolMaxAutoSlots  = 300              // 自动入池的节点上限
	poolRetryBackoff  = 30 * time.Minute // 探活失败节点的重试间隔
)

// parsePoolSources 把多行/逗号分隔的节点源链接整理为去重后的 URL 列表。
func parsePoolSources(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })
	seen := make(map[string]struct{})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		u := normalizeSourceURL(field)
		if u == "" {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// normalizeSourceURL 清理链接：去片段、把 github blob 页面转成 raw 直链。
func normalizeSourceURL(raw string) string {
	u := strings.TrimSpace(raw)
	if i := strings.Index(u, "#"); i >= 0 {
		u = strings.TrimSpace(u[:i])
	}
	u = strings.TrimSuffix(u, "?plain=1")
	const blobPrefix = "https://github.com/"
	if strings.HasPrefix(u, blobPrefix) {
		rest := strings.TrimPrefix(u, blobPrefix)
		if i := strings.Index(rest, "/blob/"); i > 0 {
			rest = rest[:i] + "/" + rest[i+len("/blob/"):]
			u = "https://raw.githubusercontent.com/" + rest
		}
	}
	return u
}

// parsePoolLine 解析节点源里的一行，支持常见格式：
// socks5://user:pass@host:port#名称、socks5://host:port、host:port、host:port:user:pass。
func parsePoolLine(line string) (slot, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return slot{}, false
	}
	if i := strings.Index(line, "#"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return slot{}, false
	}
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "socks5://"), strings.HasPrefix(lower, "socks5h://"),
		strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		s, err := slotFromRawURL(line)
		return s, err == nil
	case strings.HasPrefix(lower, "socks://"):
		converted, err := convertSharedSOCKS(line)
		if err != nil {
			return slot{}, false
		}
		s, err := slotFromRawURL(converted)
		return s, err == nil
	default:
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			return slot{}, false
		}
		candidate := "socks5://" + line
		if len(parts) >= 4 {
			hostPort := parts[0] + ":" + parts[1]
			user := strings.Join(parts[2:len(parts)-1], ":")
			pass := parts[len(parts)-1]
			candidate = "socks5://" + url.UserPassword(user, pass).String() + "@" + hostPort
		}
		s, err := slotFromRawURL(candidate)
		return s, err == nil
	}
}

type poolProbeResult struct {
	slot    slot
	latency time.Duration
	ok      bool
}

// probeSlots 并发对候选节点做真实上游探活（经代理访问 opencode.ai）。
func probeSlots(ctx context.Context, g *gateway, items []slot) []poolProbeResult {
	results := make([]poolProbeResult, len(items))
	if len(items) == 0 {
		return results
	}
	sem := make(chan struct{}, poolProbeWorkers)
	var wg sync.WaitGroup
	for i, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			ok := g.probe(ctx, item)
			results[i] = poolProbeResult{slot: item, latency: time.Since(started), ok: ok}
		}()
	}
	wg.Wait()
	return results
}

// startPoolWatcher 周期性拉取节点源、探活并入池；失效节点自动移除。全程无需重启。
func (g *gateway) startPoolWatcher(ctx context.Context) {
	g.refreshPool(ctx)
	interval := g.cfg.refreshInterval
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.refreshPool(ctx)
		}
	}
}

// refreshPool 一轮完整的「拉取 → 探活 → 入池 / 剔除」流程。
func (g *gateway) refreshPool(ctx context.Context) {
	urls := g.cfg.poolURLs
	if len(urls) == 0 {
		return
	}

	existing := make(map[string]struct{})
	for _, addr := range g.slotAddresses(true) {
		existing[addr] = struct{}{}
	}

	candidates := g.fetchPoolSources(ctx, urls)
	var fresh []slot
	for _, s := range candidates {
		if _, dup := existing[s.addr]; dup {
			continue
		}
		if g.recentlyFailed(s.addr) {
			continue
		}
		fresh = append(fresh, s)
	}
	if len(fresh) > poolMaxCandidates {
		fresh = sampleSlots(fresh, poolMaxCandidates)
	}

	added := 0
	justAdded := make(map[string]struct{})
	for _, result := range probeSlots(ctx, g, fresh) {
		if result.slot.addr == "" {
			continue
		}
		if !result.ok {
			g.noteFailed(result.slot.addr)
			continue
		}
		if g.customCount() >= poolMaxAutoSlots {
			break
		}
		if g.addSlot(result.slot, true) {
			added++
			justAdded[result.slot.addr] = struct{}{}
			log.Printf("[池+] %s (%dms)", result.slot.addr, result.latency.Milliseconds())
		}
	}

	// 复检现有节点，剔除已失效的，保持池子新鲜。
	// 本轮刚入池的节点跳过复检——几秒前才探活通过，没必要立刻再测一遍。
	current := g.customSnapshot()
	var recheck []slot
	for _, s := range current {
		if _, skip := justAdded[s.addr]; skip {
			continue
		}
		recheck = append(recheck, s)
	}
	dead := 0
	for _, result := range probeSlots(ctx, g, recheck) {
		if result.slot.addr == "" {
			continue
		}
		if !result.ok && g.dropCustom(result.slot.addr) {
			dead++
			log.Printf("[池-] %s", result.slot.addr)
		}
	}

	log.Printf("[池] 源 %d 个 | 候选 %d 条 | 新增 %d | 移除 %d | 池中 %d",
		len(urls), len(candidates), added, dead, g.customCount())
}

// fetchPoolSources 逐个拉取节点源，自动识别 JSON（amux 风格）与纯文本列表。
func (g *gateway) fetchPoolSources(ctx context.Context, urls []string) []slot {
	client := &http.Client{Transport: controlTransport(poolFetchTimeout)}
	defer client.CloseIdleConnections()

	seen := make(map[string]struct{})
	var out []slot
	for _, raw := range urls {
		if ctx.Err() != nil {
			break
		}
		source := normalizeSourceURL(raw)
		if source == "" {
			continue
		}
		fetchCtx, cancel := context.WithTimeout(ctx, poolFetchTimeout)
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source, nil)
		if err != nil {
			cancel()
			log.Printf("[池] 源无效: %s", shortSource(source))
			continue
		}
		res, err := client.Do(req)
		if err != nil {
			cancel()
			log.Printf("[池] 拉取失败 %s: %v", shortSource(source), err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		status := res.StatusCode
		res.Body.Close()
		cancel()
		if readErr != nil || status < 200 || status >= 300 {
			log.Printf("[池] 拉取失败 %s: status=%d", shortSource(source), status)
			continue
		}
		count := appendPoolBody(&out, seen, body)
		log.Printf("[池] %s -> %d 条", shortSource(source), count)
	}
	return out
}

// appendPoolBody 解析单个源的响应体，返回解析出的条数。
func appendPoolBody(out *[]slot, seen map[string]struct{}, body []byte) int {
	trimmed := strings.TrimSpace(string(body))
	count := 0
	appendSlot := func(s slot, ok bool) {
		if !ok || s.addr == "" {
			return
		}
		if _, dup := seen[s.addr]; dup {
			return
		}
		seen[s.addr] = struct{}{}
		*out = append(*out, s)
		count++
	}

	if strings.HasPrefix(trimmed, "[") {
		var items []proxyItem
		if err := json.Unmarshal([]byte(trimmed), &items); err == nil {
			for _, item := range items {
				s, slotErr := slotFromProxy(item.Address, item.Protocol)
				appendSlot(s, slotErr == nil)
			}
			return count
		}
	}
	for _, line := range strings.Split(trimmed, "\n") {
		s, ok := parsePoolLine(line)
		appendSlot(s, ok)
	}
	return count
}

// recentlyFailed 报告节点是否在最近一轮失败冷却期内。
func (g *gateway) recentlyFailed(addr string) bool {
	g.poolFailedMu.Lock()
	defer g.poolFailedMu.Unlock()
	ts, ok := g.poolFailed[addr]
	if !ok {
		return false
	}
	if time.Since(ts) >= poolRetryBackoff {
		delete(g.poolFailed, addr)
		return false
	}
	return true
}

func (g *gateway) noteFailed(addr string) {
	g.poolFailedMu.Lock()
	defer g.poolFailedMu.Unlock()
	// 顺手清理过期项，避免 map 无限增长。
	now := time.Now()
	for key, ts := range g.poolFailed {
		if now.Sub(ts) >= poolRetryBackoff {
			delete(g.poolFailed, key)
		}
	}
	g.poolFailed[addr] = now
}

func (g *gateway) dropCustom(address string) bool {
	g.mu.Lock()
	removed := false
	for i, current := range g.custom {
		if current.addr == address {
			g.custom = append(g.custom[:i], g.custom[i+1:]...)
			removed = true
			break
		}
	}
	g.mu.Unlock()
	return removed
}

// noteCustomResult 记录节点在真实请求中的表现；连续 3 次失败自动移出池子，
// 避免死节点反复浪费重试预算。成功即清零。
func (g *gateway) noteCustomResult(address string, ok bool) {
	g.customFailsMu.Lock()
	defer g.customFailsMu.Unlock()
	if ok {
		delete(g.customFails, address)
		return
	}
	g.customFails[address]++
	if g.customFails[address] < 3 {
		return
	}
	delete(g.customFails, address)
	if g.dropCustom(address) {
		log.Printf("[池x] %s 连续失败，移出节点池", address)
	}
}

func (g *gateway) customSnapshot() []slot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]slot(nil), g.custom...)
}

// sampleSlots 等距抽样，避免单轮探活过多候选。
func sampleSlots(items []slot, limit int) []slot {
	if len(items) <= limit {
		return items
	}
	out := make([]slot, 0, limit)
	step := float64(len(items)) / float64(limit)
	for i := 0; i < limit; i++ {
		out = append(out, items[int(float64(i)*step)])
	}
	return out
}

func shortSource(source string) string {
	if len(source) <= 60 {
		return source
	}
	return source[:57] + "..."
}
