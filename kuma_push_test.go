package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKumaPushSendsStatus(t *testing.T) {
	for _, up := range []bool{true, false} {
		var path, status, ping string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, status, ping = r.URL.Path, r.URL.Query().Get("status"), r.URL.Query().Get("ping")
			io.WriteString(w, `{"ok":true}`)
		}))
		err := sendKumaPush(context.Background(), srv.Client(), srv.URL, kumaPush{slug: "test-slug", up: up, ping: 123})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "down"
		if up {
			want = "up"
		}
		if path != "/api/push/test-slug" || status != want || ping != "123" {
			t.Fatalf("push: path=%s status=%s ping=%s", path, status, ping)
		}
	}
}

type kumaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kumaRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestKumaPushHasDeadlineAndHonorsCancellation(t *testing.T) {
	entered := make(chan time.Time, 1)
	client := &http.Client{Transport: kumaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("Kuma request has no deadline")
		}
		entered <- deadline
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- sendKumaPush(ctx, client, "http://kuma.invalid", kumaPush{slug: "test"})
	}()
	select {
	case deadline := <-entered:
		if left := time.Until(deadline); left <= 0 || left > kumaRequestTimeout {
			t.Errorf("request deadline is %s away", left)
		}
	case <-time.After(time.Second):
		t.Fatal("Kuma request was not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled push succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled push blocked")
	}
}

func TestKumaQueueKeepsLatestWithoutBlocking(t *testing.T) {
	queue := make(chan kumaPushBatch, 1)
	enqueueLatestKuma(queue, kumaPushBatch{baseURL: "old"})
	done := make(chan struct{})
	go func() {
		enqueueLatestKuma(queue, kumaPushBatch{baseURL: "new"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full notification queue blocked probing")
	}
	if got := <-queue; got.baseURL != "new" {
		t.Fatalf("queued snapshot = %q, want new", got.baseURL)
	}
}
