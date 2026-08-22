package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errAttemptTimeout = errors.New("代理首字节超时")
	errRequestTimeout = errors.New("请求总超时")
	errNoProxy        = errors.New("没有可用代理")
)

const maxUpstreamBody = 64 << 20

type proxyItem struct {
	Address      string `json:"address"`
	Protocol     string `json:"protocol"`
	Latency      int    `json:"latency"`
	QualityGrade string `json:"quality_grade"`
	Status       string `json:"status"`
}

type slot struct {
	addr     string
	proxyURL *url.URL
}

type requestTrace struct {
	start       time.Time
	attempts    int
	proxies     map[string]struct{}
	finalProxy  string
	finalStatus int
	upstream    string
}

func newRequestTrace() *requestTrace {
	return &requestTrace{start: time.Now(), proxies: make(map[string]struct{})}
}

func (t *requestTrace) addAttempt(proxyName string) {
	t.attempts++
	t.proxies[proxyName] = struct{}{}
}

type gateway struct {
	cfg config

	mu         sync.RWMutex
	candidates []proxyItem
	slots      []slot
	custom     []slot

	fillMu        sync.Mutex
	refreshMu     sync.Mutex
	refillRunning atomic.Bool
	rr            atomic.Uint64
	rootContext   context.Context
	modelMu       sync.Mutex
	modelCache    *cachedModels

	mirrorMu     sync.Mutex
	mirrorState  map[string]*mirrorHealth
	mirrorCursor atomic.Uint64

	poolFailedMu sync.Mutex
	poolFailed   map[string]time.Time

	customFailsMu sync.Mutex
	customFails   map[string]int

	manualMu    sync.RWMutex
	manualAddrs map[string]struct{}

	advBridge *advancedBridge // 内嵌 sing-box：高级协议 → 本地 socks 端口
	advMu     sync.Mutex      // 保护下面三个字段与桥接的创建/重建
	advSeen   map[string]struct{}
	manualAdv []string // 手动高级链接（桥接重建时必须保留）
}

func newGateway(cfg config) *gateway {
	return &gateway{cfg: cfg, poolFailed: make(map[string]time.Time), customFails: make(map[string]int), manualAddrs: make(map[string]struct{}), advSeen: make(map[string]struct{})}
}

// markManual 登记手动节点：这类节点永不参与自动探活剔除。
func (g *gateway) markManual(addr string) {
	g.manualMu.Lock()
	g.manualAddrs[addr] = struct{}{}
	g.manualMu.Unlock()
}

func (g *gateway) isManual(addr string) bool {
	g.manualMu.RLock()
	defer g.manualMu.RUnlock()
	_, ok := g.manualAddrs[addr]
	return ok
}

func (g *gateway) removeManual(addr string) {
	g.manualMu.Lock()
	delete(g.manualAddrs, addr)
	g.manualMu.Unlock()
}

func (g *gateway) start(ctx context.Context) {
	g.rootContext = ctx
	go func() {
		if g.cfg.usesPublicPool() {
			if err := g.loadCandidates(ctx); err != nil {
				log.Printf("[选] load failed: %v", err)
			}
			if err := g.fillSlots(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[槽] initial fill failed: %v", err)
			}
		}
		if err := g.initCustomSlots(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[兜底] initial fill failed: %v", err)
		}
		log.Printf("[门] 预热完成")
	}()

	if len(g.cfg.poolURLs) > 0 {
		log.Printf("[池] 在线节点池已启用：%d 个源", len(g.cfg.poolURLs))
		go g.startPoolWatcher(ctx)
	}

	if g.cfg.usesPublicPool() {
		go func() {
			ticker := time.NewTicker(g.cfg.refreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					g.refresh(ctx)
				}
			}
		}()
	}
}

func (g *gateway) refresh(ctx context.Context) {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	if err := g.loadCandidates(ctx); err != nil {
		log.Printf("[刷新] candidate load failed: %v", err)
		return
	}
	if err := g.fillSlots(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[刷新] slot fill failed: %v", err)
	}
}

