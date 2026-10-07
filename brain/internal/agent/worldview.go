package agent

import (
	"context"
	"encoding/json"
	"sync"

	"worldcraft/internal/kafkax"
	"worldcraft/internal/protocol"
)

type snapshotAgent struct {
	ID          string         `json:"id"`
	X           int            `json:"x"`
	Y           int            `json:"y"`
	Location    string         `json:"location"`
	Destination string         `json:"destination"`
	Inventory   map[string]int `json:"inventory"`
	Coins       int            `json:"coins"`
	Moving      bool           `json:"moving"`
}

type snapshot struct {
	Tick    int64             `json:"tick"`
	Agents  []snapshotAgent   `json:"agents"`
	Menu    []string          `json:"menu"`
	Tickets []protocol.Ticket `json:"tickets"`
	Stock   int               `json:"stock"`
	Clock   string            `json:"clock"`
	Phase   string            `json:"phase"`
}

// WorldView holds the newest engine snapshot. Agents read their own state from
// it right before deciding, so the tick they report as based_on_tick is the
// freshest one the brain knows about.
type WorldView struct {
	mu      sync.RWMutex
	tick    int64
	agents  map[string]protocol.AgentState
	menu    []string
	tickets []protocol.Ticket
	stock   int
	clock   string
	phase   string
}

func NewWorldView() *WorldView { return &WorldView{agents: map[string]protocol.AgentState{}} }

func (w *WorldView) update(raw []byte) {
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tick = s.Tick
	if len(s.Menu) > 0 {
		w.menu = s.Menu
	}
	w.tickets = s.Tickets
	w.stock = s.Stock
	w.clock = s.Clock
	w.phase = s.Phase
	for _, a := range s.Agents {
		w.agents[a.ID] = protocol.AgentState{
			X: a.X, Y: a.Y, Location: a.Location, Inventory: a.Inventory,
			Coins: a.Coins, Moving: a.Moving, Destination: a.Destination,
		}
	}
}

func (w *WorldView) Tick() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.tick
}

// Menu is the current menu; cooking tools are built from it.
func (w *WorldView) Menu() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return append([]string(nil), w.menu...)
}

// Rail is the open tickets and the walk-in stock, as last seen.
func (w *WorldView) Rail() ([]protocol.Ticket, int) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return append([]protocol.Ticket(nil), w.tickets...), w.stock
}

// Clock returns the service time and which part of the day it is.
func (w *WorldView) Clock() (string, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.clock, w.phase
}

func (w *WorldView) State(id string) (protocol.AgentState, int64, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	s, ok := w.agents[id]
	return s, w.tick, ok
}

// Follow tails world.snapshots until ctx is cancelled.
func (w *WorldView) Follow(ctx context.Context, brokers []string) {
	kafkax.FollowLatest(ctx, brokers, protocol.TopicSnapshots, w.update)
}
