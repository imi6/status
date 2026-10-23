package main

import (
	"database/sql"
	"encoding/json"
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

func TestCommunityRedeem(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.Enabled = true
	conf.ChatID = "-1001"
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importCodes(100, "CDK-A\nCDK-B\nCDK-A\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := importCodes(50, "CDK-HALF"); err == nil {
		t.Fatal("half yuan should be below the 10 point minimum")
	}
	now := time.Date(2026, 10, 18, 13, 0, 0, 0, shanghai)
	reply, code := applyCommunity(communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/checkin gpt-image-2 10:41 ok"}, now)
	if reply != "" || code != "" {
		t.Fatalf("removed command reply %q code %q", reply, code)
	}
	if err := addPoints(7, "@ada", 1); err != nil {
		t.Fatal(err)
	}
	redeem := communityMessage{ChatID: 7, UserID: 7, UserName: "ada", Text: "/redeem 1"}
	reply, code = applyCommunity(redeem, now)
	if code != "" || !strings.Contains(reply, "积分不够") {
		t.Fatalf("early redeem %q code %q", reply, code)
	}
	if err := addPoints(7, "@ada", 9); err != nil {
		t.Fatal(err)
	}
	groupRedeem := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "/redeem 1"}
	reply, code = applyCommunity(groupRedeem, now.Add(time.Minute))
	if code != "" || !strings.Contains(reply, "私聊") || pointBalance(7) != 10 {
		t.Fatalf("group redeem %q code %q balance %d", reply, code, pointBalance(7))
	}
	oldSend := communitySend
	var notices []string
	communitySend = func(token, chatID, text string, markup json.RawMessage) error {
		notices = append(notices, chatID+" "+text)
		return nil
	}
	t.Cleanup(func() { communitySend = oldSend })
	oldCfg := cfg
	cfg.Telegram.BotToken = "token"
	t.Cleanup(func() { cfg = oldCfg })
	reply, code = applyCommunity(redeem, now.Add(2*time.Minute))
	if code != "CDK-A" || strings.Contains(reply, "CDK-A") || !strings.Contains(reply, "1元") {
		t.Fatalf("redeem reply %q code %q", reply, code)
	}
	if pointBalance(7) != 0 {
		t.Fatalf("balance after redeem %d", pointBalance(7))
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "-1001") || !strings.Contains(notices[0], "1元") || strings.Contains(notices[0], "CDK-A") {
		t.Fatalf("group notice %#v", notices)
	}
}

func TestDailyLotteryDrawsFromQualifiedPool(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 2
	conf.Winners = 1
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

func TestLotteryWaitsForStock(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 1
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

func TestDeleteAvailableCodes(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.MinPoints = 1
	conf.RatePoints = 1
	conf.RateYuan = "1"
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importCodes(100, "CDK-A\nCDK-B\nCDK-C\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE reward_codes SET status = 'used' WHERE code = 'CDK-C'`); err != nil {
		t.Fatal(err)
	}
	removed, err := deleteAvailableCodes(nil, "CDK-A\nCDK-C\n", 0, false)
	if err != nil || removed != 1 {
		t.Fatalf("removed %d err %v", removed, err)
	}
	removed, err = deleteAvailableCodes(nil, "", 100, true)
	if err != nil || removed != 1 {
		t.Fatalf("amount removed %d err %v", removed, err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reward_codes`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("left %d", left)
	}
}

func TestSpeechDashboard(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
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
	if dash.Messages != 2 || dash.Speakers != 1 || len(dash.Users) != 1 || dash.Users[0].Today != 2 || dash.Users[0].Total != 2 {
		t.Fatalf("dash %+v users %+v", dash, dash.Users)
	}
	if len(dash.Lines) != 2 || len(dash.Days) != 1 || dash.Days[0].Messages != 2 {
		t.Fatalf("lines %d days %+v", len(dash.Lines), dash.Days)
	}
}
