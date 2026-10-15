package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBucketPointsHour(t *testing.T) {
	now := time.Date(2026, 10, 15, 10, 20, 0, 0, time.UTC)
	hour := now.Truncate(time.Hour)
	points := []probePoint{
		{up: true, lat: 1000, ts: hour.Add(10 * time.Minute).Unix()},
		{up: false, lat: 3000, ts: hour.Add(15 * time.Minute).Unix()},
	}
	buckets := bucketPoints("hour", now, points)
	if len(buckets) != 24 {
		t.Fatalf("len %d", len(buckets))
	}
	last := buckets[23]
	if last.Samples != 2 || last.Up != 1 || last.Latency != 2000 {
		t.Fatalf("%+v", last)
	}
	if last.Rate < 49.9 || last.Rate > 50.1 {
		t.Fatalf("rate %v", last.Rate)
	}
	if buckets[0].Samples != 0 {
		t.Fatalf("empty bucket %+v", buckets[0])
	}
}

func TestReplyParsersRequireModelText(t *testing.T) {
	if !openaiCompatHasReply([]byte(`{"choices":[{"message":{"content":"h"}}]}`)) {
		t.Fatal("openai content")
	}
	if openaiCompatHasReply([]byte(`{"choices":[{"message":{"content":""}}]}`)) {
		t.Fatal("empty content")
	}
	if openaiCompatHasReply([]byte(`{"error":{"message":"model not found"},"choices":[{"message":{"content":"h"}}]}`)) {
		t.Fatal("error body")
	}
	if !openaiResponseHasReply([]byte(`{"output_text":"h"}`)) {
		t.Fatal("responses output_text")
	}
	if !googleHasReply([]byte(`{"candidates":[{"content":{"parts":[{"text":"h"}]}}]}`)) {
		t.Fatal("google text")
	}
	if !anthropicHasReply([]byte(`{"content":[{"type":"text","text":"h"}]}`)) {
		t.Fatal("anthropic text")
	}
	if anthropicHasReply([]byte(`{"ok":true}`)) {
		t.Fatal("unrelated json")
	}
}

func TestCheckOpenAICompatRequiresModelText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/v1/chat/completions" && r.Header.Get("Authorization") == "Bearer sk" {
			var body struct {
				Model    string `json:"model"`
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				MaxTokens int `json:"max_tokens"`
			}
			raw, _ := io.ReadAll(r.Body)
			if json.Unmarshal(raw, &body) != nil || body.Model != "GPT-PRO" || body.MaxTokens != probeMaxTokens || len(body.Messages) != 1 || body.Messages[0].Content != probePrompt {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	up, _ := checkOpenAICompat(Model{ID: "GPT-PRO", BaseURL: srv.URL + "/v1", Key: "sk", Timeout: 5, Provider: "openai-compat"})
	if !up {
		t.Fatal("model text should be up")
	}
	up, _ = checkOpenAICompat(Model{ID: "GPT-PRO", BaseURL: srv.URL + "/empty", Key: "sk", Timeout: 5, Provider: "openai-compat"})
	if up {
		t.Fatal("HTTP 200 without model text should be down")
	}
}