func (g *gateway) loadCandidates(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.proxyAPI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Transport: controlTransport(5 * time.Second)}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("proxy API returned %d", res.StatusCode)
	}

	var all []proxyItem
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&all); err != nil {
		return err
	}
	filtered := make([]proxyItem, 0, len(all))
	for _, item := range all {
		if item.QualityGrade == "S" && item.Status == "active" {
			filtered = append(filtered, item)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Latency < filtered[j].Latency })

	g.mu.Lock()
	g.candidates = filtered
	g.mu.Unlock()
	log.Printf("[选] %d S-grade candidates", len(filtered))
	return nil
}

func controlTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
}

func (g *gateway) fillSlots(ctx context.Context) error {
	if !g.fillMu.TryLock() {
		return nil
	}
	defer g.fillMu.Unlock()

	needed := g.cfg.slotCount - g.slotCount()
	if needed <= 0 {
		return nil
	}

	batch := g.takeCandidates(needed + 3)
	if len(batch) == 0 {
		if err := g.loadCandidates(ctx); err != nil {
			return err
		}
		batch = g.takeCandidates(needed + 3)
	}
	if len(batch) == 0 {
		return errNoProxy
	}

	type probeResult struct {
		slot    slot
		latency time.Duration
		ok      bool
	}
	results := make(chan probeResult, len(batch))
	var wg sync.WaitGroup
	for _, item := range batch {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := slotFromProxy(item.Address, item.Protocol)
			if err != nil {
				results <- probeResult{}
				return
			}
			started := time.Now()
			ok := g.probe(ctx, s)
			results <- probeResult{slot: s, latency: time.Since(started), ok: ok}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	added := 0
	for result := range results {
		if !result.ok || g.slotCount() >= g.cfg.slotCount {
			continue
		}
		if g.addSlot(result.slot, false) {
			added++
			log.Printf("[探+] %s (%dms)", result.slot.addr, result.latency.Milliseconds())
		}
	}
	log.Printf("[槽] %d/%d ready (added %d)", g.slotCount(), g.cfg.slotCount, added)
	return nil
}

// Close 释放内嵌 sing-box 等后台资源（进程退出时系统也会回收）。
func (g *gateway) Close() {
	g.advMu.Lock()
	bridge := g.advBridge
	g.advMu.Unlock()
	bridge.Close()
}

func (g *gateway) initCustomSlots(ctx context.Context) error {
	// 高级协议链接（vless/vmess/trojan/ss/hysteria2/tuic）先交给内嵌
	// sing-box 转成本地 socks5 端点，再与普通节点一起走原有流程。
	// 手动高级节点与节点池高级节点统一由 ensureAdvancedBridge 管理。
	manualLinks := collectAdvancedLinks(g.cfg.customProxies)
	g.advMu.Lock()
	g.manualAdv = manualLinks
	g.advMu.Unlock()
	g.ensureAdvancedBridge(ctx, manualLinks)
	parsed := g.parseCustomProxies(g.cfg.customProxies)
	if len(parsed) == 0 {
		return nil
	}

	type probeResult struct {
		slot    slot
		latency time.Duration
		ok      bool
	}
	results := make(chan probeResult, len(parsed))
	var wg sync.WaitGroup
	for _, candidate := range parsed {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			ok := g.probe(ctx, candidate)
			results <- probeResult{slot: candidate, latency: time.Since(started), ok: ok}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	ready := 0
	for result := range results {
		// 手动节点无论探活结果一律入池、永不自动移除；
		// 失效时的表现就是请求失败并轮换到下一个节点。
		g.markManual(result.slot.addr)
		if g.addSlot(result.slot, true) {
			if result.ok {
				ready++
				log.Printf("[手动+] %s (%dms)", result.slot.addr, result.latency.Milliseconds())
			} else {
				log.Printf("[手动] %s 探活未通过，仍按配置保留", result.slot.addr)
			}
		} else if result.ok {
			ready++
		}
	}
	log.Printf("[兜底] %d/%d custom proxies ready", ready, len(parsed))
	return nil
}

// collectAdvancedLinks 从配置文本里提取高级协议链接（手动节点来源）。
func collectAdvancedLinks(raw string) []string {
	parts := strings.Split(raw, ",")
	var links []string
	seen := make(map[string]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		schemeEnd := strings.Index(part, "://")
		if schemeEnd <= 0 {
			continue
		}
		if _, ok := isAdvancedScheme(part[:schemeEnd]); !ok {
			continue
		}
		if _, dup := seen[part]; dup {
			continue
		}
		if _, err := parseAdvancedNode(part); err != nil {
			log.Printf("[高级] 忽略 %s 链接: %v", strings.ToUpper(strings.SplitN(part, ":", 2)[0]), err)
			continue
		}
		seen[part] = struct{}{}
		links = append(links, part)
	}
	return links
}

func (g *gateway) parseCustomProxies(raw string) []slot {
	parts := strings.Split(raw, ",")
	result := make([]slot, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if schemeEnd := strings.Index(part, "://"); schemeEnd > 0 {
			if _, adv := isAdvancedScheme(part[:schemeEnd]); adv {
				g.advMu.Lock()
				bridge := g.advBridge
				g.advMu.Unlock()
				if bridge == nil {
					log.Printf("[配置] 高级节点暂不可用: %s", redactProxy(part))
					continue
				}
				local, ok := bridge.Links[part]
				if !ok {
					log.Printf("[配置] 高级节点暂不可用: %s", redactProxy(part))
					continue
				}
				s, err := slotFromRawURL("socks5://" + local)
				if err != nil {
					log.Printf("[配置] 本地桥接地址无效: %v", err)
					continue
				}
				result = append(result, s)
				continue
			}
		}
		s, err := slotFromRawURL(part)
		if err != nil {
			log.Printf("[配置] 忽略无效代理 %q: %v", redactProxy(part), err)
			continue
		}
		result = append(result, s)
	}
	return result
}

func slotFromProxy(address, protocol string) (slot, error) {
	scheme := "http"
	if strings.HasPrefix(strings.ToLower(protocol), "socks5") {
		scheme = "socks5h"
	}
	return slotFromRawURL(scheme + "://" + address)
}

func slotFromRawURL(raw string) (slot, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return slot{}, err
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "socks5", "socks5h":
		u.Scheme = "socks5h"
	default:
		return slot{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return slot{}, errors.New("missing proxy host")
	}
	return slot{addr: u.Host, proxyURL: u}, nil
}

func redactProxy(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}

func (g *gateway) probe(ctx context.Context, candidate slot) bool {
	deadline := time.Now().Add(g.cfg.probeTimeout)
	request := upstreamRequest{
		method:   http.MethodGet,
		path:     g.cfg.project.probePath,
		headers:  g.cfg.project.probeHeaders.Clone(),
		deadline: deadline,
	}
	live, err := g.openUpstream(ctx, request, candidate.proxyURL, g.cfg.probeTimeout)
	if err != nil {
		return false
	}
	status := live.response.StatusCode
	live.Close()
	return status >= 200 && status < 400
}

func (g *gateway) takeCandidates(limit int) []proxyItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	used := make(map[string]struct{}, len(g.slots))
	for _, current := range g.slots {
		used[current.addr] = struct{}{}
	}

	result := make([]proxyItem, 0, limit)
	for len(g.candidates) > 0 && len(result) < limit {
		item := g.candidates[0]
		g.candidates = g.candidates[1:]
		if _, exists := used[item.Address]; exists {
			continue
		}
		used[item.Address] = struct{}{}
		result = append(result, item)
	}
	return result
}

func (g *gateway) addSlot(candidate slot, custom bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	target := &g.slots
	if custom {
		target = &g.custom
	}
	for _, existing := range *target {
		if existing.addr == candidate.addr {
			return false
		}
	}
	*target = append(*target, candidate)
	return true
}

func (g *gateway) slotCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.slots)
}

