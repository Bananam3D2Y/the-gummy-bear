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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"worldcraft/internal/cast"
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
	Cast    *cast.Cast
	LLM     *llm.Client
	Memory  *memory.Store
	RAG     *rag.Store
	World   *WorldView
	Writer  *kafka.Writer
	Brokers []string
}

type Agent struct {
	ID string
	me cast.Character
	d  *Deps

	// Unbuffered on purpose: while the agent is thinking, its Kafka reader
	// cannot hand over the next event, so its consumer group falls behind.
	// That makes Kafka consumer lag a direct measure of "how far behind
	// reality this agent's perception is".
	events chan protocol.Event
	orders chan protocol.OverseerCommand
	chats  chan protocol.OverseerCommand

	lastTick      int64
	pending       []string
	pendingStored int // how many pending lines were also written to Redis
	lastDecision  time.Time
	idleFor       time.Duration
}

func New(ch cast.Character, d *Deps) *Agent {
	return &Agent{
		ID: ch.ID, me: ch, d: d,
		events:  make(chan protocol.Event),
		orders:  make(chan protocol.OverseerCommand, 4),
		chats:   make(chan protocol.OverseerCommand, 8),
		idleFor: 5 * time.Second, // first decision shortly after startup
	}
}

// Orders is where standing instructions arrive; they go through the normal
// decision loop so they shape what the character does next.
func (a *Agent) Orders() chan<- protocol.OverseerCommand { return a.orders }

// Chats is a separate lane. Questions are answered by their own goroutine on
// the fast model chain, so you get a reply in about a second even if the
// character is in the middle of a slow decision.
func (a *Agent) Chats() chan<- protocol.OverseerCommand { return a.chats }

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

// who is what this character calls someone, e.g. "Cousin" or "Chef".
func (a *Agent) who(id string) string { return a.d.Cast.Calls(a.me, id) }

func (a *Agent) describe(ev protocol.Event) string {
	switch ev.Type {
	case "arrived":
		return fmt.Sprintf("You got to the %s.", ev.Location)
	case "action_result":
		return fmt.Sprintf("Your %s succeeded: %s.", ev.Action, ev.Detail)
	case "action_rejected":
		s := fmt.Sprintf("Your %s was REJECTED (%s).", ev.Action, ev.Reason)
		if ev.StalenessTicks > 20 {
			s += fmt.Sprintf(" You decided on information %.1f seconds old.", float64(ev.StalenessTicks)/20)
		}
		return s
	case "heard":
		to := "the kitchen"
		if ev.To != "" && ev.To != "everyone" {
			to = a.who(ev.To)
			if ev.To == a.ID {
				to = "you"
			}
		}
		return fmt.Sprintf("%s said to %s: %q", a.who(ev.From), to, ev.Text)
	case "received":
		return fmt.Sprintf("%s handed you 1 %s.", a.who(ev.From), ev.Item)
	case "menu_changed":
		return fmt.Sprintf("%s put %q on the menu. Tickets can come in for it now.", ev.By, ev.Dish)
	case "ticket_in":
		return fmt.Sprintf("New ticket: %s. %d tickets on the rail now.", ev.Dish, ev.OpenTickets)
	case "walkout":
		return fmt.Sprintf("A table walked out waiting for %s. That is %d tonight.", ev.Dish, ev.Walkouts)
	case "ticket_report":
		if len(ev.Tickets) == 0 {
			return fmt.Sprintf("The rail is clear. Walk-in has %d prep. Served %d, %d walkouts.",
				ev.Stock, ev.Served, ev.Walkouts)
		}
		lines := make([]string, 0, len(ev.Tickets))
		for _, t := range ev.Tickets {
			state := fmt.Sprintf("due in %ds", t.DueInS)
			if t.DueInS <= 0 {
				state = "LATE"
			}
			lines = append(lines, fmt.Sprintf("#%d %s (waiting %ds, %s)", t.ID, t.Dish, t.WaitingS, state))
		}
		return fmt.Sprintf("Rail: %s. Walk-in has %d prep. Served %d, %d walkouts.",
			strings.Join(lines, "; "), ev.Stock, ev.Served, ev.Walkouts)
	}
	return ""
}

