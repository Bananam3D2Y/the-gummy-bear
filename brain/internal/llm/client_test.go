package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"worldcraft/internal/ratelimit"
)

func TestFallback(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429); w.Write([]byte(`{"error":"quota"}`)) }))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path == "/embeddings" {
			w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"mine","arguments":"{\"thought\":\"dig\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	c := &Client{http: http.DefaultClient, providers: []*Provider{{Name: "a", BaseURL: bad.URL + "/", Model: "m", limiter: ratelimit.New(600)}, {Name: "b", BaseURL: good.URL + "/", Model: "m2", EmbedModel: "e", limiter: ratelimit.New(600)}}}
	c.embedder = c.providers[1]
	c.embedLimit = ratelimit.New(600)
	res, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || res.Provider != "b" || res.Message.ToolCalls[0].Function.Name != "mine" {
		t.Fatal(err, res)
	}
	v, err := c.Embed(context.Background(), []string{"x"})
	if err != nil || len(v[0]) != 2 {
		t.Fatal(err)
	}
}
