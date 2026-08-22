package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
