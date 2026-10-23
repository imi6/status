package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openCommunityDB(t *testing.T) {
	t.Helper()
	old := db
	opened, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "community.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = opened
	t.Cleanup(func() {
		db.Close()
		db = old
	})
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}
}

func TestDailyLotteryDrawsFromQualifiedPool(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 2
	conf.Winners = 1
	conf.MinPool = 1
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importPrizes("", "CDK-X"); err == nil {
		t.Fatal("prize without name should fail")
	}
	if n, err := importPrizes("5元额度", "CDK-A\nCDK-B\nCDK-A\n"); err != nil || n != 2 {
		t.Fatalf("import %d %v", n, err)
	}
	oldSend := communitySend
	var sent []string
	communitySend = func(token, chatID, text string, markup json.RawMessage) error {
		sent = append(sent, chatID+" "+text)
		return nil
	}
	t.Cleanup(func() { communitySend = oldSend })
	oldCfg := cfg
	cfg.Telegram.BotToken = "token"
	t.Cleanup(func() { cfg = oldCfg })
	oldShuffle := lotteryShuffle
	// Reverse order so the last pool entry wins deterministically.
	lotteryShuffle = func(n int, swap func(i, j int)) {
		for i := 0; i < n/2; i++ {
			swap(i, n-1-i)
		}
	}
	t.Cleanup(func() { lotteryShuffle = oldShuffle })

	day := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "hello"}
	recordSpeech(ada, day)
	if speechCount(7, "2026-10-17") != 0 {
		t.Fatal("speech before join was counted")
	}
	ada.Text = "/join"
	reply, _ := applyCommunity(ada, day)
	if !strings.Contains(reply, "当前共 1 人报名") || !strings.Contains(reply, "今日奖池 0 人") {
		t.Fatalf("join %q", reply)
	}
	bob := communityMessage{ChatID: -1001, UserID: 8, UserName: "bob", Text: "/join"}
	applyCommunity(bob, day)
	cat := communityMessage{ChatID: -1001, UserID: 9, UserName: "cat", Text: "/join"}
	applyCommunity(cat, day)
	for _, text := range []string{"a1", "a1", "a2"} {
		ada.Text = text
		recordSpeech(ada, day)
	}
	for _, text := range []string{"b1", "b2"} {
		bob.Text = text
		recordSpeech(bob, day)
	}
	cat.Text = "c1"
	recordSpeech(cat, day)
	cat.Text = "/pool"
	if reply, _ = applyCommunity(cat, day); !strings.Contains(reply, "今日奖池 2 人") || !strings.Contains(reply, "5元额度 × 2") {
		t.Fatalf("pool %q", reply)
	}

	settleDue(time.Date(2026, 10, 18, 0, 10, 0, 0, shanghai))
	var winner int64
	var used int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lottery_prizes WHERE status = 'used'`).Scan(&used)
	_ = db.QueryRow(`SELECT winner_id FROM lottery_prizes WHERE code = 'CDK-A'`).Scan(&winner)
	if used != 1 || winner != 8 {
		t.Fatalf("used %d winner %d", used, winner)
	}
	if !speechMember(7) || !speechMember(9) {
		t.Fatal("signups should stay after the draw")
	}
	dm, groupHasCode, announced := false, false, false
	for _, item := range sent {
		if strings.HasPrefix(item, "8 ") && strings.Contains(item, "CDK-A") && strings.Contains(item, "5元额度") {
			dm = true
		}
		if strings.HasPrefix(item, "-1001 ") {
			groupHasCode = groupHasCode || strings.Contains(item, "CDK-A")
			announced = announced || strings.Contains(item, "奖池 2 人")
		}
	}
	if !dm || groupHasCode || !announced {
		t.Fatalf("sent %#v", sent)
	}
	settleDue(time.Date(2026, 10, 18, 0, 20, 0, 0, shanghai))
	_ = db.QueryRow(`SELECT COUNT(*) FROM lottery_prizes WHERE status = 'used'`).Scan(&used)
	if used != 1 {
		t.Fatalf("day settled twice, used %d", used)
	}
	bob.Text = "/code"
	if _, code := applyCommunity(bob, day.Add(24*time.Hour)); code != "CDK-A" {
		t.Fatalf("resend %q", code)
	}
	if removed := deletePrizes([]int64{1, 2}); removed != 1 {
		t.Fatalf("removed %d", removed)
	}
}

func TestLotterySkipsSmallPool(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	if conf.MinPool != 100 {
		t.Fatalf("default min pool %d", conf.MinPool)
	}
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 1
	conf.MinPool = 2
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importPrizes("1元", "CDK-ONE"); err != nil {
		t.Fatal(err)
	}
	oldSend := communitySend
	var sent []string
	communitySend = func(token, chatID, text string, markup json.RawMessage) error {
		sent = append(sent, chatID+" "+text)
		return nil
	}
	t.Cleanup(func() { communitySend = oldSend })
	oldCfg := cfg
	cfg.Telegram.BotToken = "token"
	t.Cleanup(func() { cfg = oldCfg })

	day := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/join"}
	if reply, _ := applyCommunity(ada, day); !strings.Contains(reply, "奖池满 2 人才开奖") {
		t.Fatalf("join %q", reply)
	}
	ada.Text = "hi"
	recordSpeech(ada, day)
	settleDue(time.Date(2026, 10, 18, 0, 10, 0, 0, shanghai))
	if prizeCount() != 1 {
		t.Fatal("prize should stay when pool is too small")
	}
	var settled int
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_settle WHERE day = '2026-10-17'`).Scan(&settled)
	if settled != 1 || len(sent) != 1 || !strings.Contains(sent[0], "未满 2 人") {
		t.Fatalf("settled %d sent %#v", settled, sent)
	}

	day2 := day.Add(24 * time.Hour)
	bob := communityMessage{ChatID: -1001, UserID: 8, UserName: "bob", Text: "/join"}
	applyCommunity(bob, day2)
	ada.Text, bob.Text = "a", "b"
	recordSpeech(ada, day2)
	recordSpeech(bob, day2)
	settleDue(time.Date(2026, 10, 19, 0, 10, 0, 0, shanghai))
	if prizeCount() != 0 {
		t.Fatal("full pool should draw")
	}
}

