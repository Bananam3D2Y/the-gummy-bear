# Design notes

The Gummy Bear is a multi-agent simulation. A C++ engine runs a restaurant kitchen at 20 ticks per second; four LLM-driven cooks work in it, each as a pair of Go goroutines; the Owner talks to them and gives them orders from a browser. Kafka carries every event, Redis holds memory, and Grafana shows the metrics.

The framing that shaped every decision: **a fast world and slow minds.** The world updates every 50 ms. A mind takes anywhere from under a second to twenty seconds to decide. The design exists to make those two speeds work together.

## Components

```
Browser ⇄ Go gateway (:8080) ⇄ Kafka ⇄ C++ engine (:9100)
                                  ⇅
                          Go brain (:9101) ⇄ Redis (memory + vectors)
                                            ⇄ Groq / Gemini / Ollama
Prometheus scrapes engine, brain, and kafka-exporter; Grafana reads Prometheus.
```

| Topic | Direction | Notes |
| --- | --- | --- |
| `world.snapshots` | engine → gateway, brain | Full world state, 10 Hz |
| `world.events` | engine → brain | Keyed by cook, so each cook's stream stays ordered |
| `agent.actions` | brain → engine | Every action carries `based_on_tick` |
| `overseer.commands` | gateway → brain | Owner chat and standing orders |
| `agent.thoughts` | brain → gateway | Speech, chat replies, reasoning shown in the UI |

The full message contract is in [`protocol.md`](protocol.md).

## Three layers of reliability

Small models propose impossible actions no matter how plainly the rules are written. Each layer catches what the one before it lets through.

1. **The prompt states the legal move.** A `RIGHT NOW:` block is computed in Go from live state: where the cook is, what they hold, the oldest open ticket, and the next sensible step.
2. **The brain corrects before sending.** `correct()` rewrites actions that cannot work. Cooking with no prep becomes a walk to the walk-in; serving away from the pass becomes a move to the pass; a dish nobody ordered goes in the bin.
3. **The engine has the final say.** It validates every action against current state and rejects with a reason. It is the only authority, and the model never writes to the world directly.

Each action carries the tick it was based on. The engine measures `current_tick - based_on_tick` as staleness and exports it, so "the cooks feel slow" becomes a number on a dashboard.

## Design decisions

**Why is the engine authoritative?** Decisions arrive seconds late. Validating against current state, not the state the decision was made on, is how multiplayer game servers handle network lag, and the same reasoning applies to model latency.

**Why Kafka?** It decouples a 20 Hz producer from consumers that take seconds per message. Each cook has its own consumer group and commits only after it has finished thinking, which makes consumer lag a direct measure of how far behind reality that cook is.

**Why rebuild the prompt every time?** Each decision builds a bounded prompt from short-term memory plus the four nearest retrieved memories. Cost and context size stay flat however long the service runs.

**Why a separate chat lane?** Chat shared a queue with kitchen decisions at first, and a question could wait behind a slow local-model decision. Chat now runs on its own goroutine per cook, always on the fast provider chain, with no tool calls. Orders still go through the normal decision loop.

**Why tiered model routing?** Free-tier rate limits are the binding constraint. Lead cooks use the primary provider chain; supporting cooks use a cheaper background chain. Each provider sits behind its own token-bucket limiter, and the client falls through to the next provider on failure.

**Why four cooks?** The first version had seven. On the same rate limits each one thought less often, acted on older information, and collided with the others more.

## Memory

- **Short-term**, in Redis: recent observations, the chat thread with the Owner, and any standing order.
- **Long-term**, in a Redis HNSW vector index: lore documents from `brain/lore/` and each cook's own experiences, embedded locally with `nomic-embed-text` (768 dimensions). Retrieval returns memories that are either private to that cook or shared by all.

## Engine details

- Fixed 20 Hz tick; tick time p99 is about 0.48 ms of the 50 ms budget.
- A* pathfinding on the tile map in `engine/map.txt`.
- Two cooks walking head-on in a one-tile corridor swap places instead of blocking each other.
- A service clock drives ticket arrival: open at 11:00, lunch rush from 12:00, a lull, dinner rush from 17:00.

## Operating it

Run from `infra/`:

```bash
# clear one cook's standing order
docker compose exec redis redis-cli HDEL agent:tina:state order

# clear one conversation thread
docker compose exec redis redis-cli DEL agent:carmy:chat

# see a cook's working memory
docker compose exec redis redis-cli LRANGE agent:sydney:obs 0 -1

# reset everything (memories, threads, lore index; lore re-ingests on next brain start)
docker compose exec redis redis-cli FLUSHALL

# engine metrics
curl -s localhost:9100/metrics | grep engine_
```

If chat stops appearing, open the browser console. The WebSocket handler logs errors per message, so a broken handler shows up as `message handler failed` instead of silently killing the stream. The brain logs a line such as `[carmy] answered the Owner (groq, 566ms)` whenever a reply was produced, which tells you which side to look at.

## Known rough edges

- Small models still propose impossible actions. The correction layer rewrites them, so the engine sees few rejections, but the corrections are visible in the brain log.
- Decisions on a local Ollama model take 5 to 30 seconds, so cooks routed to it feel sluggish. Groq and Gemini answer in about a second.
- A standing order persists until the cook calls `complete_order`; a stale one keeps influencing them.
- The dining room on the map is decorative; customers are not drawn.
