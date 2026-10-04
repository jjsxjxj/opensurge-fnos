package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"open-mihomo-gateway/internal/config"
	"open-mihomo-gateway/internal/mihomo"
)

func TestProxyHealthEndpointsExposeAndTestCurrentNodes(t *testing.T) {
	server := newTestServer(t)
	server.fetchProxyHealth = func(context.Context, config.Config) (mihomo.ProxyHealthSnapshot, error) {
		return mihomo.ProxyHealthSnapshot{TestURL: mihomo.DefaultProxyDelayTestURL, Proxies: []mihomo.ProxyHealth{
			{Name: "DIRECT", Type: "Direct", Status: "reachable", Probeable: true},
			{Name: "HK", Type: "Hysteria2", Status: "untested", UDP: true, Probeable: true},
			{Name: "REJECT", Type: "Reject", Status: "not_applicable", Probeable: false},
			{Name: mihomo.LocalRoutingGlobalGroup, Type: "Selector", Status: "untested", Probeable: true},
		}}, nil
	}
	server.measureProxyDelay = func(_ context.Context, _ config.Config, name, testURL string, timeout time.Duration) mihomo.ProxyDelayResult {
		if testURL != mihomo.DefaultProxyDelayTestURL || timeout != 5*time.Second {
			t.Fatalf("testURL=%q timeout=%s", testURL, timeout)
		}
		return mihomo.ProxyDelayResult{Name: name, Status: "reachable", DelayMS: 88, TestedAt: "2026-07-15T10:00:00Z", TestURL: testURL}
	}

	response := performAuthorized(server, http.MethodGet, "/api/v1/proxy-health", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"HK"`) || !strings.Contains(response.Body.String(), `"probeable":true`) || strings.Contains(response.Body.String(), mihomo.LocalRoutingGlobalGroup) {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}

	response = performAuthorized(server, http.MethodPost, "/api/v1/proxy-health/tests", []byte(`{"names":["HK","HK"]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	var result ProxyHealthTestResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].DelayMS != 88 || result.Results[0].Status != "reachable" {
		t.Fatalf("result = %#v", result)
	}
}

func TestProxyHealthTestsRejectUnknownAndNonProbeableNames(t *testing.T) {
	server := newTestServer(t)
	server.fetchProxyHealth = func(context.Context, config.Config) (mihomo.ProxyHealthSnapshot, error) {
		return mihomo.ProxyHealthSnapshot{TestURL: mihomo.DefaultProxyDelayTestURL, Proxies: []mihomo.ProxyHealth{{Name: "REJECT", Type: "Reject", Status: "not_applicable", Probeable: false}}}, nil
	}
	for _, body := range []string{`{"names":["missing"]}`, `{"names":["REJECT"]}`, `{"names":[]}`} {
		response := performAuthorized(server, http.MethodPost, "/api/v1/proxy-health/tests", []byte(body))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
}

// A batch must never probe more than proxyHealthConcurrency nodes at once:
// airports cap concurrent connections per subscription and a burst makes every
// node report unreachable. Modeled on the Clash Verge Rev delay manager.
func TestProxyHealthTestsThrottleBatchProbing(t *testing.T) {
	server := newTestServer(t)
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	proxies := make([]mihomo.ProxyHealth, 0, len(names))
	for _, name := range names {
		proxies = append(proxies, mihomo.ProxyHealth{Name: name, Type: "Vless", Status: "untested", Probeable: true})
	}
	server.fetchProxyHealth = func(context.Context, config.Config) (mihomo.ProxyHealthSnapshot, error) {
		return mihomo.ProxyHealthSnapshot{TestURL: mihomo.DefaultProxyDelayTestURL, Proxies: proxies}, nil
	}

	var mu sync.Mutex
	current, maxConcurrent := 0, 0
	server.measureProxyDelay = func(_ context.Context, _ config.Config, name, testURL string, _ time.Duration) mihomo.ProxyDelayResult {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		current--
		mu.Unlock()
		return mihomo.ProxyDelayResult{Name: name, Status: "reachable", DelayMS: 50, TestURL: testURL}
	}

	body, err := json.Marshal(ProxyHealthTestRequest{Names: names})
	if err != nil {
		t.Fatal(err)
	}
	response := performAuthorized(server, http.MethodPost, "/api/v1/proxy-health/tests", body)
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	var result ProxyHealthTestResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != len(names) {
		t.Fatalf("results = %d, want %d", len(result.Results), len(names))
	}
	if maxConcurrent > proxyHealthConcurrency {
		t.Fatalf("max concurrent probes = %d, want <= %d", maxConcurrent, proxyHealthConcurrency)
	}
}
