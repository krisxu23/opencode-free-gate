package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDispatchRaceFastestExitWins(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	fastURL, err := url.Parse(fast.URL)
	if err != nil {
		t.Fatal(err)
	}
	slowURL, err := url.Parse(slow.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config{
		project:          projectSpec{upstream: "http://upstream.invalid/zen"},
		proxyMode:        "auto",
		proxyOrder:       []string{layerCustom},
		raceEnabled:      true,
		raceWidth:        4,
		firstByteTimeout: time.Second,
		hardTimeout:      10 * time.Second,
	}
	gw := newGateway(cfg)
	gw.custom = []slot{
		{addr: slowURL.Host, proxyURL: slowURL},
		{addr: fastURL.Host, proxyURL: fastURL},
	}

	started := time.Now()
	resp, err := gw.dispatchRace(context.Background(), upstreamRequest{
		method:   http.MethodGet,
		path:     "/zen/v1/models",
		headers:  http.Header{},
		stream:   true,
		deadline: time.Now().Add(cfg.hardTimeout),
	}, newRequestTrace())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.status)
	}
	if time.Since(started) > 400*time.Millisecond {
		t.Fatalf("slow exit won the race: %v", time.Since(started))
	}
}

// 回归测试：竞速返回后，赢家的流式响应必须仍然可读。
// 曾经的 bug：dispatchRace 返回时触发 cancel，把赢家的流当场掐断，
// 客户端表现为 Connection error。
func TestDispatchRaceWinnerStreamStaysAlive(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("data: first\n\n"))
			f.Flush()
		}
		<-r.Context().Done() // 保持连接直到客户端离开
	}))
	defer fast.Close()

	fastURL, err := url.Parse(fast.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{
		project:          projectSpec{upstream: "http://upstream.invalid/zen"},
		proxyMode:        "auto",
		proxyOrder:       []string{layerCustom},
		raceEnabled:      true,
		raceWidth:        4,
		firstByteTimeout: time.Second,
		hardTimeout:      10 * time.Second,
	}
	gw := newGateway(cfg)
	gw.custom = []slot{{addr: fastURL.Host, proxyURL: fastURL}}

	resp, err := gw.dispatchRace(context.Background(), upstreamRequest{
		method:   http.MethodPost,
		path:     "/zen/v1/chat/completions",
		headers:  http.Header{},
		stream:   true,
		deadline: time.Now().Add(cfg.hardTimeout),
	}, newRequestTrace())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.status != http.StatusOK || resp.live == nil {
		t.Fatalf("expected streaming 200 with live body, got status=%d live=%v", resp.status, resp.live)
	}

	// 返回之后流必须还能读出赢家已推送的数据。
	buf := make([]byte, 32)
	_ = resp.live.response.Body.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, readErr := resp.live.response.Body.Read(buf)
	if readErr != nil || n == 0 {
		t.Fatalf("winner stream was killed after return: n=%d err=%v", n, readErr)
	}
	if !strings.Contains(string(buf[:n]), "data: first") {
		t.Fatalf("unexpected stream content: %q", string(buf[:n]))
	}
	resp.live.Close()
}

func TestRaceExitsManualAlwaysIncluded(t *testing.T) {
	cfg := config{raceWidth: 2}
	gw := newGateway(cfg)
	gw.custom = []slot{
		{addr: "manual-1"}, {addr: "auto-1"}, {addr: "auto-2"}, {addr: "auto-3"}, {addr: "auto-4"},
	}
	gw.markManual("manual-1")

	exits := gw.raceExits()
	found := false
	for _, s := range exits {
		if s.addr == "manual-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("手动节点应始终参加竞速")
	}
	autoCount := 0
	for _, s := range exits {
		if s.addr != "manual-1" {
			autoCount++
		}
	}
	if autoCount > 2 {
		t.Fatalf("自动节点应受宽度限制（≤%d），实际 %d", cfg.raceWidth, autoCount)
	}
}

func TestEnvIsOn(t *testing.T) {
	for _, raw := range []string{"1", "true", "ON", "yes", " y "} {
		if !envIsOn(raw) {
			t.Errorf("envIsOn(%q) 应为 true", raw)
		}
	}
	for _, raw := range []string{"0", "false", "off", "", "no"} {
		if envIsOn(raw) {
			t.Errorf("envIsOn(%q) 应为 false", raw)
		}
	}
}
