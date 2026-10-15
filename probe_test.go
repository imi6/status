package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
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

func TestBucketPointsDayIsTwelve(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	buckets := bucketPoints("day", now, []probePoint{{up: true, lat: 1000, ts: now.Add(-2 * time.Hour).Unix()}})
	if len(buckets) != 12 {
		t.Fatalf("len %d", len(buckets))
	}
	shown := trimLeadingEmpty(buckets)
	if len(shown) != 1 || shown[0].Samples != 1 {
		t.Fatalf("shown %+v", shown)
	}
	weeks := bucketPoints("week", now, nil)
	if len(weeks) != 12 {
		t.Fatalf("weeks %d", len(weeks))
	}
	if len(trimLeadingEmpty(weeks)) != 0 {
		t.Fatal("empty weeks should stay hidden")
	}
}

func TestProbeBucketsAreIndividualChecks(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	points := make([]probePoint, 0, 15)
	for i := 15; i >= 1; i-- {
		points = append(points, probePoint{up: i != 1, lat: i * 1000, ts: now.Add(-time.Duration(i) * time.Minute).Unix()})
	}
	buckets := probeBuckets(now, points)
	if len(buckets) != 12 {
		t.Fatalf("len %d", len(buckets))
	}
	if buckets[0].Start != buckets[0].End || buckets[0].Samples != 1 || buckets[0].Latency != 12000 {
		t.Fatalf("oldest kept %+v", buckets[0])
	}
	if buckets[11].Up != 0 || buckets[11].Latency != 1000 {
		t.Fatalf("latest %+v", buckets[11])
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

func TestHydrateStatusShowsSavedGroups(t *testing.T) {
	oldDB := db
	oldCfg := cfg
	mu.Lock()
	oldStatus := statusV2
	mu.Unlock()
	opened, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "checks.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = opened
	t.Cleanup(func() {
		opened.Close()
		db = oldDB
		mu.Lock()
		cfg = oldCfg
		statusV2 = oldStatus
		mu.Unlock()
	})
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}
	checked := time.Now().Add(-time.Minute)
	if _, err := db.Exec(
		`INSERT INTO checks (group_name, sub_name, model_id, up, latency, checked_at) VALUES (?,?,?,?,?,?)`,
		"GPT", "GPT Pro", "gpt", 1, 1500, checked.Unix(),
	); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cfg = Config{Frontend: FrontendConfig{Title: "监控"}, Groups: []Group{{
		Name: "GPT",
		SubGroups: []SubGroup{
			{Name: "GPT Pro", Models: []Model{{ID: "gpt", Provider: "openai-compat"}}},
			{Name: "GPT Plus", Models: []Model{{ID: "plus", Provider: "openai-compat"}}},
		},
	}}}
	mu.Unlock()
	hydrateStatus()
	mu.RLock()
	defer mu.RUnlock()
	if len(statusV2.Sections) != 1 || len(statusV2.Sections[0].Rows) != 2 {
		t.Fatalf("sections %+v", statusV2.Sections)
	}
	pro := statusV2.Sections[0].Rows[0]
	if pro.SubName != "GPT Pro" || pro.Status != "up" || pro.LatencyMs != 1500 {
		t.Fatalf("pro %+v", pro)
	}
	if statusV2.Sections[0].Rows[1].SubName != "GPT Plus" || statusV2.Sections[0].Rows[1].Status != "unknown" {
		t.Fatalf("plus %+v", statusV2.Sections[0].Rows[1])
	}
}

func TestClearProbeHistory(t *testing.T) {
	oldDB := db
	oldCfg := cfg
	oldEpoch := atomic.LoadUint64(&historyEpoch)
	oldApplied := atomic.LoadUint64(&appliedHistoryEpoch)
	mu.Lock()
	oldStatus := statusV2
	mu.Unlock()
	opened, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "checks.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = opened
	t.Cleanup(func() {
		opened.Close()
		db = oldDB
		atomic.StoreUint64(&historyEpoch, oldEpoch)
		atomic.StoreUint64(&appliedHistoryEpoch, oldApplied)
		mu.Lock()
		cfg = oldCfg
		statusV2 = oldStatus
		mu.Unlock()
	})
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}
	checked := time.Now().Add(-time.Minute)
	if _, err := db.Exec(
		`INSERT INTO checks (group_name, sub_name, model_id, up, latency, checked_at) VALUES (?,?,?,?,?,?)`,
		"GPT", "GPT Pro", "gpt", 1, 1500, checked.Unix(),
	); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cfg = Config{Frontend: FrontendConfig{Title: "监控"}, Groups: []Group{{
		Name:      "GPT",
		SubGroups: []SubGroup{{Name: "GPT Pro", Models: []Model{{ID: "gpt", Provider: "openai-compat"}}}},
	}}}
	mu.Unlock()
	hydrateStatus()
	n, err := clearProbeHistory()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d", n)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("left %d", left)
	}
	mu.RLock()
	defer mu.RUnlock()
	if len(statusV2.Sections) != 1 || len(statusV2.Sections[0].Rows) != 1 {
		t.Fatalf("sections %+v", statusV2.Sections)
	}
	row := statusV2.Sections[0].Rows[0]
	if row.Status != "unknown" || row.LatencyMs != 0 || row.LastCheckedAt != nil || len(row.Timelines["probe"]) != 0 {
		t.Fatalf("row %+v", row)
	}
}