func (a *Agent) isTrigger(ev protocol.Event) bool {
	if ev.Type == "heard" {
		// Only speech aimed at you wakes you up; otherwise the crew would talk forever.
		if ev.To == a.ID {
			return true
		}
		lower := strings.ToLower(ev.Text)
		for _, name := range []string{a.ID, a.me.Name, a.d.Cast.Calls(a.me, a.ID)} {
			if name != "" && strings.Contains(lower, strings.ToLower(name)) {
				return true
			}
		}
		return false
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
	text := a.describe(ev)
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
	case "heard", "received", "action_rejected", "ticket_report", "walkout":
		a.d.RAG.Remember(a.ID, ev.Tick, fmt.Sprintf("At tick %d: %s", ev.Tick, text))
	}
	return a.isTrigger(ev)
}

// ---------------------------------------------------------------- main loop

func (a *Agent) Run(ctx context.Context) {
	go a.runReader(ctx)
	go a.chatLoop(ctx)
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
			// Questions never reach this lane; they are answered by chatLoop.
			note := fmt.Sprintf("The Owner told you: %q", cmd.Text)
			_ = a.d.Memory.SetField(ctx, a.ID, orderField, cmd.Text)
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

	if clock, phase := a.d.World.Clock(); clock != "" {
		fmt.Fprintf(&b, "TIME: %s, %s.\n", clock, phase)
	}
	if tickets, stock := a.d.World.Rail(); len(tickets) > 0 || stock >= 0 {
		if len(tickets) == 0 {
			fmt.Fprintf(&b, "RAIL: clear, nothing waiting. Walk-in shelf (not in your hands): %d prep. "+
				"This is your chance to take a breather: call wait, or talk to someone.\n", stock)
		} else {
			var parts []string
			for _, t := range tickets {
				state := fmt.Sprintf("due in %ds", t.DueInS)
				if t.DueInS <= 0 {
					state = "LATE"
				}
				parts = append(parts, fmt.Sprintf("#%d %s (%ds, %s)", t.ID, t.Dish, t.WaitingS, state))
			}
			late := 0
			for _, t := range tickets {
				if t.DueInS <= 0 {
					late++
				}
			}
			mood := "You have room to breathe."
			switch {
			case late > 0 || len(tickets) >= 4:
				mood = "You are slammed. Be short with people. Still be accurate."
			case len(tickets) >= 2:
				mood = "Steady. Keep moving."
			}
			fmt.Fprintf(&b, "RAIL: %s. Walk-in shelf (not in your hands): %d prep. %s\n",
				strings.Join(parts, "; "), stock, mood)
		}
	}
	if known {
		where := state.Location
		if where == "" {
			where = "on the road"
		}
		fmt.Fprintf(&b, "YOU: at the %s (x=%d, y=%d)", where, state.X, state.Y)
		if state.Moving && state.Destination != "" {
			fmt.Fprintf(&b, ", currently walking to the %s", state.Destination)
		}
		var carrying []string
		for item, n := range state.Inventory {
			if n > 0 {
				carrying = append(carrying, fmt.Sprintf("%d %s", n, item))
			}
		}
		sort.Strings(carrying)
		if len(carrying) == 0 {
			carrying = []string{"nothing"}
		}
		fmt.Fprintf(&b, ". Hands: %s. Tips tonight: %d.\n", strings.Join(carrying, ", "), state.Coins)
	} else {
		b.WriteString("YOU: you just woke up and haven't looked around yet.\n")
	}

	// Deterministic hints. The models reliably confuse walk-in stock with what
	// they are carrying, and forget which dish the rail is actually waiting on,
	// so the brain works out the one legal move and states it outright.
	if known {
		prep := state.Inventory["prep"]
		var dish string
		for item, n := range state.Inventory {
			if item != "prep" && n > 0 {
				dish = item
				break
			}
		}
		tickets, _ := a.d.World.Rail()
		oldest := ""
		if len(tickets) > 0 {
			oldest = tickets[0].Dish
		}
		wanted := false
		for _, t := range tickets {
			if t.Dish == dish {
				wanted = true
				break
			}
		}

		b.WriteString("\nRIGHT NOW:\n")
		switch {
		case dish != "" && !wanted:
			fmt.Fprintf(&b, "- Nobody has ordered the %s you are holding. Call bin with item=%q to scrape it, "+
				"then work the rail.\n", dish, dish)
		case dish != "" && state.Location == "pass":
			fmt.Fprintf(&b, "- You are holding a %s at the pass and there is a ticket for it. Call serve with dish=%q.\n", dish, dish)
		case dish != "":
			fmt.Fprintf(&b, "- You are holding a %s and there is a ticket for it. Walk to the pass (move_to pass) and serve it.\n", dish)
		case prep >= 2 && state.Location == "line" && oldest != "":
			fmt.Fprintf(&b, "- You have enough prep and you are at the line. Call cook with dish=%q, the oldest ticket.\n", oldest)
		case prep >= 2 && state.Location == "line":
			b.WriteString("- You have enough prep and you are at the line, but nothing is on the rail. Wait for a ticket.\n")
		case prep >= 2 && oldest != "":
			fmt.Fprintf(&b, "- You have enough prep. Walk to the line (move_to line) and cook %q.\n", oldest)
		case prep >= 2:
			b.WriteString("- You have enough prep. Walk to the line (move_to line) and wait for a ticket.\n")
		case state.Location == "walkin":
			fmt.Fprintf(&b, "- You are carrying %d prep and you need 2. Call pull_stock again before you leave.\n", prep)
		default:
			fmt.Fprintf(&b, "- You are carrying %d prep, which is NOT enough to cook. The prep in the walk-in is not in your hands. "+
				"Walk to the walk-in (move_to walkin) and pull_stock twice.\n", prep)
		}
	}

	order := a.d.Memory.GetField(ctx, a.ID, orderField)
	if order != "" {
		fmt.Fprintf(&b, "\nORDER FROM THE OWNER (top priority until you call complete_order): %q\n", order)
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
	if chat := a.d.Memory.Chat(ctx, a.ID); len(chat) > 0 {
		b.WriteString("\nYOUR CONVERSATION WITH THE OWNER (oldest first, continue it):\n")
		for _, line := range chat {
			b.WriteString("- " + line + "\n")
		}
	}

	b.WriteString("\nWhat do you do next?")
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

// chatLoop answers the Owner directly. It never touches the action pipeline,
// so a question is answered while the character keeps working.
func (a *Agent) chatLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-a.chats:
			a.answer(ctx, cmd.Text)
		}
	}
}