func (g *gateway) slotAddresses(custom bool) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	source := g.slots
	if custom {
		source = g.custom
	}
	result := make([]string, 0, len(source))
	for _, current := range source {
		result = append(result, current.addr)
	}
	return result
}

// nextSlot 选取下一个代理槽位。session 非空时使用 rendezvous 哈希，
// 让同一会话在槽位存活期间固定同一出口（匿名通道按出口 IP 限流），
// 槽位增删只影响映射到该槽位的会话；session 为空时保持轮询行为。
func (g *gateway) nextSlot(custom bool, tried map[string]struct{}, session string, attempt int) (slot, bool) {
	g.mu.RLock()
	source := g.slots
	if custom {
		source = g.custom
	}
	snapshot := append([]slot(nil), source...)
	g.mu.RUnlock()
	if len(snapshot) == 0 {
		return slot{}, false
	}

	if session != "" {
		sort.SliceStable(snapshot, func(i, j int) bool {
			return rendezvousScore(session, snapshot[i].addr) > rendezvousScore(session, snapshot[j].addr)
		})
		if custom {
			return snapshot[attempt%len(snapshot)], true
		}
		for _, candidate := range snapshot {
			if _, exists := tried[candidate.addr]; !exists {
				return candidate, true
			}
		}
		return slot{}, false
	}

	start := int(g.rr.Add(1)-1) % len(snapshot)
	for offset := 0; offset < len(snapshot); offset++ {
		candidate := snapshot[(start+offset)%len(snapshot)]
		if custom {
			return candidate, true
		}
		if _, exists := tried[candidate.addr]; !exists {
			return candidate, true
		}
	}
	return slot{}, false
}