func TestApplyConfigOrder(t *testing.T) {
	groups := []Group{
		{Name: "GPT", SubGroups: []SubGroup{
			{Name: "Plus", Models: []Model{{ID: "plus"}}},
			{Name: "Pro", Models: []Model{{ID: "pro-a"}, {ID: "pro-b"}}},
		}},
		{Name: "GLM", SubGroups: []SubGroup{{Name: "GLM", Models: []Model{{ID: "glm"}}}}},
		{Name: "国模聚合", SubGroups: []SubGroup{{Name: "聚合", Models: []Model{{ID: "mix"}}}}},
	}
	sections := []Section{
		{Name: "国模聚合", Rows: []GroupResp{{Name: "mix", SubName: "聚合"}}},
		{Name: "GPT", Rows: []GroupResp{
			{Name: "pro-b", SubName: "Pro"},
			{Name: "plus", SubName: "Plus"},
			{Name: "pro-a", SubName: "Pro"},
		}},
		{Name: "GLM", Rows: []GroupResp{{Name: "glm", SubName: "GLM"}}},
	}
	got := applyConfigOrder(sections, groups)
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	if names[0] != "GPT" || names[1] != "GLM" || names[2] != "国模聚合" {
		t.Fatalf("groups %v", names)
	}
	rows := got[0].Rows
	if rows[0].SubName != "Plus" || rows[1].Name != "pro-a" || rows[2].Name != "pro-b" {
		t.Fatalf("rows %+v", rows)
	}
}

func TestOverallFromRows(t *testing.T) {
	row := func(status string) GroupResp { return GroupResp{Status: status} }
	sec := func(rows ...GroupResp) Section { return Section{Rows: rows} }
	check := func(wantStatus, wantLabel string, sections ...Section) {
		t.Helper()
		status, label := overallFromRows(sections)
		if status != wantStatus || label != wantLabel {
			t.Fatalf("got %s %s, want %s %s", status, label, wantStatus, wantLabel)
		}
	}
	check("up", "运行正常", sec(row("up"), row("up"), row("up"), row("up"), row("degraded")))
	check("degraded", "降级", sec(row("degraded"), row("degraded"), row("degraded"), row("up"), row("up")))
	check("degraded", "降级", sec(row("degraded")))
	check("down", "部分异常", sec(row("up"), row("up"), row("up"), row("up"), row("down")))
}
