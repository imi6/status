package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxConcurrentProbes        = 8
	maxConcurrentProbesPerHost = 2
	probeResponseLimit         = 1 << 20
	imageResponseLimit         = 32 << 20
)

// One pool serves every probe; idle connections expire even if a target is removed.
var probeTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, // Preserve existing upstream compatibility.
	TLSHandshakeTimeout:   10 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	MaxIdleConns:          maxConcurrentProbes * 2,
	MaxIdleConnsPerHost:   maxConcurrentProbesPerHost,
	MaxConnsPerHost:       maxConcurrentProbesPerHost,
	ResponseHeaderTimeout: 0, // The request context supplies each model's timeout.
}

var probeClient = &http.Client{Transport: probeTransport}

type probeHostSlots struct {
	slots chan struct{}
	users int
}

type probeLimiter struct {
	mu      sync.Mutex
	global  chan struct{}
	hosts   map[string]*probeHostSlots
	perHost int
}

func newProbeLimiter(global, perHost int) *probeLimiter {
	return &probeLimiter{
		global:  make(chan struct{}, global),
		hosts:   make(map[string]*probeHostSlots),
		perHost: perHost,
	}
}

var probeRequests = newProbeLimiter(maxConcurrentProbes, maxConcurrentProbesPerHost)

func (l *probeLimiter) acquire(ctx context.Context, host string) (func(), error) {
	l.mu.Lock()
	h := l.hosts[host]
	if h == nil {
		h = &probeHostSlots{slots: make(chan struct{}, l.perHost)}
		l.hosts[host] = h
	}
	h.users++
	l.mu.Unlock()
	forget := func() {
		l.mu.Lock()
		h.users--
		if h.users == 0 {
			delete(l.hosts, host)
		}
		l.mu.Unlock()
	}
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
	// Wait for the host first so a busy upstream cannot occupy all global slots.
	select {
	case l.global <- struct{}{}:
	case <-ctx.Done():
		<-h.slots
		forget()
		return nil, ctx.Err()
	}
	return func() {
		<-l.global
		<-h.slots
		forget()
	}, nil
}

func doPost(url, key string, body []byte, timeout int) (int, []byte, int, string) {
	return doPostWithLimit(url, key, body, timeout, probeResponseLimit)
}

func doPostWithLimit(url, key string, body []byte, timeout int, limit int64) (int, []byte, int, string) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, "探测请求无法生成"
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	return doReqWithLimit(req, timeout, limit)
}

func doReq(req *http.Request, timeout int) (int, []byte, int, string) {
	return doReqWithLimit(req, timeout, probeResponseLimit)
}

func doReqWithLimit(req *http.Request, timeout int, limit int64) (int, []byte, int, string) {
	release, err := probeRequests.acquire(req.Context(), strings.ToLower(req.URL.Host))
	if err != nil {
		return 0, nil, 0, shortNetErr(err)
	}
	defer release()
	if timeout <= 0 {
		timeout = 30
	}
	// Queueing behind another probe does not consume the model's request timeout.
	ctx, cancel := context.WithTimeout(req.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	req = req.Clone(ctx)
	start := time.Now()
	resp, err := probeClient.Do(req)
	lat := int(time.Since(start).Milliseconds())
	if err != nil {
		return 0, nil, lat, shortNetErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, lat, "读取响应失败"
	}
	if int64(len(b)) > limit {
		return resp.StatusCode, nil, lat, fmt.Sprintf("响应体超过大小上限（%d 字节）", limit)
	}
	return resp.StatusCode, b, lat, ""
}

func shortNetErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "请求超时"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(s, "no such host"):
		return "域名无法解析"
	case strings.Contains(s, "connection reset"):
		return "连接被重置"
	case strings.Contains(s, "tls"), strings.Contains(s, "certificate"):
		return "证书校验失败"
	default:
		return "网络错误"
	}
}
