package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestClassifyProbeStatus(t *testing.T) {
	old := db
	opened, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "checks.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = opened
	t.Cleanup(func() {
		opened.Close()
		db = old
	})
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	insert := func(model string, up int, ts time.Time) {
		_, err := db.Exec(
			`INSERT INTO checks (group_name, sub_name, model_id, up, latency, checked_at) VALUES (?,?,?,?,?,?)`,
			"G", "S", model, up, 10, ts.Unix(),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(model, wantStatus, wantLabel string) {
		t.Helper()
		status, label := classifyProbeStatus("G", "S", model, now)
		if status != wantStatus || label != wantLabel {
			t.Fatalf("%s: got %s %s, want %s %s", model, status, label, wantStatus, wantLabel)
		}
	}

	insert("ok", 1, now.Add(-40*time.Minute))
	insert("ok", 1, now.Add(-time.Minute))
	check("ok", "up", "正常")

	insert("blip", 1, now.Add(-40*time.Minute))
	insert("blip", 1, now.Add(-10*time.Minute))
	insert("blip", 0, now.Add(-2*time.Minute))
	insert("blip", 0, now)
	check("blip", "degraded", "降级")

	insert("wave", 1, now.Add(-40*time.Minute))
	insert("wave", 0, now.Add(-20*time.Minute))
	insert("wave", 1, now)
	check("wave", "degraded", "降级")

	insert("short", 1, now.Add(-50*time.Minute))
	insert("short", 0, now.Add(-20*time.Minute))
	insert("short", 0, now)
	check("short", "degraded", "降级")

	insert("out", 0, now.Add(-80*time.Minute))
	insert("out", 0, now.Add(-40*time.Minute))
	insert("out", 0, now)
	check("out", "down", "异常")
}