func TestLotteryDailyPlan(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 1
	conf.Winners = 5
	conf.MinPool = 1
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importPrizes("10元额度", "TEN-A\nTEN-B"); err != nil {
		t.Fatal(err)
	}
	if _, err := importPrizes("30额度", "BIG-C"); err != nil {
		t.Fatal(err)
	}
	id := func(code string) int64 {
		var n int64
		_ = db.QueryRow(`SELECT id FROM lottery_prizes WHERE code = ?`, code).Scan(&n)
		return n
	}
	if _, err := planPrizes([]int64{id("BIG-C")}, "10-17"); err == nil {
		t.Fatal("bad date should fail")
	}
	if n, err := planPrizes([]int64{id("BIG-C")}, "2026-10-17"); err != nil || n != 1 {
		t.Fatalf("plan %d %v", n, err)
	}
	if _, err := planPrizes([]int64{id("TEN-B")}, "2026-10-20"); err != nil {
		t.Fatal(err)
	}
	oldSend := communitySend
	communitySend = func(token, chatID, text string, markup json.RawMessage) error { return nil }
	t.Cleanup(func() { communitySend = oldSend })
	oldCfg := cfg
	cfg.Telegram.BotToken = "token"
	t.Cleanup(func() { cfg = oldCfg })

	speak := func(day time.Time, ids ...int64) {
		for _, uid := range ids {
			m := communityMessage{ChatID: -1001, UserID: uid, UserName: fmt.Sprintf("u%d", uid), Text: "/join"}
			applyCommunity(m, day)
			m.Text = "hi"
			recordSpeech(m, day)
		}
	}
	day1 := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai)
	speak(day1, 7, 8, 9)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "u7", Text: "/pool"}
	if reply, _ := applyCommunity(ada, day1); !strings.Contains(reply, "次日抽 1 人") || !strings.Contains(reply, "30额度 × 1") || strings.Contains(reply, "10元额度") {
		t.Fatalf("pool %q", reply)
	}
	settleDue(time.Date(2026, 10, 18, 0, 10, 0, 0, shanghai))
	var used int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lottery_prizes WHERE status = 'used'`).Scan(&used)
	var bigDay string
	_ = db.QueryRow(`SELECT day FROM lottery_prizes WHERE code = 'BIG-C' AND status = 'used'`).Scan(&bigDay)
	if used != 1 || bigDay != "2026-10-17" {
		t.Fatalf("planned day: used %d big day %q", used, bigDay)
	}

	day2 := day1.Add(24 * time.Hour)
	speak(day2, 7, 8)
	settleDue(time.Date(2026, 10, 19, 0, 10, 0, 0, shanghai))
	var status string
	_ = db.QueryRow(`SELECT status FROM lottery_prizes WHERE code = 'TEN-A'`).Scan(&status)
	if status != "used" {
		t.Fatalf("unplanned prize should go on an unplanned day, got %q", status)
	}
	_ = db.QueryRow(`SELECT status FROM lottery_prizes WHERE code = 'TEN-B'`).Scan(&status)
	if status != "available" {
		t.Fatal("prize reserved for 10-20 was handed out early")
	}
}

func TestLotteryWaitsForStock(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 1
	conf.MinPool = 1
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/join"}
	applyCommunity(ada, day)
	ada.Text = "hi"
	recordSpeech(ada, day)
	settleDue(time.Date(2026, 10, 18, 0, 10, 0, 0, shanghai))
	var settled int
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_settle`).Scan(&settled)
	if settled != 0 {
		t.Fatal("should wait for prizes before settling")
	}
	if _, err := importPrizes("1元", "CDK-LATE"); err != nil {
		t.Fatal(err)
	}
	settleDue(time.Date(2026, 10, 18, 0, 20, 0, 0, shanghai))
	var winner int64
	_ = db.QueryRow(`SELECT winner_id FROM lottery_prizes WHERE code = 'CDK-LATE'`).Scan(&winner)
	if winner != 7 {
		t.Fatalf("winner %d", winner)
	}
}

