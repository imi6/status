package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeReusesConnections(t *testing.T) {
	var accepted atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	defer probeClient.CloseIdleConnections()
	for i := 0; i < 40; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		code, _, _, failure := doReq(req, 2)
		if code != http.StatusOK || failure != "" {
			t.Fatalf("probe %d: HTTP %d, %s", i, code, failure)
		}
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("40 sequential probes opened %d connections, want 1", got)
	}
	t.Log("40 sequential probes reused one connection")
}

func TestProbeResponseLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		limit    int64
		wantFail bool
	}{
		{"exact limit", 1024, 1024, false},
		{"over limit", 1025, 1024, true},
		{"large image", 1400026, imageResponseLimit, false},
		{"text remains bounded", 1400026, probeResponseLimit, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("x", tc.size)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			code, got, _, failure := doReqWithLimit(req, 2, tc.limit)
			if code != http.StatusOK {
				t.Fatalf("HTTP %d", code)
			}
			if tc.wantFail {
				if !strings.Contains(failure, "超过大小上限") || got != nil {
					t.Fatalf("oversize must be explicit: bytes=%d, reason=%q", len(got), failure)
				}
			} else if failure != "" || string(got) != body {
				t.Fatalf("response was truncated: bytes=%d/%d, reason=%q", len(got), len(body), failure)
			}
		})
	}
}

func TestProbeLimitsRequestsToOneHost(t *testing.T) {
	var active, peak atomic.Int32
	entered := make(chan struct{}, 20)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	defer unblock()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			code, _, _, failure := doReq(req, 2)
			if code != http.StatusOK || failure != "" {
				t.Errorf("probe: HTTP %d, %s", code, failure)
			}
		}()
	}
	for i := 0; i < maxConcurrentProbesPerHost; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			unblock()
			wg.Wait()
			t.Fatal("probes did not reach the server")
		}
	}
	unblock()
	wg.Wait()
	if got := peak.Load(); got != maxConcurrentProbesPerHost {
		t.Fatalf("peak concurrent requests = %d, want %d", got, maxConcurrentProbesPerHost)
	}
}

func TestProbeLimiterCancellationAndCleanup(t *testing.T) {
	l := newProbeLimiter(2, 1)
	releaseA, _ := l.acquire(context.Background(), "a")
	releaseB, _ := l.acquire(context.Background(), "b")
	for _, host := range []string{"a", "c"} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			release, err := l.acquire(ctx, host)
			if release != nil {
				release()
			}
			done <- err
		}()
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Errorf("host %s: got %v, want cancellation", host, err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled admission is stuck")
		}
	}
	releaseA()
	releaseB()
	if len(l.hosts) != 0 || len(l.global) != 0 {
		t.Fatalf("admission leaked state: hosts=%d, global=%d", len(l.hosts), len(l.global))
	}
}

func TestProbeCancellationReleasesAdmission(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		_, _, _, failure := doReq(req, 2)
		if failure == "" {
			t.Error("cancelled probe succeeded")
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive the probe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled probe is stuck")
	}
	probeRequests.mu.Lock()
	defer probeRequests.mu.Unlock()
	if len(probeRequests.hosts) != 0 || len(probeRequests.global) != 0 {
		t.Fatal("cancelled probe leaked admission slots")
	}
}
