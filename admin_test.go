package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"database/sql"
)

func TestMain(m *testing.M) {
	afterConfigChange = func() {}
	os.Exit(m.Run())
}

func TestUpdateModelKeepsKeyWhenBlank(t *testing.T) {
	c := Config{Groups: []Group{{
		Name: "G",
		SubGroups: []SubGroup{{
			Name: "S",
			Models: []Model{{
				ID: "m", Provider: "openai-compat", BaseURL: "https://api.example.com/v1", Key: "secret", Timeout: 30,
			}},
		}},
	}}}
	err := updateModel(&c, "G", "S", 0, Model{
		ID: "m", Provider: "openai-compat", BaseURL: "https://api.example.com/v1/", Key: "", Timeout: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := c.Groups[0].SubGroups[0].Models[0]
	if got.Key != "secret" {
		t.Fatalf("key = %q", got.Key)
	}
	if got.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("baseURL = %q", got.BaseURL)
	}
}

func TestAddModelRejectsBadURLAndDuplicate(t *testing.T) {
	c := Config{Groups: []Group{{Name: "G", SubGroups: []SubGroup{{Name: "S", Models: []Model{}}}}}}
	err := addModel(&c, "G", "S", Model{ID: `bad"id`, Provider: "openai-compat", BaseURL: "https://api.example.com/v1", Key: "k", Timeout: 30})
	if err == nil {
		t.Fatal("expected invalid id")
	}
	if err := addModel(&c, "G", "S", Model{ID: "m", Provider: "nope", BaseURL: "https://api.example.com", Key: "k"}); err == nil {
		t.Fatal("expected invalid provider")
	}
	if err := addModel(&c, "G", "S", Model{ID: "m", BaseURL: "ftp://api.example.com", Key: "k"}); err == nil {
		t.Fatal("expected invalid url")
	}
	if err := addModel(&c, "G", "S", Model{ID: "m", BaseURL: "https://api.example.com/v1", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := addModel(&c, "G", "S", Model{ID: "m", BaseURL: "https://api.example.com/v1", Key: "k2"}); err == nil {
		t.Fatal("expected duplicate")
	}
}

func TestAdminRoundTripHidesSecrets(t *testing.T) {
	dir := t.TempDir()
	oldPath := configPath
	configPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() {
		configPath = oldPath
		sessionsMu.Lock()
		sessions = map[string]time.Time{}
		sessionsMu.Unlock()
	})

	cfg = Config{
		Port: 8080, Interval: 60, ManualRefreshCooldown: 60, AdminPassword: "secret",
		Frontend: FrontendConfig{Title: "T", Footer: "f"},
		Groups:   []Group{},
	}
	if err := writeConfigFile(cfg); err != nil {
		t.Fatal(err)
	}
	oldDB := db
	opened, err := sql.Open("sqlite", filepath.Join(dir, "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	db = opened
	t.Cleanup(func() {
		db.Close()
		db = oldDB
	})
	if err := ensureSchema(); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/{action}", apiAdmin)
	mux.HandleFunc("/api/config", apiConfig)

	loginReq := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"nope"}`))
	loginReq.Host = "status.local"
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != 401 {
		t.Fatalf("bad password status %d", loginRec.Code)
	}

	evil := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"secret"}`))
	evil.Host = "status.local"
	evil.Header.Set("Origin", "https://evil.example")
	evilRec := httptest.NewRecorder()
	mux.ServeHTTP(evilRec, evil)
	if evilRec.Code != 403 {
		t.Fatalf("cross origin status %d", evilRec.Code)
	}

	okReq := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"password":"secret"}`))
	okReq.Host = "status.local"
	okRec := httptest.NewRecorder()
	mux.ServeHTTP(okRec, okReq)
	if okRec.Code != 200 {
		t.Fatalf("login %d %s", okRec.Code, okRec.Body.String())
	}
	cookies := okRec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("cookie = %+v", cookies)
	}

	add := httptest.NewRequest(http.MethodPost, "/api/admin/groups", strings.NewReader(`{"name":"OpenAI","manualRefresh":true}`))
	add.Host = "status.local"
	add.AddCookie(cookies[0])
	addRec := httptest.NewRecorder()
	mux.ServeHTTP(addRec, add)
	if addRec.Code != 200 {
		t.Fatalf("add group %d %s", addRec.Code, addRec.Body.String())
	}

	sub := httptest.NewRequest(http.MethodPost, "/api/admin/subgroups", strings.NewReader(`{"group":"OpenAI","name":"GPT","kumaSlug":""}`))
	sub.Host = "status.local"
	sub.AddCookie(cookies[0])
	subRec := httptest.NewRecorder()
	mux.ServeHTTP(subRec, sub)
	if subRec.Code != 200 {
		t.Fatalf("add sub %d %s", subRec.Code, subRec.Body.String())
	}

	modelReq := httptest.NewRequest(http.MethodPost, "/api/admin/models", strings.NewReader(`{"group":"OpenAI","subgroup":"GPT","model":{"id":"gpt","provider":"openai-compat","baseURL":"https://api.example.com/v1","key":"sk-test-key","timeout":30}}`))
	modelReq.Host = "status.local"
	modelReq.AddCookie(cookies[0])
	modelRec := httptest.NewRecorder()
	mux.ServeHTTP(modelRec, modelReq)
	if modelRec.Code != 200 {
		t.Fatalf("add model %d %s", modelRec.Code, modelRec.Body.String())
	}

	unauth := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	unauth.Host = "status.local"
	unauthRec := httptest.NewRecorder()
	mux.ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != 401 {
		t.Fatalf("unauth config %d", unauthRec.Code)
	}

	pub := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	pubRec := httptest.NewRecorder()
	mux.ServeHTTP(pubRec, pub)
	pubBody, _ := io.ReadAll(pubRec.Body)
	if strings.Contains(string(pubBody), "sk-test-key") || strings.Contains(string(pubBody), "secret") {
		t.Fatalf("public config leaked secret: %s", pubBody)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-test-key") {
		t.Fatalf("config file still has key: %s", raw)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AdminPassword != "secret" || len(saved.Groups) != 0 {
		t.Fatalf("saved = %+v", saved)
	}
	var key string
	if err := db.QueryRow(`SELECT api_key FROM monitor_models WHERE model_id = ?`, "gpt").Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "sk-test-key" {
		t.Fatalf("db key = %q", key)
	}
}

func TestListUpstreamModels(t *testing.T) {
	var gotKey string
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o"},{"id":"bad\"id"},{"id":"GPT-PRO"}]}`)
	}))
	defer openai.Close()
	ids, err := listUpstreamModels("openai-compat", openai.URL+"/v1/", "sk")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "GPT-PRO,gpt-4o" {
		t.Fatalf("ids = %#v", ids)
	}

	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-2.5-pro"}]}`)
	}))
	defer google.Close()
	ids, err = listUpstreamModels("google", google.URL+"/v1beta", "g-key")
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "g-key" || strings.Join(ids, ",") != "gemini-2.0-flash,gemini-2.5-pro" {
		t.Fatalf("key=%q ids=%#v", gotKey, ids)
	}

	if _, err := listUpstreamModels("openai-compat", "ftp://example.com", "sk"); err == nil {
		t.Fatal("expected invalid url")
	}
	ids, err = listUpstreamModels("openai-compat", openai.URL, "sk")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "GPT-PRO,gpt-4o" {
		t.Fatalf("root url ids = %#v", ids)
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		provider string
		in       string
		want     string
	}{
		{"openai-compat", "https://api.example.com", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/v1", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/v1/", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/v1/models", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/v1/chat/completions", "https://api.example.com/v1"},
		{"openai-compat", "https://api.example.com/api", "https://api.example.com/api/v1"},
		{"anthropic", "https://api.anthropic.com", "https://api.anthropic.com/v1"},
		{"google", "https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta"},
		{"google", "https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta"},
	}
	for _, tc := range cases {
		got, err := normalizeBaseURL(tc.provider, tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("%s %s => %q err=%v, want %s", tc.provider, tc.in, got, err, tc.want)
		}
	}
}

