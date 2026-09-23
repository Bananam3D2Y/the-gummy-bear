// Package protocol mirrors docs/protocol.md. If you change a field here,
// change it in the C++ engine too.
package protocol

const (
	TopicSnapshots = "world.snapshots"   // engine -> gateway, full state at 10 Hz
	TopicEvents    = "world.events"      // engine -> brains, keyed by agent_id
	TopicActions   = "agent.actions"     // brains -> engine, keyed by agent_id
	TopicOverseer  = "overseer.commands" // gateway -> brains, keyed by agent_id
	TopicThoughts  = "agent.thoughts"    // brains -> gateway, keyed by agent_id
)

// AgentState is attached to every event, so an agent always knows where it is.
type AgentState struct {
	X           int            `json:"x"`
	Y           int            `json:"y"`
	Location    string         `json:"location"`
	Inventory   map[string]int `json:"inventory"`
	Coins       int            `json:"coins"`
	Moving      bool           `json:"moving"`
	Destination string         `json:"destination"`
}

// Event is one thing an agent perceived. Type is one of:
// arrived, action_result, action_rejected, heard, received, market_report.
type Event struct {
	Type           string               `json:"type"`
	AgentID        string               `json:"agent_id"`
	Tick           int64                `json:"tick"`
	State          AgentState           `json:"state"`
	Action         string               `json:"action,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	Detail         string               `json:"detail,omitempty"`
	Location       string               `json:"location,omitempty"`
	Target         string               `json:"target,omitempty"`
	From           string               `json:"from,omitempty"`
	To             string               `json:"to,omitempty"`
	Text           string               `json:"text,omitempty"`
	Item           string               `json:"item,omitempty"`
	StalenessTicks int64                `json:"staleness_ticks,omitempty"`
	Prices         map[string]float64   `json:"prices,omitempty"`
	History        map[string][]float64 `json:"history,omitempty"`
}

// Action is a request from a brain. The engine may reject it.
type Action struct {
	Type        string `json:"type"`
	AgentID     string `json:"agent_id"`
	BasedOnTick int64  `json:"based_on_tick"`
	DecisionID  string `json:"decision_id"`
	Target      string `json:"target,omitempty"`
	Item        string `json:"item,omitempty"`
	To          string `json:"to,omitempty"`
	Text        string `json:"text,omitempty"`
}

// OverseerCommand is what you type in the browser chat box.
type OverseerCommand struct {
	AgentID string `json:"agent_id"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}

// Thought is an agent's decision, published so the browser can show its reasoning.
type Thought struct {
	AgentID   string `json:"agent_id"`
	Text      string `json:"text"`
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	Tick      int64  `json:"tick"`
	Model     string `json:"model"`
	LatencyMs int64  `json:"latency_ms"`
	Retrieved int    `json:"retrieved"`
	TS        int64  `json:"ts"`
}
