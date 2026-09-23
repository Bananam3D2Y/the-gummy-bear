// Package rag is long-term memory + village knowledge, stored as vectors in
// Redis (Redis Stack's search module). Two kinds of documents live in one index:
//
//	lore    - chunks of the markdown files in brain/lore/, visible to every agent
//	memory  - an agent's own past experiences, visible only to that agent
//
// Each decision embeds a query built from "what just happened" and pulls the
// top-k nearest documents into the prompt. That keeps prompts a fixed size no
// matter how long the simulation runs.
package rag

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"worldcraft/internal/llm"
	"worldcraft/internal/metrics"
)

const indexName = "wc_rag"

type Hit struct {
	Kind  string
	Text  string
	Tick  string
	Score float64 // cosine distance: 0 = identical, 2 = opposite
}

type Store struct {
	rdb   *redis.Client
	llm   *llm.Client
	mu    sync.Mutex
	ready bool
	queue chan pending
}

type pending struct {
	kind, agent, text string
	tick              int64
}

func New(rdb *redis.Client, client *llm.Client) *Store {
	return &Store{rdb: rdb, llm: client, queue: make(chan pending, 256)}
}

// Enabled reports whether an embedding provider exists. Without one, the
// agents still work; they just run without long-term memory.
func (s *Store) Enabled() bool { return s.llm.CanEmbed() }

// ensureIndex creates the vector index the first time we learn the embedding size.
func (s *Store) ensureIndex(ctx context.Context, dim int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	err := s.rdb.Do(ctx, "FT.CREATE", indexName, "ON", "HASH", "PREFIX", "1", "rag:",
		"SCHEMA",
		"kind", "TAG",
		"agent", "TAG",
		"text", "TEXT",
		"tick", "NUMERIC",
		"embedding", "VECTOR", "HNSW", "6", "TYPE", "FLOAT32", "DIM", dim, "DISTANCE_METRIC", "COSINE",
	).Err()
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return fmt.Errorf("FT.CREATE failed (are you running redis-stack?): %w", err)
	}
	s.ready = true
	return nil
}

func toBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func (s *Store) store(ctx context.Context, items []pending) error {
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.text
	}
	vecs, err := s.llm.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if err := s.ensureIndex(ctx, len(vecs[0])); err != nil {
		return err
	}
	pipe := s.rdb.Pipeline()
	for i, it := range items {
		key := fmt.Sprintf("rag:%s:%s:%d:%d", it.kind, it.agent, time.Now().UnixNano(), i)
		pipe.HSet(ctx, key, "kind", it.kind, "agent", it.agent, "text", it.text,
			"tick", it.tick, "embedding", toBytes(vecs[i]))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	for _, it := range items {
		metrics.RAGWrites.WithLabelValues(it.kind).Inc()
	}
	return nil
}

// Remember queues a memory for embedding. It never blocks the agent: if the
// queue is full the memory is dropped and counted.
func (s *Store) Remember(agent string, tick int64, text string) {
	if !s.Enabled() {
		return
	}
	select {
	case s.queue <- pending{kind: "memory", agent: agent, text: text, tick: tick}:
	default:
		metrics.RAGDropped.Inc()
	}
}

// RunWriter batches queued memories (up to 16 per embedding call, flushed every
// 5s) so storing memories costs a few embedding requests per minute, not dozens.
func (s *Store) RunWriter(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var batch []pending
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.store(ctx, batch); err != nil {
			log.Printf("[rag] dropping %d memories: %v", len(batch), err)
			metrics.RAGDropped.Add(float64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-s.queue:
			batch = append(batch, p)
			if len(batch) >= 16 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Search returns the k documents nearest to query that this agent may see.
func (s *Store) Search(ctx context.Context, agent, query string, k int) ([]Hit, error) {
	if !s.Enabled() {
		return nil, nil
	}
	start := time.Now()
	defer func() { metrics.RAGSearchDuration.Observe(time.Since(start).Seconds()) }()

	vecs, err := s.llm.Embed(ctx, []string{query})
	if err != nil {
		metrics.RAGSearches.WithLabelValues("embed_error").Inc()
		return nil, err
	}
	if err := s.ensureIndex(ctx, len(vecs[0])); err != nil {
		metrics.RAGSearches.WithLabelValues("index_error").Inc()
		return nil, err
	}

	// Pre-filter to lore OR this agent's own memories, then KNN over what's left.
	q := fmt.Sprintf("(@agent:{%s | all})=>[KNN %d @embedding $vec AS score]", agent, k)
	res, err := s.rdb.Do(ctx, "FT.SEARCH", indexName, q,
		"PARAMS", "2", "vec", toBytes(vecs[0]),
		"SORTBY", "score", "RETURN", "4", "kind", "text", "tick", "score",
		"DIALECT", "2").Result()
	if err != nil {
		metrics.RAGSearches.WithLabelValues("search_error").Inc()
		return nil, err
	}
	hits := parseSearch(res)
	if len(hits) == 0 {
		metrics.RAGSearches.WithLabelValues("empty").Inc()
	} else {
		metrics.RAGSearches.WithLabelValues("hit").Inc()
	}
	return hits, nil
}

// parseSearch reads the RESP2 reply: [total, key1, [f1, v1, ...], key2, [...], ...]
func parseSearch(res any) []Hit {
	arr, ok := res.([]any)
	if !ok || len(arr) < 1 {
		return nil
	}
	var hits []Hit
	for i := 1; i+1 < len(arr); i += 2 {
		fields, ok := arr[i+1].([]any)
		if !ok {
			continue
		}
		h := Hit{}
		for j := 0; j+1 < len(fields); j += 2 {
			name, _ := fields[j].(string)
			val, _ := fields[j+1].(string)
			switch name {
			case "kind":
				h.Kind = val
			case "text":
				h.Text = val
			case "tick":
				h.Tick = val
			case "score":
				fmt.Sscanf(val, "%g", &h.Score)
			}
		}
		hits = append(hits, h)
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].Score < hits[b].Score })
	return hits
}

// IngestLore splits every .md file in dir into paragraphs and indexes them once.
// Delete the Redis key rag:lore:ingested (or run FLUSHALL) to re-ingest after editing lore.
func (s *Store) IngestLore(ctx context.Context, dir string) error {
	if !s.Enabled() {
		log.Printf("[rag] no embedding provider, skipping lore ingestion")
		return nil
	}
	if n, _ := s.rdb.Exists(ctx, "rag:lore:ingested").Result(); n == 1 {
		log.Printf("[rag] lore already ingested")
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	var chunks []pending
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		title := strings.TrimSuffix(filepath.Base(f), ".md")
		for _, para := range strings.Split(string(b), "\n\n") {
			para = strings.TrimSpace(para)
			if len(para) < 40 || strings.HasPrefix(para, "#") && !strings.Contains(para, "\n") {
				continue // skip headings and fragments
			}
			chunks = append(chunks, pending{kind: "lore", agent: "all", text: "(" + title + ") " + para})
		}
	}
	for i := 0; i < len(chunks); i += 16 {
		end := i + 16
		if end > len(chunks) {
			end = len(chunks)
		}
		if err := s.store(ctx, chunks[i:end]); err != nil {
			return err
		}
	}
	log.Printf("[rag] ingested %d lore chunks from %d files", len(chunks), len(files))
	return s.rdb.Set(ctx, "rag:lore:ingested", "1", 0).Err()
}