func TestAdminPathReplacesDefaultRoute(t *testing.T) {
	if _, err := cleanAdminPath("admin"); err == nil {
		t.Fatal("expected common path to be rejected")
	}
	if name, err := cleanAdminPath("/panel-9f3k/"); err != nil || name != "panel-9f3k" {
		t.Fatalf("name=%q err=%v", name, err)
	}

	old := configPath
	configPath = filepath.Join(t.TempDir(), "missing.json")
	t.Cleanup(func() { configPath = old })

	mux := http.NewServeMux()
	base, err := mountAdmin(mux, "panel-9f3k")
	if err != nil || base != "/panel-9f3k" {
		t.Fatalf("base=%q err=%v", base, err)
	}

	for _, path := range []string{"/admin", "/api/admin/login"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status %d", path, rec.Code)
		}
	}

	page := httptest.NewRecorder()
	mux.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/panel-9f3k", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "管理后台") {
		t.Fatalf("custom page status %d", page.Code)
	}

	session := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel-9f3k/api/session", nil)
	req.Host = "status.local"
	mux.ServeHTTP(session, req)
	if session.Code != http.StatusOK || !strings.Contains(session.Body.String(), `"enabled"`) {
		t.Fatalf("custom api status %d body %s", session.Code, session.Body.String())
	}
}
