# WorldCraft Kitchen — handoff

**What it is:** a multi-agent simulation. A C++ engine runs a restaurant kitchen at 20 ticks
per second; four LLM-driven characters work in it, each as a Go goroutine; you talk to them
and give them orders from a browser. Kafka carries every event, Redis holds memory, Grafana
shows the metrics.

**The one-line framing:** *a fast world and slow minds.* The world updates every 50 ms; a mind
takes 1 to 20 seconds to decide. Everything in the design exists to make those two speeds work
together.

---

## Current state (as of this handoff)

Working:

- Four characters (Carmy, Sydney, Richie, Tina) defined in `cast.json`, spawned by both services
- Ticket rail with a service clock: 11:00 open, lunch rush from 12:00, lull, dinner rush from 17:00
- Full loop running: pull prep → cook the oldest ticket → carry to the pass → serve → tip
- Chat with any character (Ask), standing instructions (Order), per-character conversation threads
- Recipes on request, grounded in their own specialty
- `create_dish`: a chef invents a dish, it joins the menu, tickets start coming in for it
- `bin`: scrape a dish nobody ordered so hands are free
- RAG over kitchen lore plus each character's own memories (Ollama embeddings, Redis vector search)
- Tiered routing: Carmy and Sydney on the cloud chain, Richie and Tina on the cheap/local chain
- Action-correction guard in the brain (see "Three layers" below)
- Grafana dashboard: tick rate, tick duration, consumer lag, LLM latency, rejections, service stats

Recent fixes worth knowing:

- Head-on deadlock in one-tile gangways is solved in the engine: two cooks walking into each
  other now swap positions instead of both waiting
- Chronicle and Chat are tabs; each character has their own conversation thread
- Chat runs on its own goroutine per character and always uses the fast (primary) model chain,
  so a question is answered in about a second even while that character is mid-decision on a
  slow local model. Orders still go through the normal decision loop
- `cast.json` is the only place the crew is defined. If the cast or the restaurant name looks
  wrong, that file was overwritten; it should list carmy, sydney, richie, tina and
  "The Gummy Bear"

If chat ever stops appearing again: open the browser console (F12). The websocket handler now
catches and logs errors per message, so a broken handler shows up as `message handler failed
thought ...` instead of silently killing the stream. The backend logs `[carmy] answered the
Owner (groq, 566ms)` whenever a reply was actually produced, so that line tells you which side
to look at.

Known rough edges:

- Small models still propose impossible actions; the guard rewrites them, so the engine sees
  far fewer rejections than it used to, but the corrections are visible in the log
- Ollama decisions take 5 to 30 seconds, so characters on it feel sluggish; Groq and Gemini are ~1 s
- Standing orders persist until the character calls `complete_order`; a stale order keeps
  influencing them. Clear one with `redis-cli HDEL agent:<id>:state order`
- The dining room on the map is decorative; customers are not drawn

---

## Architecture

```
Browser ⇄ Go gateway (:8080) ⇄ Kafka ⇄ C++ engine (:9100)
                                  ⇅
                          Go brain (:9101) ⇄ Redis (memory + vectors)
                                            ⇄ Gemini / Groq / Ollama
Prometheus scrapes all three + kafka-exporter; Grafana reads Prometheus.
```

Kafka topics: `world.snapshots` (engine→gateway, 10 Hz), `world.events` (engine→brain, keyed by
character), `agent.actions` (brain→engine), `overseer.commands` (gateway→brain),
`agent.thoughts` (brain→gateway).

**Three layers of reliability**, cheapest last:

1. The prompt states the one legal move (`RIGHT NOW:` block, computed in Go from live state).
2. The brain's `correct()` rewrites impossible actions before they are sent (cook with no prep
   becomes a walk to the walk-in, serving an unordered dish becomes `bin`, and so on).
3. The engine validates everything anyway and rejects with a reason. It is the only authority.

Each action carries `based_on_tick`; the engine measures staleness, which is graphed.

---

## Layout

```
cast.json              the crew: personas, specialties, address book, model tier
engine/                C++: map.txt, src/{main,world,pathfinding,kafka_io,metrics}.cpp
brain/                 Go: cmd/{brain,gateway}, internal/{agent,cast,llm,memory,rag,kafkax,metrics,protocol}
brain/lore/*.md        RAG knowledge: service rules, the crew, the house
web/                   browser UI (plain HTML/CSS/JS)
infra/                 docker-compose, prometheus.yml, grafana dashboards
docs/protocol.md       the message contract
.env                   API keys and settings (git-ignored)
```

---

## Running it

```bash
open -a Docker
cd ~/worldcraft/infra && docker compose up -d      # Kafka, Redis, Prometheus, Grafana
```

Three terminals:

```bash
cd ~/worldcraft/engine && ./build/engine
cd ~/worldcraft/brain && set -a && source ../.env && set +a && go run ./cmd/brain
cd ~/worldcraft/brain && go run ./cmd/gateway
```

Browser: http://localhost:8080 and http://localhost:3000

After changing C++: `cd ~/worldcraft/engine && cmake --build build -j`.
After changing `.env`: re-run the `set -a && source` line before starting the brain.

### Model modes (`.env`)

```
# testing, local only, unlimited
LLM_PROVIDERS=ollama,groq,gemini
LLM_BACKGROUND_PROVIDERS=ollama,groq,gemini

# demo, fast
LLM_PROVIDERS=gemini,groq,openrouter,ollama
LLM_BACKGROUND_PROVIDERS=groq,ollama,gemini
```

Gemini free tier is roughly 500 requests/day; a 15-minute demo uses ~180. Embeddings run on
Ollama (`EMBED_PROVIDER=ollama`) so RAG costs nothing.

---

## Useful commands

```bash
# clear one character's standing order
docker compose exec redis redis-cli HDEL agent:tina:state order

# clear one conversation thread
docker compose exec redis redis-cli DEL agent:carmy:chat

# see an agent's working memory
docker compose exec redis redis-cli LRANGE agent:sydney:obs 0 -1

# reset everything (memories, threads, lore index; lore re-ingests on next brain start)
docker compose exec redis redis-cli FLUSHALL

# engine metrics
curl -s localhost:9100/metrics | grep engine_
```

---

## Demo sequence (about 3 minutes)

1. Show the kitchen running. "Four LLM agents. Nobody scripted the routine."
2. Ask Carmy what he's working on — the answer comes from the live rail.
3. Ask Tina how she makes the beef sandwich — real technique, in her voice.
4. Order Carmy: "clear the two oldest tickets before anything else."
5. Ask Sydney to invent a dessert and put it on the menu — new chip appears, tickets follow.
6. Grafana: tick duration ~480 µs against a 50 ms budget, next to LLM latency of 1 to 20 s,
   plus consumer lag spiking while agents think.

## Interview talking points

- **Why is the engine authoritative?** LLM decisions arrive seconds late; validating against
  current state is how multiplayer game servers handle network lag.
- **Why Kafka?** Decouples a 20 Hz producer from consumers that take seconds per message.
  Per-agent consumer groups, committing only after the agent takes the event, make consumer lag
  a direct measure of perception delay.
- **Why stateless prompts?** Each decision rebuilds a bounded prompt from working memory plus
  RAG, so cost and context stay flat for hours.
- **What did you learn?** For small models, deterministic state hints and a correction layer beat
  prompt instructions. The rules said "you need 2 prep" plainly and they still failed; computing
  the legal move and rewriting invalid actions fixed it.
- **What next?** Reflection (agents summarizing memories into insights), Kubernetes manifests,
  and an evaluation harness scoring tips earned per LLM call across models.
