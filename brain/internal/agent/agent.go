// Package agent is one villager's brain. Each agent runs in its own goroutine:
//
//	perceive  - read its own events from Kafka (its own consumer group)
//	remember  - write observations to Redis (short-term) and the RAG store (long-term)
//	retrieve  - pull relevant lore and old memories by vector search
//	decide    - one LLM call with tools
//	act       - publish the chosen action to agent.actions
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"worldcraft/internal/kafkax"
	"worldcraft/internal/llm"
	"worldcraft/internal/memory"
	"worldcraft/internal/metrics"
	"worldcraft/internal/protocol"
	"worldcraft/internal/rag"
)

const (
	defaultIdle  = 20 * time.Second       // think again if nothing happens for this long
	minThinkGap  = 3 * time.Second        // never think more often than this
	coalesceWait = 150 * time.Millisecond // gather events that arrive together into one decision
	orderField   = "order"
)

type Deps struct {
	LLM     *llm.Client
	Memory  *memory.Store
	RAG     *rag.Store
	World   *WorldView
	Writer  *kafka.Writer
	Brokers []string
}

type Agent struct {
	ID string
	d  *Deps

	// Unbuffered on purpose: while the agent is thinking, its Kafka reader
	// cannot hand over the next event, so its consumer group falls behind.
	// That makes Kafka consumer lag a direct measure of "how far behind
	// reality this agent's perception is".
	events chan protocol.Event
	orders chan protocol.OverseerCommand

	lastTick      int64
	pending       []string
	pendingStored int // how many pending lines were also written to Redis
	lastDecision  time.Time
	idleFor       time.Duration
}

func New(id string, d *Deps) *Agent {
	return &Agent{
		ID: id, d: d,
		events:  make(chan protocol.Event),
		orders:  make(chan protocol.OverseerCommand, 4),
		idleFor: 5 * time.Second, // first decision shortly after startup
	}
}

// Orders is where the overseer router delivers chat commands.
func (a *Agent) Orders() chan<- protocol.OverseerCommand { return a.orders }

// ---------------------------------------------------------------- perception

func (a *Agent) runReader(ctx context.Context) {
	r := kafkax.NewGroupReader(a.d.Brokers, protocol.TopicEvents, "brain-"+a.ID)
	defer r.Close()
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[%s] kafka fetch: %v", a.ID, err)
			time.Sleep(time.Second)
			continue
		}
		if string(m.Key) == a.ID {
			var ev protocol.Event
			if err := json.Unmarshal(m.Value, &ev); err == nil {
				select {
				case a.events <- ev: // blocks until the agent is free to listen
				case <-ctx.Done():
					return
				}
			}
		}
		// Commit only after the agent has taken the event (or it wasn't ours).
		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			log.Printf("[%s] commit: %v", a.ID, err)
		}
	}
}

func describe(ev protocol.Event) string {
	switch ev.Type {
	case "arrived":
		return fmt.Sprintf("You arrived at the %s.", ev.Location)
	case "action_result":
		return fmt.Sprintf("Your %s succeeded: %s.", ev.Action, ev.Detail)
	case "action_rejected":
		s := fmt.Sprintf("Your %s was REJECTED (%s).", ev.Action, ev.Reason)
		if ev.StalenessTicks > 20 {
			s += fmt.Sprintf(" You decided on information %.1f seconds old.", float64(ev.StalenessTicks)/20)
		}
		return s
	case "heard":
		to := ev.To
		if to == "" {
			to = "everyone"
		}
		return fmt.Sprintf("%s said to %s: %q", ev.From, to, ev.Text)
	case "received":
		return fmt.Sprintf("%s gave you 1 %s.", ev.From, ev.Item)
	case "market_report":
		var parts []string
		for _, item := range []string{"ore", "tool"} {
			h := ev.History[item]
			if len(h) > 8 {
				h = h[len(h)-8:]
			}
			trend := "flat"
			if len(h) >= 2 {
				switch diff := h[len(h)-1] - h[0]; {
				case diff > 0.05*h[0]:
					trend = "rising"
				case diff < -0.05*h[0]:
					trend = "falling"
				}
			}
			parts = append(parts, fmt.Sprintf("%s %.2f coins (%s; recent %v)", item, ev.Prices[item], trend, h))
		}
		return "Market report: " + strings.Join(parts, "; ") + "."
	}
	return ""
}

func (a *Agent) isTrigger(ev protocol.Event) bool {
	if ev.Type == "heard" {
		// Only speech aimed at you wakes you up; otherwise agents would talk forever.
		return ev.To == a.ID || strings.Contains(strings.ToLower(ev.Text), a.ID)
	}
	return true
}