func rendezvousScore(session, addr string) uint64 {
	sum := sha256.Sum256([]byte(session + "\x00" + addr))
	return binary.BigEndian.Uint64(sum[:8])
}

func (g *gateway) dropSlot(address string) {
	g.mu.Lock()
	removed := false
	for i, current := range g.slots {
		if current.addr == address {
			g.slots = append(g.slots[:i], g.slots[i+1:]...)
			removed = true
			break
		}
	}
	remaining := len(g.slots)
	g.mu.Unlock()
	if !removed {
		return
	}
	log.Printf("[弃] %s -> %d/%d", address, remaining, g.cfg.slotCount)
	g.scheduleFill()
}

func (g *gateway) scheduleFill() {
	if g.rootContext == nil || !g.refillRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer g.refillRunning.Store(false)
		ctx, cancel := context.WithTimeout(g.rootContext, g.cfg.probeTimeout+5*time.Second)
		defer cancel()
		if err := g.fillSlots(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[槽] refill failed: %v", err)
		}
	}()
}

type upstreamRequest struct {
	method    string
	path      string
	headers   http.Header
	body      []byte
	stream    bool
	nonStream bool
	session   string
	deadline  time.Time
	upstream  string // 本次请求使用的上游基址；空值表示项目默认上游
}

type gatewayResponse struct {
	status int
	header http.Header
	body   []byte
	live   *liveResponse
}

type liveResponse struct {
	response  *http.Response
	cancel    context.CancelFunc
	transport *http.Transport
	once      sync.Once
}

func (r *liveResponse) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.response != nil && r.response.Body != nil {
			_ = r.response.Body.Close()
		}
		r.cancel()
		r.transport.CloseIdleConnections()
	})
}

func (r *liveResponse) readAll(deadline time.Time) ([]byte, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		r.Close()
		return nil, errRequestTimeout
	}
	timedOut := make(chan struct{})
	timer := time.AfterFunc(remaining, func() {
		close(timedOut)
		r.cancel()
	})
	body, err := io.ReadAll(io.LimitReader(r.response.Body, maxUpstreamBody+1))
	stopped := timer.Stop()
	r.Close()
	if !stopped {
		<-timedOut
		return nil, errRequestTimeout
	}
	if err != nil {
		return nil, err
	}
	if len(body) > maxUpstreamBody {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", maxUpstreamBody)
	}
	return body, nil
}

func (g *gateway) openUpstream(ctx context.Context, request upstreamRequest, proxyURL *url.URL, maxWait time.Duration) (*liveResponse, error) {
	base := request.upstream
	if base == "" {
		base = g.cfg.project.upstream
	}
	target := strings.TrimRight(base, "/") + request.path
	wait, connectTimeout, responseHeaderTimeout := g.openTimeouts(request.deadline, maxWait)
	return openHTTP(ctx, request.method, target, request.headers, request.body, proxyURL, wait, connectTimeout, responseHeaderTimeout)
}

