package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// Minimal message envelope
// t = type ("join","move","chat","pong"), m = payload

type Msg struct {
	T string                 `json:"t"`
	M map[string]interface{} `json:"m,omitempty"`
}

type Client struct {
	id   string
	conn *websocket.Conn
	send chan []byte
}

type Hub struct {
	allowOrigins map[string]bool
	clients      map[*Client]struct{}
	mu           sync.RWMutex
	broadcast    chan []byte
}

func NewHub(allow []string) *Hub {
	m := map[string]bool{}
	for _, a := range allow {
		m[a] = true
	}
	return &Hub{
		allowOrigins: m,
		clients:      map[*Client]struct{}{},
		broadcast:    make(chan []byte, 256),
	}
}

func (h *Hub) Run() {
	for msg := range h.broadcast {
		h.mu.RLock()
		for c := range h.clients {
			select {
			case c.send <- msg:
			default:
			}
		}
		h.mu.RUnlock()
	}
}

func (h *Hub) ServeWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && !h.allowOrigins[origin] {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}

	client := &Client{id: randID(), conn: c, send: make(chan []byte, 64)}

	h.mu.Lock()
	h.clients[client] = struct{}{}
	h.mu.Unlock()
	log.Printf("client %s connected", client.id)

	// Writer
	go func() {
		ping := time.NewTicker(15 * time.Second)
		defer func() { ping.Stop(); c.Close(websocket.StatusNormalClosure, "bye") }()
		for {
			select {
			case msg, ok := <-client.send:
				if !ok {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = c.Write(ctx, websocket.MessageText, msg)
				cancel()
			case <-ping.C:
				_ = c.Ping(context.Background())
			}
		}
	}()

	// Reader
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		// Minimal echo/handshake until game rooms are wired
		var m Msg
		if err := json.Unmarshal(data, &m); err == nil {
			switch m.T {
			case "join":
				// TODO: room assignment + engine init
				out, _ := json.Marshal(Msg{T: "joined", M: map[string]interface{}{"id": client.id}})
				client.send <- out
			case "chat":
				h.broadcast <- data // broadcast chat to all for now
			case "pong":
				// ignore
			}
		}
	}

	h.mu.Lock()
	delete(h.clients, client)
	close(client.send)
	h.mu.Unlock()
	log.Printf("client %s disconnected", client.id)
}