// observe records an event and reports whether it should trigger a decision.
func (a *Agent) observe(ctx context.Context, ev protocol.Event) bool {
	a.lastTick = ev.Tick
	metrics.EventsProcessed.WithLabelValues(a.ID, ev.Type).Inc()
	if t := a.d.World.Tick(); t > 0 {
		metrics.PerceptionLagTicks.WithLabelValues(a.ID).Set(float64(t - ev.Tick))
	}
	text := describe(ev)
	if text == "" {
		return false
	}
	a.pending = append(a.pending, text)
	if err := a.d.Memory.AddObservation(ctx, a.ID, ev.Tick, text); err != nil {
		log.Printf("[%s] redis: %v", a.ID, err)
	} else {
		a.pendingStored++
	}
	// Worth keeping long-term: things that change what you know, not every footstep.
	switch ev.Type {
	case "heard", "received", "action_rejected", "market_report":
		a.d.RAG.Remember(a.ID, ev.Tick, fmt.Sprintf("At tick %d: %s", ev.Tick, text))
	}
	return a.isTrigger(ev)
}

// ---------------------------------------------------------------- main loop

func (a *Agent) Run(ctx context.Context) {
	go a.runReader(ctx)
	timer := time.NewTimer(a.idleFor)
	defer timer.Stop()

	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(a.idleFor)
	}

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-a.events:
			trigger := a.observe(ctx, ev)
			// Events often arrive in bursts (e.g. "received" + "heard"); decide once for the burst.
			deadline := time.After(coalesceWait)
		collect:
			for {
				select {
				case ev2 := <-a.events:
					if a.observe(ctx, ev2) {
						trigger = true
					}
				case <-deadline:
					break collect
				}
			}
			if trigger {
				a.decide(ctx)
				reset()
			}

		case cmd := <-a.orders:
			_ = a.d.Memory.SetField(ctx, a.ID, orderField, cmd.Text)
			note := fmt.Sprintf("The Overseer ordered you: %q", cmd.Text)
			a.pending = append(a.pending, note)
			if a.d.Memory.AddObservation(ctx, a.ID, a.lastTick, note) == nil {
				a.pendingStored++
			}
			a.d.RAG.Remember(a.ID, a.lastTick, note)
			a.decide(ctx)
			reset()

		case <-timer.C:
			a.pending = append(a.pending, "A while has passed and nothing new happened.")
			a.decide(ctx)
			timer.Reset(a.idleFor)
		}
	}
}

// ---------------------------------------------------------------- decision

func argStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func argInt(m map[string]any, k string, d int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return d
}

func (a *Agent) buildPrompt(ctx context.Context) (string, int) {
	var b strings.Builder
	state, tick, known := a.d.World.State(a.ID)
	if tick > a.lastTick {
		a.lastTick = tick
	}

	fmt.Fprintf(&b, "TICK %d\n", a.lastTick)
	if known {
		where := state.Location
		if where == "" {
			where = "on the road"
		}
		fmt.Fprintf(&b, "YOU: at %s (x=%d, y=%d)", where, state.X, state.Y)
		if state.Moving && state.Destination != "" {
			fmt.Fprintf(&b, ", currently walking to the %s", state.Destination)
		}
		fmt.Fprintf(&b, ". Inventory: ore=%d, tool=%d. Coins: %d.\n",
			state.Inventory["ore"], state.Inventory["tool"], state.Coins)
	} else {
		b.WriteString("YOU: you just woke up and haven't looked around yet.\n")
	}

	order := a.d.Memory.GetField(ctx, a.ID, orderField)
	if order != "" {
		fmt.Fprintf(&b, "\nOVERSEER ORDER (top priority until you call complete_order): %q\n", order)
	}

	b.WriteString("\nWHAT JUST HAPPENED:\n")
	for _, p := range a.pending {
		b.WriteString("- " + p + "\n")
	}

	// Recent history, minus the lines already listed above.
	recent, _ := a.d.Memory.Recent(ctx, a.ID, 10+a.pendingStored)
	if n := len(recent) - a.pendingStored; n > 0 {
		b.WriteString("\nRECENT MEMORY (oldest first):\n")
		for _, r := range recent[:n] {
			b.WriteString("- " + r + "\n")
		}
	}

	// Long-term retrieval: embed what's happening now, fetch the nearest lore and memories.
	query := strings.Join(a.pending, " ") + " " + order
	sctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	hits, err := a.d.RAG.Search(sctx, a.ID, query, 4)
	cancel()
	if err != nil {
		log.Printf("[%s] rag search: %v", a.ID, err)
	}
	if len(hits) > 0 {
		b.WriteString("\nRELEVANT KNOWLEDGE AND OLDER MEMORIES:\n")
		for _, h := range hits {
			fmt.Fprintf(&b, "- [%s] %s\n", h.Kind, h.Text)
		}
	}
	b.WriteString("\nChoose your next action.")
	return b.String(), len(hits)
}