func (g *gateway) openTimeouts(deadline time.Time, firstByteLimit time.Duration) (time.Duration, time.Duration, time.Duration) {
	if firstByteLimit > 0 {
		wait := boundedWait(deadline, firstByteLimit)
		return wait, wait, wait
	}
	wait := boundedWait(deadline, 0)
	connectTimeout := boundedWait(deadline, g.cfg.firstByteTimeout)
	return wait, connectTimeout, 0
}

func openHTTP(ctx context.Context, method, target string, headers http.Header, body []byte, proxyURL *url.URL, wait, connectTimeout, responseHeaderTimeout time.Duration) (*liveResponse, error) {
	if wait <= 0 {
		return nil, errRequestTimeout
	}
	if connectTimeout <= 0 || connectTimeout > wait {
		connectTimeout = wait
	}
	if responseHeaderTimeout < 0 {
		responseHeaderTimeout = 0
	}
	if responseHeaderTimeout > wait {
		responseHeaderTimeout = wait
	}

	attemptContext, cancel := context.WithCancel(ctx)
	timedOut := make(chan struct{})
	timer := time.AfterFunc(wait, func() {
		close(timedOut)
		cancel()
	})

	transport := requestTransport(proxyURL, connectTimeout, responseHeaderTimeout)
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(attemptContext, method, target, bytes.NewReader(body))
	if err != nil {
		timer.Stop()
		cancel()
		transport.CloseIdleConnections()
		return nil, err
	}
	req.Header = headers.Clone()
	req.Header.Del("Host")
	req.Header.Del("Content-Length")

	res, err := client.Do(req)
	stopped := timer.Stop()
	if err != nil {
		cancel()
		transport.CloseIdleConnections()
		select {
		case <-timedOut:
			return nil, errAttemptTimeout
		default:
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !stopped {
		<-timedOut
		_ = res.Body.Close()
		cancel()
		transport.CloseIdleConnections()
		return nil, errAttemptTimeout
	}
	return &liveResponse{response: res, cancel: cancel, transport: transport}, nil
}

func requestTransport(proxyURL *url.URL, connectTimeout, responseHeaderTimeout time.Duration) *http.Transport {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: -1,
		}).DialContext,
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSHandshakeTimeout:    connectTimeout,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // 保持旧网关行为；上游与代理链可能使用非标准证书。
		},
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return transport
}

func boundedWait(deadline time.Time, maximum time.Duration) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	if maximum <= 0 || remaining < maximum {
		return remaining
	}
	return maximum
}

func (g *gateway) perform(ctx context.Context, request upstreamRequest, proxyURL *url.URL) (*gatewayResponse, error) {
	firstByteLimit := g.cfg.firstByteTimeout
	if request.nonStream {
		firstByteLimit = 0
	}
	live, err := g.openUpstream(ctx, request, proxyURL, firstByteLimit)
	if err != nil {
		if errors.Is(err, errAttemptTimeout) && time.Until(request.deadline) <= 0 {
			return nil, errRequestTimeout
		}
		return nil, err
	}
	status := live.response.StatusCode
	header := cloneEndToEndHeaders(live.response.Header)
	if request.stream && status < 400 {
		return &gatewayResponse{status: status, header: header, live: live}, nil
	}
	body, err := live.readAll(request.deadline)
	if err != nil {
		return nil, err
	}
	return &gatewayResponse{status: status, header: header, body: body}, nil
}

// dispatch 在主上游与镜像之间轮询：任一上游给出非重试状态即返回，
// 失败的上游会被记录并在连续失败后短暂冷却。
func (g *gateway) dispatch(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	pool := g.cfg.upstreamPool()
	attempts := len(pool)
	if attempts > 3 {
		attempts = 3
	}
	if request.upstream != "" {
		attempts = 1
	}

	var lastResponse *gatewayResponse
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		current := request
		if current.upstream == "" {
			current.upstream = g.pickUpstream()
		}
		if len(pool) > 1 {
			log.Printf("[镜] 上游: %s", shortUpstream(current.upstream))
		}
		trace.upstream = shortUpstream(current.upstream)
		response, err := g.dispatchOnce(ctx, current, trace)
		if err != nil {
			g.noteUpstreamResult(current.upstream, false)
			if isTerminalContextError(ctx, err) {
				return nil, err
			}
			lastErr = err
			continue
		}
		if !retryableStatus(response.status) {
			g.noteUpstreamResult(current.upstream, true)
			return response, nil
		}
		g.noteUpstreamResult(current.upstream, false)
		lastResponse = response
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errNoProxy
}