func TestOldCodesMoveToPrizes(t *testing.T) {
	openCommunityDB(t)
	if _, err := db.Exec(`INSERT INTO reward_codes (code, tier, amount_cents, status) VALUES ('OLD-1', '1元', 100, 'available'), ('OLD-2', '1元', 100, 'used')`); err != nil {
		t.Fatal(err)
	}
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var name string
	_ = db.QueryRow(`SELECT name FROM lottery_prizes WHERE code = 'OLD-1' AND status = 'available'`).Scan(&name)
	if name != "1元" || prizeCount() != 1 {
		t.Fatalf("name %q left %d", name, prizeCount())
	}
	var old int
	_ = db.QueryRow(`SELECT COUNT(*) FROM reward_codes`).Scan(&old)
	if old != 1 {
		t.Fatalf("old codes left %d, want only the used one", old)
	}
}

func TestSpeechDashboard(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 2
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/join"}
	applyCommunity(ada, now)
	ada.Text = "hello"
	recordSpeech(ada, now)
	recordSpeech(ada, now)
	ada.Text = "world"
	recordSpeech(ada, now)
	dash := speechDashboard(now)
	if dash.Messages != 2 || dash.Speakers != 1 {
		t.Fatalf("dash %+v", dash)
	}
	if len(dash.Days) != 1 || dash.Days[0].Messages != 2 {
		t.Fatalf("days %+v", dash.Days)
	}
}

