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

func TestDailySpeechNeedsJoinedQuorum(t *testing.T) {
	openCommunityDB(t)
	conf := defaultCommunityConfig()
	conf.Enabled = true
	conf.ChatID = "-1001"
	conf.SpeechNeed = 2
	conf.MinSpeakers = 2
	conf.SpeechYuan = "1"
	if err := saveCommunityConfig(conf); err != nil {
		t.Fatal(err)
	}
	if _, err := importCodes(100, "CDK-A\nCDK-B\n"); err != nil {
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

	day1 := time.Date(2026, 10, 17, 12, 0, 0, 0, shanghai)
	ada := communityMessage{ChatID: -1001, UserID: 7, UserName: "ada", Text: "hello"}
	recordSpeech(ada, day1)
	if speechCount(7, "2026-10-17") != 0 {
		t.Fatal("speech before join was counted")
	}
	ada.Text = "/join"
	reply, _ := applyCommunity(ada, day1)
	if !strings.Contains(reply, "当前报名 1/2") {
		t.Fatalf("join %q", reply)
	}
	recordSpeech(ada, day1)
	ada.Text = "one"
	recordSpeech(ada, day1)
	ada.Text = "one"
	recordSpeech(ada, day1)
	ada.Text = "two"
	recordSpeech(ada, day1)
	if speechCount(7, "2026-10-17") != 2 {
		t.Fatalf("ada count %d", speechCount(7, "2026-10-17"))
	}
	settleDue(time.Date(2026, 10, 18, 0, 10, 0, 0, shanghai))
	if speechMember(7) {
		t.Fatal("signup should be void after missing headcount")
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reward_codes WHERE status = 'available'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Fatalf("codes left %d", left)
	}

	day2 := time.Date(2026, 10, 18, 12, 0, 0, 0, shanghai)
	ada.Text = "/join"
	if reply, _ = applyCommunity(ada, day2); !strings.Contains(reply, "当前报名 1/2") {
		t.Fatalf("rejoin %q", reply)
	}
	bob := communityMessage{ChatID: -1001, UserID: 8, UserName: "bob", Text: "/join"}
	reply, code := applyCommunity(bob, day2)
	if code != "" || !strings.Contains(reply, "当前报名 2/2") {
		t.Fatalf("bob join %q", reply)
	}
	ada.Text = "a1"
	recordSpeech(ada, day2)
	ada.Text = "a2"
	recordSpeech(ada, day2)
	bob.Text = "b1"
	recordSpeech(bob, day2)
	settleDue(time.Date(2026, 10, 19, 0, 10, 0, 0, shanghai))
	var usedBy string
	if err := db.QueryRow(`SELECT used_by FROM reward_codes WHERE code = 'CDK-A'`).Scan(&usedBy); err != nil {
		t.Fatal(err)
	}
	if usedBy != "7" {
		t.Fatalf("used by %s", usedBy)
	}
	var bobCode string
	_ = db.QueryRow(`SELECT code FROM reward_codes WHERE used_by = '8'`).Scan(&bobCode)
	if bobCode != "" {
		t.Fatalf("bob got %s", bobCode)
	}
	dm, groupHasCode := false, false
	for _, item := range sent {
		if strings.HasPrefix(item, "7 ") && strings.Contains(item, "CDK-A") {
			dm = true
		}
		if strings.HasPrefix(item, "-1001 ") && strings.Contains(item, "CDK-A") {
			groupHasCode = true
		}
	}
	if !dm || groupHasCode {
		t.Fatalf("sent %#v", sent)
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