func (g *gateway) dispatchOnce(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	if g.cfg.forceRelay {
		if g.cfg.zenKey == "" {
			return jsonGatewayResponse(http.StatusBadGateway, "FORCE_RELAY 但未配置 ZENPROXY_KEY"), nil
		}
		return g.dispatchZen(ctx, request, trace)
	}

	var last *gatewayResponse
	// 并行竞速模式：手动节点 + 轮选在线池节点 + 直连同时出发，最快返回者胜出。
	// 仅直连模式（PROXY_ORDER=direct）不参与竞速。
	if g.cfg.raceEnabled && g.customCount() > 0 {
		skip := false
		for _, layer := range g.cfg.orderedLayers() {
			if layer == layerDirect {
				skip = true
				break
			}
		}
		if !skip {
			return g.dispatchRace(ctx, request, trace)
		}
	}
	for _, layer := range g.cfg.orderedLayers() {
		var response *gatewayResponse
		var err error
		switch layer {
		case layerPublic:
			response, err = g.dispatchPublicLayer(ctx, request, trace)
		case layerZen:
			if g.cfg.zenKey == "" {
				continue
			}
			response, err = g.dispatchZen(ctx, request, trace)
		case layerCustom:
			if g.customCount() == 0 {
				continue
			}
			response, err = g.dispatchCustomLayer(ctx, request, trace)
		default:
			continue
		}
		if err != nil {
			if isTerminalContextError(ctx, err) {
				return nil, err
			}
			if !errors.Is(err, errNoProxy) {
				log.Printf("[层错] %s: %v", layer, err)
			}
			continue
		}
		if !retryableStatus(response.status) {
			return response, nil
		}
		last = response
	}

	if g.cfg.project.directFallback {
		return g.dispatchDirect(ctx, request, trace)
	}
	if last != nil {
		return last, nil
	}
	return nil, errNoProxy
}

func (g *gateway) dispatchPublicLayer(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	if g.slotCount() == 0 {
		fillContext, cancel := context.WithDeadline(ctx, request.deadline)
		_ = g.fillSlots(fillContext)
		cancel()
	}

	var last *gatewayResponse
	lastProxy := ""
	tried := make(map[string]struct{})
	for retry := 0; retry < g.cfg.slotRetries; retry++ {
		if err := requestBudgetError(ctx, request.deadline); err != nil {
			return nil, err
		}
		candidate, ok := g.nextSlot(false, tried, request.session, retry)
		if !ok {
			break
		}
		tried[candidate.addr] = struct{}{}
		trace.addAttempt(candidate.addr)
		log.Printf("[S级] %s (%d/%d)", candidate.addr, retry+1, g.cfg.slotRetries)
		response, err := g.perform(ctx, request, candidate.proxyURL)
		if err != nil {
			if isTerminalContextError(ctx, err) {
				return nil, err
			}
			log.Printf("[错] %s: %v", candidate.addr, err)
			g.dropSlot(candidate.addr)
			continue
		}
		if !retryableStatus(response.status) {
			trace.finalProxy = candidate.addr
			return response, nil
		}
		log.Printf("[错码] %s 状态码 %d", candidate.addr, response.status)
		g.dropSlot(candidate.addr)
		last = response
		lastProxy = candidate.addr
	}
	if last != nil {
		trace.finalProxy = lastProxy
		return last, nil
	}
	return nil, errNoProxy
}