func TestSpeechLinesCleanup(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinChars, conf.Cooldown = 1, 0
	if !conf.CleanLines {
		t.Fatal("cleanup should be on by default")
	}
	conf.Enabled = true
	conf.ChatID = "-1001"
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/join"}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, shanghai)
	applyCommunity(ada, now)
	for _, at := range []time.Time{now.Add(-72 * time.Hour), now.Add(-24 * time.Hour), now} {
		ada.Text = "hi " + at.Format("0102")
		recordSpeech(ada, at)
	}
	lines := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM daily_speech_lines`).Scan(&n)
		return n
	}
	if n := pruneSpeechLines(now); n != 1 || lines() != 2 {
		t.Fatalf("pruned %d left %d", n, lines())
	}
	if n, err := clearSpeechLines(now); err != nil || n != 1 || lines() != 1 {
		t.Fatalf("cleared %d left %d err %v", n, lines(), err)
	}
	// Today's line survives, so the same text still does not count twice.
	ada.Text = "hi 1020"
	recordSpeech(ada, now.Add(time.Minute))
	if speechCount(7, "2026-10-20") != 1 {
		t.Fatalf("count %d", speechCount(7, "2026-10-20"))
	}
	if speechCount(7, "2026-10-17") != 1 {
		t.Fatal("counts of a day not yet drawn must survive cleanup")
	}
	if _, err := db.Exec(`INSERT INTO daily_settle (day, participants, rewarded, settled_at) VALUES ('2026-10-17', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	pruneSpeechLines(now)
	if speechCount(7, "2026-10-17") != 0 || speechCount(7, "2026-10-19") != 1 {
		t.Fatalf("drawn old day should go, recent stays: %d %d", speechCount(7, "2026-10-17"), speechCount(7, "2026-10-19"))
	}
}

func TestSpeechWorth(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1", "", false},
		{"12345", "", false},
		{"哈哈哈哈", "", false},
		{"。。。", "", false},
		{"好的", "", false},
		{"今天天气不错！", "今天天气不错", true},
		{"Hello, World", "helloworld", true},
		{"gpt5 好用", "gpt5好用", true},
	}
	for _, c := range cases {
		got, ok := speechWorth(c.in, 3)
		if got != c.want || ok != c.ok {
			t.Errorf("speechWorth(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSpeechSpamFilters(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	if conf.MinChars != 3 || conf.Cooldown != 10 {
		t.Fatalf("defaults %d %d", conf.MinChars, conf.Cooldown)
	}
	conf.Enabled = true
	conf.ChatID = "-1001"
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 19, 28, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/join"}
	applyCommunity(ada, now)
	for i, text := range []string{"1", "2", "3", "4", "5"} {
		ada.Text = text
		recordSpeech(ada, now.Add(time.Duration(i)*time.Minute))
	}
	if n := speechCount(7, "2026-10-23"); n != 0 {
		t.Fatalf("digit spam counted %d", n)
	}
	ada.Text = "今天天气不错"
	recordSpeech(ada, now.Add(10*time.Minute))
	ada.Text = "晚上一起打游戏吗"
	recordSpeech(ada, now.Add(10*time.Minute+5*time.Second))
	ada.Text = "今天天气不错！！"
	recordSpeech(ada, now.Add(11*time.Minute))
	ada.Text = "晚上一起打游戏吗"
	if n := speechCount(7, "2026-10-23"); n != 1 {
		t.Fatalf("punctuation dupe or fast line counted, got %d", n)
	}
	recordSpeech(ada, now.Add(12*time.Minute))
	if n := speechCount(7, "2026-10-23"); n != 2 {
		t.Fatalf("line after cooldown should count, got %d", n)
	}
	conf.Cooldown = -1
	if err := saveCommunityConfig(conf); err == nil {
		t.Fatal("negative cooldown should fail")
	}
}
