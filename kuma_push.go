package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kumaRequestTimeout = 5 * time.Second
	kumaBatchTimeout   = 20 * time.Second
)

type kumaPush struct {
	slug string
	up   bool
	ping int
}

type kumaPushBatch struct {
	baseURL string
	pushes  []kumaPush
}

var (
	kumaClient    = &http.Client{Timeout: kumaRequestTimeout}
	kumaPushQueue = make(chan kumaPushBatch, 1)
	kumaPushOnce  sync.Once
)

// Keep one pending snapshot. A slow notification service never blocks probing
// or creates an unbounded backlog of obsolete status notifications.
func enqueueLatestKuma(queue chan kumaPushBatch, batch kumaPushBatch) {
	select {
	case queue <- batch:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- batch:
	default:
	}
}

func queueKumaPushes(baseURL string, pushes []kumaPush) {
	if baseURL == "" || len(pushes) == 0 {
		return
	}
	kumaPushOnce.Do(func() {
		go func() {
			for batch := range kumaPushQueue {
				ctx, cancel := context.WithTimeout(context.Background(), kumaBatchTimeout)
				for _, push := range batch.pushes {
					if ctx.Err() != nil {
						break
					}
					_ = sendKumaPush(ctx, kumaClient, batch.baseURL, push)
				}
				cancel()
			}
		}()
	})
	enqueueLatestKuma(kumaPushQueue, kumaPushBatch{baseURL: baseURL, pushes: pushes})
}

func sendKumaPush(ctx context.Context, client *http.Client, baseURL string, push kumaPush) error {
	ctx, cancel := context.WithTimeout(ctx, kumaRequestTimeout)
	defer cancel()
	status := "down"
	if push.up {
		status = "up"
	}
	query := url.Values{"status": {status}, "msg": {"ok"}}
	if push.ping > 0 {
		query.Set("ping", strconv.Itoa(push.ping))
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/api/push/" + url.PathEscape(push.slug) + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return err
}