func (g *gateway) dispatchCustomLayer(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	maxRetries := g.cfg.customRetries
	if maxRetries == 0 {
		maxRetries = g.customCount()
	}
	var last *gatewayResponse
	lastProxy := ""
	for retry := 0; retry < maxRetries; retry++ {
		if err := requestBudgetError(ctx, request.deadline); err != nil {
			return nil, err
		}
		candidate, ok := g.nextSlot(true, nil, request.session, retry)
		if !ok {
			break
		}
		trace.addAttempt(candidate.addr)
		log.Printf("[自定义] %s (%d/%d)", candidate.addr, retry+1, maxRetries)
		response, err := g.perform(ctx, request, candidate.proxyURL)
		if err != nil {
			if isTerminalContextError(ctx, err) {
				return nil, err
			}
			g.noteCustomResult(candidate.addr, false)
			log.Printf("[错] %s: %v", candidate.addr, err)
			continue
		}
		if !retryableStatus(response.status) {
			g.noteCustomResult(candidate.addr, true)
			trace.finalProxy = candidate.addr
			return response, nil
		}
		// 注意：429 等状态码说明节点能连通上游（只是暂时限流），不算节点故障。
		log.Printf("[错码] %s 状态码 %d", candidate.addr, response.status)
		last = response
		lastProxy = candidate.addr
	}
	if last != nil {
		trace.finalProxy = lastProxy
		return last, nil
	}
	return nil, errNoProxy
}

// raceExits 组装竞速出口：手动节点全部参加，自动节点轮选补足宽度。
func (g *gateway) raceExits() []slot {
	all := g.customSnapshot()
	var manual, auto []slot
	for _, s := range all {
		if g.isManual(s.addr) {
			manual = append(manual, s)
		} else {
			auto = append(auto, s)
		}
	}
	width := g.cfg.raceWidth
	if width < 2 {
		width = 2
	}
	exits := make([]slot, 0, len(manual)+width)
	exits = append(exits, manual...)
	if len(auto) > 0 {
		start := int(g.rr.Add(1)-1) % len(auto)
		for i := 0; i < len(auto) && len(exits) < width+len(manual); i++ {
			exits = append(exits, auto[(start+i)%len(auto)])
		}
	}
	return exits
}

// dispatchRace 并行竞速：同一请求同时发往多个出口（手动节点、在线池节点、直连），
// 最先给出可用响应的通道胜出，其余通道立即取消。
// 客户端因此无需设置超长超时——等待由网关内部并行消化。
func (g *gateway) dispatchRace(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	exits := g.raceExits()

	type raceResult struct {
		addr    string
		isDirect bool
		resp    *gatewayResponse
		err     error
	}
	raceCtx, cancel := context.WithCancel(ctx)
	// 关键：胜出/兜底路径会把仍挂着数据流的响应交给调用方（流式响应的 body
	// 就活在 raceCtx 的取消链上），所以这两条路径绝不能触发 cancel，
	// 否则赢家刚返回流就被掐断，客户端表现为 Connection error。
	results := make(chan raceResult, len(exits)+1)

	const directAddr = "直连"
	launched := 0
	for _, s := range exits {
		launched++
		trace.addAttempt(s.addr)
		go func(s slot) {
			resp, err := g.perform(raceCtx, request, s.proxyURL)
			results <- raceResult{addr: s.addr, resp: resp, err: err}
		}(s)
	}
	if g.cfg.project.directFallback {
		launched++
		trace.addAttempt(directAddr)
		go func() {
			resp, err := g.perform(raceCtx, request, nil)
			results <- raceResult{addr: directAddr, isDirect: true, resp: resp, err: err}
		}()
	}
	log.Printf("[竞速] %d 路并行出发", launched)

	var last *gatewayResponse
	lastAddr := ""
	for received := 0; received < launched; received++ {
		res := <-results
		if res.err != nil {
			if !res.isDirect {
				g.noteCustomResult(res.addr, false)
			}
			if ctx.Err() != nil {
				cancel()
				return nil, res.err
			}
			continue
		}
		if retryableStatus(res.resp.status) {
			// 限流/5xx 不算赢家；留第一个作为全军覆没时的兜底，重复的直接关闭。
			if last == nil {
				last = res.resp
				lastAddr = res.addr
			} else {
				res.resp.live.Close()
			}
			continue
		}
		// 赢家出现。不 cancel：在途败者等它们自己的首字节超时自然退场，
		// 由下面的收割协程负责关闭迟到响应的连接，防止泄漏。
		if last != nil {
			last.live.Close()
			last = nil
		}
		if !res.isDirect {
			g.noteCustomResult(res.addr, true)
		}
		log.Printf("[竞速] 胜出: %s", res.addr)
		trace.finalProxy = res.addr
		go func() {
			deadline := time.After(40 * time.Second) // 覆盖最长的首字节超时窗口
			for {
				select {
				case extra := <-results:
					if extra.resp != nil {
						extra.resp.live.Close()
					}
				case <-deadline:
					return
				}
			}
		}()
		return res.resp, nil
	}
	if last != nil {
		log.Printf("[竞速] 无健康出口，使用可重试兜底响应: %s", lastAddr)
		trace.finalProxy = lastAddr
		return last, nil
	}
	cancel()
	return nil, errNoProxy
}

