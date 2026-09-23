// Package metrics defines every Prometheus metric the Go services expose.
package metrics

import (
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// LLM latency buckets go up to 60s: default Prometheus buckets stop at 10s,
// which would hide exactly the slow calls you care about.
var llmBuckets = []float64{0.25, 0.5, 1, 2, 3, 5, 8, 13, 20, 30, 60}

var (
	LLMRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llm_request_duration_seconds", Help: "LLM chat call latency.", Buckets: llmBuckets,
	}, []string{"provider", "model", "status"})

	LLMTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total", Help: "Tokens consumed, by provider, model and kind (prompt/completion).",
	}, []string{"provider", "model", "kind"})

	LLMFallbacks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_fallback_total", Help: "Times a provider failed and the next one was tried.",
	}, []string{"from", "reason"})

	RateLimitWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "llm_ratelimit_wait_seconds", Help: "Time spent queued in the shared rate limiter.",
		Buckets: []float64{0, 0.1, 0.5, 1, 2, 5, 10, 20, 40},
	}, []string{"provider"})

	Decisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_decisions_total", Help: "Tool calls chosen by each agent.",
	}, []string{"agent", "tool"})

	EventsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agent_events_processed_total", Help: "World events processed by each agent.",
	}, []string{"agent", "type"})

	PerceptionLagTicks = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agent_perception_lag_ticks", Help: "Newest world tick minus the tick of the event the agent is acting on.",
	}, []string{"agent"})

	RAGSearches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_searches_total", Help: "Vector searches by outcome.",
	}, []string{"result"})

	RAGSearchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "rag_search_duration_seconds", Help: "Embedding + vector search latency.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	})

	RAGWrites = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rag_memories_written_total", Help: "Memories embedded and stored, by kind.",
	}, []string{"kind"})

	RAGDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "rag_memories_dropped_total", Help: "Memories dropped because the write queue was full or embedding failed.",
	})

	WSClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_ws_clients", Help: "Connected browser clients.",
	})

	GatewayForwarded = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_messages_forwarded_total", Help: "Messages pushed to browsers, by kind.",
	}, []string{"kind"})
)

// Serve exposes /metrics on addr (e.g. ":9101") in a background goroutine.
func Serve(addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("metrics server stopped: %v", err)
		}
	}()
}
