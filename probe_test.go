package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"h"}}]}`)
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