func (g *gateway) dispatchZen(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	retries := g.cfg.zenRetries
	if retries < 1 {
		retries = 1
	}
	var last *gatewayResponse
	for retry := 0; retry < retries; retry++ {
		if err := requestBudgetError(ctx, request.deadline); err != nil {
			return nil, err
		}
		trace.addAttempt("ZenProxy")
		log.Printf("[ZenProxy] (%d/%d)", retry+1, retries)
		response, err := g.performRelay(ctx, request)
		if err != nil {
			if isTerminalContextError(ctx, err) {
				return nil, err
			}
			log.Printf("[ZenProxy] 错误: %v", err)
			continue
		}
		if !retryableStatus(response.status) {
			trace.finalProxy = "ZenProxy"
			return response, nil
		}
		log.Printf("[ZenProxy] 状态码 %d，重试", response.status)
		last = response
	}
	if last != nil {
		trace.finalProxy = "ZenProxy"
		return last, nil
	}
	return nil, errNoProxy
}

func (g *gateway) performRelay(ctx context.Context, request upstreamRequest) (*gatewayResponse, error) {
	relay, err := url.Parse(g.cfg.zenRelay)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(requestUpstream(g.cfg.project.upstream, request), "/") + request.path
	query := relay.Query()
	query.Set("api_key", g.cfg.zenKey)
	query.Set("url", target)
	query.Set("method", request.method)
	relay.RawQuery = query.Encode()

	headers := request.headers.Clone()
	headers.Del("Host")
	headers.Del("Content-Length")
	headers.Del("Authorization")
	firstByteLimit := g.cfg.firstByteTimeout
	if request.nonStream {
		firstByteLimit = 0
	}
	wait, connectTimeout, responseHeaderTimeout := g.openTimeouts(request.deadline, firstByteLimit)
	live, err := openHTTP(ctx, http.MethodPost, relay.String(), headers, request.body, nil, wait, connectTimeout, responseHeaderTimeout)
	if err != nil {
		if errors.Is(err, errAttemptTimeout) && time.Until(request.deadline) <= 0 {
			return nil, errRequestTimeout
		}
		return nil, err
	}
	status := live.response.StatusCode
	header := cloneEndToEndHeaders(live.response.Header)
	if request.stream && status < 400 {
		return &gatewayResponse{status: status, header: header, live: live}, nil
	}
	body, err := live.readAll(request.deadline)
	if err != nil {
		return nil, err
	}
	return &gatewayResponse{status: status, header: header, body: body}, nil
}

func (g *gateway) dispatchDirect(ctx context.Context, request upstreamRequest, trace *requestTrace) (*gatewayResponse, error) {
	trace.addAttempt("direct")
	log.Printf("[直连] directly connecting to upstream")
	response, err := g.perform(ctx, request, nil)
	if err != nil {
		return nil, err
	}
	trace.finalProxy = "direct"
	return response, nil
}

func (g *gateway) customCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.custom)
}

func requestBudgetError(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Until(deadline) <= 0 {
		return errRequestTimeout
	}
	return nil
}

func isTerminalContextError(ctx context.Context, err error) bool {
	return errors.Is(err, errRequestTimeout) || ctx.Err() != nil
}

func retryableStatus(status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

func jsonGatewayResponse(status int, message string) *gatewayResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return &gatewayResponse{
		status: status,
		header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		body:   body,
	}
}

func cloneEndToEndHeaders(source http.Header) http.Header {
	result := make(http.Header, len(source))
	for key, values := range source {
		if hopByHopHeader(key) {
			continue
		}
		result[key] = append([]string(nil), values...)
	}
	return result
}

func hopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
