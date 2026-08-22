package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// transportPool 按「出口代理 + 关键超时」缓存 http.Transport，
// 让同一出口的连续请求复用连接，省掉每次请求完整的 TCP+TLS 握手。
// 条目闲置超过 idle 阈值后由后台清扫回收；已被取出的 transport
// 即使被回收也仍可安全使用，只是不再接受新连接。
type transportPool struct {
	mu      sync.Mutex
	entries map[string]*transportEntry
}

type transportEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

var sharedTransports = &transportPool{entries: make(map[string]*transportEntry)}

func (p *transportPool) get(proxyURL *url.URL, connectTimeout, responseHeaderTimeout time.Duration) *http.Transport {
	// 超时向上取整到 100ms 档位：transport 层超时只会比请求预算更宽松，
	// 精确的截止仍由每次请求自身的计时器兜底；取整让同一代理
	// 在剩余预算波动时仍复用同一条目，而不是每毫秒一个新 Transport。
	connectTimeout = quantizeTimeout(connectTimeout)
	responseHeaderTimeout = quantizeTimeout(responseHeaderTimeout)
	key := transportKey(proxyURL, connectTimeout, responseHeaderTimeout)

	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.entries[key]; ok {
		entry.lastUsed = time.Now()
		return entry.transport
	}
	transport := requestTransport(proxyURL, connectTimeout, responseHeaderTimeout)
	p.entries[key] = &transportEntry{transport: transport, lastUsed: time.Now()}
	return transport
}

// sweep 关闭并移除超过 idle 未使用的条目，防止节点池轮换导致缓存无限增长。
func (p *transportPool) sweep(idle time.Duration) {
	p.mu.Lock()
	now := time.Now()
	for key, entry := range p.entries {
		if now.Sub(entry.lastUsed) >= idle {
			entry.transport.CloseIdleConnections()
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()
}

func quantizeTimeout(d time.Duration) time.Duration {
	const step = 100 * time.Millisecond
	if d <= 0 {
		return 0
	}
	return (d + step - 1) / step * step
}

func transportKey(proxyURL *url.URL, connectTimeout, responseHeaderTimeout time.Duration) string {
	proxy := ""
	if proxyURL != nil {
		proxy = proxyURL.String()
	}
	return fmt.Sprintf("%s|%d|%d", proxy, connectTimeout.Milliseconds(), responseHeaderTimeout.Milliseconds())
}