func (a *Agent) answer(ctx context.Context, question string) {
	start := time.Now()
	_ = a.d.Memory.AddChat(ctx, a.ID, "Owner", question)

	var b strings.Builder
	state, _, known := a.d.World.State(a.ID)
	if clock, phase := a.d.World.Clock(); clock != "" {
		fmt.Fprintf(&b, "It is %s, %s.\n", clock, phase)
	}
	if known {
		where := state.Location
		if where == "" {
			where = "crossing the kitchen"
		}
		fmt.Fprintf(&b, "You are at the %s.\n", where)
	}
	if tickets, stock := a.d.World.Rail(); len(tickets) > 0 {
		late := 0
		for _, t := range tickets {
			if t.DueInS <= 0 {
				late++
			}
		}
		fmt.Fprintf(&b, "There are %d tickets on the rail (%d late) and %d prep in the walk-in.\n",
			len(tickets), late, stock)
	} else {
		b.WriteString("The rail is clear right now.\n")
	}
	if chat := a.d.Memory.Chat(ctx, a.ID); len(chat) > 1 {
		b.WriteString("\nYour conversation so far:\n")
		for _, line := range chat {
			b.WriteString("- " + line + "\n")
		}
	}
	fmt.Fprintf(&b, "\nThe Owner says: %q\n\nAnswer them now, in your own voice.", question)

	msgs := []llm.Message{
		{Role: "system", Content: SystemPrompt(a.d.Cast, a.me) + "\n\n" + chatRules},
		{Role: "user", Content: b.String()},
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	res, err := a.d.LLM.Chat(cctx, msgs, nil, "primary")
	cancel()
	if err != nil {
		log.Printf("[%s] chat failed: %v", a.ID, err)
		a.publishThought(ctx, protocol.Thought{Text: "(can't hear you over the rail right now)", Tool: "reply_to_owner"})
		return
	}
	reply := strings.TrimSpace(res.Message.Content)
	if reply == "" {
		reply = "(no answer)"
	}
	_ = a.d.Memory.AddChat(ctx, a.ID, a.me.Name, reply)
	a.d.RAG.Remember(a.ID, a.lastTick, "You told the Owner: "+reply)
	log.Printf("[%s] answered the Owner (%s, %dms)", a.ID, res.Provider, time.Since(start).Milliseconds())
	a.publishThought(ctx, protocol.Thought{Text: reply, Tool: "reply_to_owner",
		Model: res.Model, LatencyMs: res.Latency.Milliseconds()})
}

// correct rewrites an action the kitchen would reject into the step that
// actually gets the character closer to doing it. Small models keep trying to
// cook with one portion of prep or serve from the wrong station; the engine
// would reject those, so the brain fixes them before they are ever sent.
func (a *Agent) correct(act protocol.Action) (protocol.Action, string) {
	state, _, known := a.d.World.State(a.ID)
	if !known {
		return act, ""
	}
	tickets, _ := a.d.World.Rail()
	prep := state.Inventory["prep"]
	held := ""
	for item, n := range state.Inventory {
		if item != "prep" && n > 0 {
			held = item
			break
		}
	}
	hasTicket := func(dish string) bool {
		for _, t := range tickets {
			if t.Dish == dish {
				return true
			}
		}
		return false
	}
	swap := func(to protocol.Action, why string) (protocol.Action, string) {
		to.AgentID, to.BasedOnTick, to.DecisionID = act.AgentID, act.BasedOnTick, act.DecisionID
		return to, why
	}

	switch act.Type {
	case "cook":
		if prep < 2 {
			if state.Location == "walkin" {
				return swap(protocol.Action{Type: "pull_stock"}, "only had "+strconv.Itoa(prep)+" prep, pulling instead")
			}
			return swap(protocol.Action{Type: "move_to", Target: "walkin"}, "no prep in hand, heading to the walk-in")
		}
		if state.Location != "line" {
			return swap(protocol.Action{Type: "move_to", Target: "line"}, "not at the line yet")
		}
		if !hasTicket(act.Dish) && len(tickets) > 0 {
			act.Dish = tickets[0].Dish
			return act, "switched to the oldest ticket"
		}
	case "serve":
		if held == "" {
			return swap(protocol.Action{Type: "move_to", Target: "walkin"}, "nothing in hand to serve")
		}
		if !hasTicket(held) {
			return swap(protocol.Action{Type: "bin", Item: held}, "nobody ordered that, binning it")
		}
		act.Dish = held
		if state.Location != "pass" {
			return swap(protocol.Action{Type: "move_to", Target: "pass"}, "not at the pass yet")
		}
	case "pull_stock":
		if state.Location != "walkin" {
			return swap(protocol.Action{Type: "move_to", Target: "walkin"}, "not at the walk-in yet")
		}
	}
	return act, ""
}

// tier decides which model chain this character thinks on.
func (a *Agent) tier() string {
	if a.me.Tier == "" {
		return "background"
	}
	return a.me.Tier
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
		{Role: "system", Content: SystemPrompt(a.d.Cast, a.me)},
		{Role: "user", Content: prompt},
	}
	a.pending = nil
	a.pendingStored = 0

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	res, err := a.d.LLM.Chat(cctx, msgs, Tools(a.d.Cast, a.me, a.d.World.Menu()), a.tier())
	cancel()
	if err != nil {
		log.Printf("[%s] LLM failed: %v", a.ID, err)
		a.publishThought(ctx, protocol.Thought{Text: "(no answer from the kitchen brain: every model is unavailable)", Tool: "error"})
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

	case "create_dish":
		recipe := argStr(args, "recipe")
		dish := argStr(args, "dish")
		act := protocol.Action{Type: "create_dish", AgentID: a.ID, BasedOnTick: a.lastTick,
			DecisionID: fmt.Sprintf("%s-%d", a.ID, time.Now().UnixNano()), Dish: dish}
		if err := kafkax.PublishJSON(ctx, a.d.Writer, protocol.TopicActions, a.ID, act); err != nil {
			log.Printf("[%s] publish action: %v", a.ID, err)
		}
		// The recipe goes into shared memory, so any cook can retrieve it later.
		a.d.RAG.RememberShared("recipe", a.lastTick,
			fmt.Sprintf("Recipe for %q, created by %s: %s", dish, a.me.Name, recipe))
		_ = a.d.Memory.AddObservation(ctx, a.ID, a.lastTick, "You created "+dish+": "+recipe)
		a.idleFor = 3 * time.Second
		if thought == "" {
			thought = "Putting " + dish + " on the menu."
		}
		thought += " — " + recipe

	case "reply_to_owner":
		reply := argStr(args, "text")
		_ = a.d.Memory.AddChat(ctx, a.ID, a.me.Name, reply)
		note := "You told the Owner: " + reply
		_ = a.d.Memory.AddObservation(ctx, a.ID, a.lastTick, note)
		a.d.RAG.Remember(a.ID, a.lastTick, note)
		a.idleFor = 2 * time.Second // straight back to work
		thought = reply

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
			Target:     argStr(args, "target"), Item: argStr(args, "item"), Dish: argStr(args, "dish"),
			To: argStr(args, "to"), Text: argStr(args, "text"),
		}
		if act.To == "everyone" {
			act.To = ""
		}
		if fixed, why := a.correct(act); why != "" {
			log.Printf("[%s] corrected %s -> %s (%s)", a.ID, act.Type, fixed.Type, why)
			act = fixed
			name = act.Type
			thought += " (" + why + ")"
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
