// Command gateway serves the web UI and bridges browsers to Kafka:
//
//	world.snapshots, agent.thoughts, world.events  --> WebSocket --> browser
//	browser chat box --> overseer.commands
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"worldcraft/internal/kafkax"
	"worldcraft/internal/metrics"
	"worldcraft/internal/protocol"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ---------------------------------------------------------------- hub

type client struct {
	conn *websocket.Conn
	send chan []byte
}

type hub struct {
	mu       sync.Mutex
	clients  map[*client]struct{}
	latest   []byte // newest snapshot, sent to every new browser immediately
	latestMu sync.RWMutex
}

func newHub() *hub { return &hub{clients: map[*client]struct{}{}} }

func (h *hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	metrics.WSClients.Set(float64(len(h.clients)))
	h.mu.Unlock()
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	metrics.WSClients.Set(float64(len(h.clients)))
	h.mu.Unlock()
}

// broadcast never blocks: a slow browser just misses a frame.
func (h *hub) broadcast(kind string, b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default:
		}
	}
	metrics.GatewayForwarded.WithLabelValues(kind).Inc()
}

// ---------------------------------------------------------------- kafka -> browser

func wrap(kind string, raw []byte) []byte {
	return []byte(`{"type":"` + kind + `","data":` + string(raw) + `}`)
}

func forwardGroup(ctx context.Context, brokers []string, topic, group, kind string, h *hub, filter func([]byte) bool) {
	r := kafkax.NewGroupReader(brokers, topic, group)
	defer r.Close()
	for {
		m, err := r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[gateway] %s: %v", topic, err)
			time.Sleep(time.Second)
			continue
		}
		if filter == nil || filter(m.Value) {
			h.broadcast(kind, wrap(kind, m.Value))
		}
	}
}

// ---------------------------------------------------------------- browser -> kafka

var upgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 64 * 1024,
	CheckOrigin: func(*http.Request) bool { return true }, // local dev only
}

type inbound struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id"`
	Text    string `json:"text"`
}

func serveWS(ctx context.Context, h *hub, publish func(protocol.OverseerCommand) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &client{conn: conn, send: make(chan []byte, 64)}
		h.add(c)

		h.latestMu.RLock()
		if h.latest != nil {
			c.send <- wrap("snapshot", h.latest)
		}
		h.latestMu.RUnlock()

		// Writer goroutine: the only place that writes to this connection.
		go func() {
			defer conn.Close()
			for b := range c.send {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
					return
				}
			}
		}()

		// Reader loop: chat commands from the overseer.
		defer h.remove(c)
		conn.SetReadLimit(8 * 1024)
		for {
			var msg inbound
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type != "order" || msg.Text == "" || msg.AgentID == "" {
				continue
			}
			if len(msg.Text) > 500 {
				msg.Text = msg.Text[:500]
			}
			cmd := protocol.OverseerCommand{AgentID: msg.AgentID, Text: msg.Text, TS: time.Now().UnixMilli()}
			if err := publish(cmd); err != nil {
				log.Printf("[gateway] publish order: %v", err)
				continue
			}
			b, _ := json.Marshal(cmd)
			h.broadcast("order", wrap("order", b))
		}
	}
}

// ---------------------------------------------------------------- main

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	brokers := kafkax.Brokers(envOr("KAFKA_BROKERS", "localhost:9092"))
	metrics.Serve(envOr("METRICS_ADDR", ":9102"))
	h := newHub()

	// Snapshots: tail the latest, remember it, fan out to browsers.
	go kafkax.FollowLatest(ctx, brokers, protocol.TopicSnapshots, func(b []byte) {
		cp := append([]byte(nil), b...)
		h.latestMu.Lock()
		h.latest = cp
		h.latestMu.Unlock()
		h.broadcast("snapshot", wrap("snapshot", cp))
	})

	go forwardGroup(ctx, brokers, protocol.TopicThoughts, "gateway-thoughts", "thought", h, nil)

	// Only forward events that are interesting to watch (not every arrival).
	interesting := map[string]bool{"action_rejected": true, "action_result": true, "heard": true, "received": true, "market_report": true}
	go forwardGroup(ctx, brokers, protocol.TopicEvents, "gateway-events", "event", h, func(b []byte) bool {
		var e struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(b, &e) == nil && interesting[e.Type]
	})

	writer := kafkax.NewWriter(brokers)
	defer writer.Close()
	publish := func(cmd protocol.OverseerCommand) error {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return kafkax.PublishJSON(pctx, writer, protocol.TopicOverseer, cmd.AgentID, cmd)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(envOr("WEB_DIR", "../web"))))
	mux.HandleFunc("/ws", serveWS(ctx, h, publish))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	addr := envOr("HTTP_ADDR", ":8080")
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("gateway on http://localhost%s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
