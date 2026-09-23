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
	Tick   int64           `json:"tick"`
	Agents []snapshotAgent `json:"agents"`
}

// WorldView holds the newest engine snapshot. Agents read their own state from
// it right before deciding, so the tick they report as based_on_tick is the
// freshest one the brain knows about.
type WorldView struct {
	mu     sync.RWMutex
	tick   int64
	agents map[string]protocol.AgentState
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