func (a *Agent) publishThought(ctx context.Context, t protocol.Thought) {
	t.AgentID, t.TS = a.ID, time.Now().UnixMilli()
	if t.Tick == 0 {
		t.Tick = a.lastTick
	}
	if err := kafkax.PublishJSON(ctx, a.d.Writer, protocol.TopicThoughts, a.ID, t); err != nil {
		log.Printf("[%s] publish thought: %v", a.ID, err)
	}
}

func (a *Agent) decide(ctx context.Context) {
	if gap := time.Since(a.lastDecision); gap < minThinkGap {
		select {
		case <-time.After(minThinkGap - gap):
		case <-ctx.Done():
			return
		}
	}
	a.lastDecision = time.Now()

	prompt, retrieved := a.buildPrompt(ctx)
	msgs := []llm.Message{
		{Role: "system", Content: Personas[a.ID] + "\n\n" + worldRules},
		{Role: "user", Content: prompt},
	}
	a.pending = nil
	a.pendingStored = 0

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	res, err := a.d.LLM.Chat(cctx, msgs, Tools)
	cancel()
	if err != nil {
		log.Printf("[%s] LLM failed: %v", a.ID, err)
		a.publishThought(ctx, protocol.Thought{Text: "(my mind is foggy: every model is unavailable right now)", Tool: "error"})
		a.idleFor = 30 * time.Second
		return
	}

	if len(res.Message.ToolCalls) == 0 {
		// The model answered in prose instead of calling a tool: treat it as thinking out loud.
		a.publishThought(ctx, protocol.Thought{Text: res.Message.Content, Tool: "none",
			Model: res.Model, LatencyMs: res.Latency.Milliseconds(), Retrieved: retrieved})
		a.idleFor = 10 * time.Second
		return
	}

	call := res.Message.ToolCalls[0]
	name := call.Function.Name
	args := map[string]any{}
	_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	thought := argStr(args, "thought")
	delete(args, "thought")
	argJSON, _ := json.Marshal(args)
	metrics.Decisions.WithLabelValues(a.ID, name).Inc()
	log.Printf("[%s] %s %s | %s (%s, %dms, %d retrieved)", a.ID, name, argJSON, thought, res.Provider, res.Latency.Milliseconds(), retrieved)

	a.idleFor = defaultIdle
	switch name {
	case "wait":
		a.idleFor = time.Duration(argInt(args, "seconds", 10)) * time.Second

	case "complete_order":
		summary := argStr(args, "summary")
		_ = a.d.Memory.ClearField(ctx, a.ID, orderField)
		note := "You completed the Overseer's order: " + summary
		_ = a.d.Memory.AddObservation(ctx, a.ID, a.lastTick, note)
		a.d.RAG.Remember(a.ID, a.lastTick, note)
		a.idleFor = time.Second // get back to work right away
		if thought == "" {
			thought = summary
		} else {
			thought += " Report: " + summary
		}

	default:
		act := protocol.Action{
			Type: name, AgentID: a.ID, BasedOnTick: a.lastTick,
			DecisionID: fmt.Sprintf("%s-%d", a.ID, time.Now().UnixNano()),
			Target:     argStr(args, "target"), Item: argStr(args, "item"),
			To: argStr(args, "to"), Text: argStr(args, "text"),
		}
		if act.To == "everyone" {
			act.To = ""
		}
		if err := kafkax.PublishJSON(ctx, a.d.Writer, protocol.TopicActions, a.ID, act); err != nil {
			log.Printf("[%s] publish action: %v", a.ID, err)
		}
		if name == "say" {
			a.idleFor = 5 * time.Second // give the listener a moment to answer
		}
	}

	decision := fmt.Sprintf("You chose %s %s because: %s", name, argJSON, thought)
	_ = a.d.Memory.AddObservation(ctx, a.ID, a.lastTick, decision)
	a.publishThought(ctx, protocol.Thought{Text: thought, Tool: name, Args: string(argJSON),
		Model: res.Model, LatencyMs: res.Latency.Milliseconds(), Retrieved: retrieved})
}
