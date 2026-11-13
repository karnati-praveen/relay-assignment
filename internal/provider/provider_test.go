package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestModelOverridesProviderDefault(t *testing.T) {
	var gotModel string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()
	g := NewGroq("test", "default-model")
	g.Base = ts.URL
	g.HTTP = ts.Client()
	res := g.Chat(context.Background(), Request{Model: "step-model", MaxTokens: 5})
	if res.Status != "ok" || res.Text != "ok" || gotModel != "step-model" {
		t.Fatalf("step model was not preserved: model=%q result=%+v", gotModel, res)
	}
	if res.TTFTMs <= 0 {
		t.Fatal("content token did not set TTFT")
	}
}

// A Groq HTTP 200 can carry a JSON-validation failure as an in-stream error
// event (observed for json_validate_failed when the completion budget runs out
// before valid JSON is produced). The client must treat that as a failure, not
// as a successful empty completion, or the pipeline silently skips checking.
func TestStreamErrorEventIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"Failed to validate JSON. Please adjust your prompt.\",\"type\":\"invalid_request_error\",\"code\":\"json_validate_failed\"}}\n\n"))
	}))
	defer ts.Close()
	g := NewGroq("test", "default-model")
	g.Base = ts.URL
	g.HTTP = ts.Client()
	res := g.Chat(context.Background(), Request{MaxTokens: 5})
	if res.Status != "error" {
		t.Fatalf("stream error event classified as %q (text=%q)", res.Status, res.Text)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "json_validate_failed") {
		t.Fatalf("provider error message was not preserved: %v", res.Err)
	}
	if res.SawDone {
		t.Fatal("stream error should stop parsing before [DONE]")
	}
}

func TestHTTPTimeoutIsAProviderTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer ts.Close()
	g := NewGroq("test", "default-model")
	g.Base = ts.URL
	g.HTTP = &http.Client{Timeout: time.Millisecond}
	res := g.Chat(context.Background(), Request{MaxTokens: 1})
	if res.Status != "timeout" {
		t.Fatalf("timeout classified as %q: %v", res.Status, res.Err)
	}
	if strings.Contains(res.Err.Error(), "test") {
		t.Fatal("error unexpectedly contains credentials")
	}
}
