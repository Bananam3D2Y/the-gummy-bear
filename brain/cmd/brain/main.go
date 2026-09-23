// Command brain runs one goroutine per villager, each with its own Kafka
// consumer group, plus the overseer-command router and the RAG writer.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"worldcraft/internal/agent"
	"worldcraft/internal/kafkax"
	"worldcraft/internal/llm"
	"worldcraft/internal/memory"
	"worldcraft/internal/metrics"
	"worldcraft/internal/protocol"
	"worldcraft/internal/rag"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	brokers := kafkax.Brokers(envOr("KAFKA_BROKERS", "localhost:9092"))
	metrics.Serve(envOr("METRICS_ADDR", ":9101"))

	// Protocol 2 (RESP2) keeps FT.SEARCH replies as plain arrays, which rag.parseSearch expects.
	rdb := redis.NewClient(&redis.Options{Addr: envOr("REDIS_ADDR", "localhost:6379"), Protocol: 2})
	for i := 0; ; i++ {
		if err := rdb.Ping(ctx).Err(); err == nil {
			break
		} else if i == 30 {
			log.Fatalf("redis unreachable: %v", err)
		}
		time.Sleep(time.Second)
	}

	client, err := llm.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("LLM providers (in fallback order): %v", client.ProviderNames())

	store := rag.New(rdb, client)
	go store.RunWriter(ctx)
	if err := store.IngestLore(ctx, envOr("LORE_DIR", "lore")); err != nil {
		log.Printf("[rag] lore ingestion failed, continuing without it: %v", err)
	}

	world := agent.NewWorldView()
	go world.Follow(ctx, brokers)

	writer := kafkax.NewWriter(brokers)
	defer writer.Close()

	deps := &agent.Deps{LLM: client, Memory: memory.New(rdb), RAG: store, World: world, Writer: writer, Brokers: brokers}
	agents := map[string]*agent.Agent{}
	for _, id := range []string{"miner", "blacksmith", "merchant"} {
		a := agent.New(id, deps)
		agents[id] = a
		go a.Run(ctx)
	}
	go routeOverseer(ctx, brokers, agents)

	log.Printf("brain running with %d agents (RAG enabled: %v)", len(agents), store.Enabled())
	<-ctx.Done()
	log.Printf("brain shutting down")
}

// routeOverseer delivers chat commands from the browser to the right agent.
func routeOverseer(ctx context.Context, brokers []string, agents map[string]*agent.Agent) {
	r := kafkax.NewGroupReader(brokers, protocol.TopicOverseer, "brain-overseer")
	defer r.Close()
	for {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[overseer] read: %v", err)
			time.Sleep(time.Second)
			continue
		}
		var cmd protocol.OverseerCommand
		if json.Unmarshal(m.Value, &cmd) != nil || cmd.Text == "" {
			continue
		}
		targets := []string{cmd.AgentID}
		if cmd.AgentID == "all" {
			targets = []string{"miner", "blacksmith", "merchant"}
		}
		for _, id := range targets {
			a, ok := agents[id]
			if !ok {
				continue
			}
			select {
			case a.Orders() <- cmd:
				log.Printf("[overseer] -> %s: %s", id, cmd.Text)
			default:
				log.Printf("[overseer] %s has too many pending orders, dropping", id)
			}
		}
	}
}
