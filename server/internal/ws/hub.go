package ws

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// ---------- message envelope ----------

type Msg struct {
	T string                 `json:"t"`           // type
	M map[string]interface{} `json:"m,omitempty"` // payload
}

// ---------- client / room / hub ----------

type Client struct {
	id   string
	conn *websocket.Conn
	send chan []byte
}

type Card struct{ Suit, Rank string } // Suit: A,L,H,B ; Rank: A,K,O,U,10,9,8,7 or "WELI"

type Room struct {
	ID        string
	Game      string
	Seats     int
	PlayerIDs []string       // seat -> client.id ("" if empty)
	Hands     map[int][]Card // seat -> hand
	Turn      int            // current seat to act
	Started   bool
	Trump     string // "A","L","H","B"
}

type Hub struct {
	allowOrigins map[string]bool
	clients      map[*Client]struct{}
	mu           sync.RWMutex
	broadcast    chan []byte

	roomsMu sync.RWMutex
	rooms   map[string]*Room
}

func NewHub(allow []string) *Hub {
	m := map[string]bool{}
	for _, a := range allow {
		if a != "" {
			m[a] = true
		}
	}
	return &Hub{
		allowOrigins: m,
		clients:      map[*Client]struct{}{},
		broadcast:    make(chan []byte, 256),
		rooms:        map[string]*Room{},
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

// ---------- websockets ----------

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
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

	// writer
	go func() {
		ping := time.NewTicker(15 * time.Second)
		defer func() { ping.Stop(); _ = c.Close(websocket.StatusNormalClosure, "bye") }()
		for {
			select {
			case msg, ok := <-client.send:
				if !ok {
					return
				}
				_ = c.Write(r.Context(), websocket.MessageText, msg)
			case <-ping.C:
				_ = c.Ping(r.Context())
			}
		}
	}()

	// reader
	for {
		_, data, err := c.Read(r.Context())
		if err != nil {
			break
		}
		var m Msg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}

		switch m.T {

		// ---- Lobby API ----

		case "list_rooms":
			// one-shot snapshot for this client
			h.sendRoomsSnapshotTo(client)

		case "create_table":
			// m.M: game, seats
			seats := 3
			if v, ok := m.M["seats"].(float64); ok && v >= 2 && v <= 5 {
				seats = int(v)
			}
			roomID := randID()
			room := &Room{
				ID: roomID, Game: "mulatschak",
				Seats: seats, PlayerIDs: make([]string, seats),
			}
			h.roomsMu.Lock()
			h.rooms[roomID] = room
			h.roomsMu.Unlock()
			log.Printf("room %s created seats=%d", roomID, seats)
			h.sendTo(client, Msg{T: "created", M: map[string]interface{}{"room": roomID, "seats": seats}})
			h.broadcastRooms()

		case "join_table":
			// m.M: room
			roomID, _ := m.M["room"].(string)
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room == nil {
				h.sendTo(client, Msg{T: "error", M: map[string]interface{}{"code": "NO_ROOM"}})
				break
			}
			// assign first empty seat
			seat := -1
			for i := 0; i < room.Seats; i++ {
				if room.PlayerIDs[i] == "" {
					seat = i
					break
				}
			}
			if seat == -1 {
				h.sendTo(client, Msg{T: "error", M: map[string]interface{}{"code": "ROOM_FULL"}})
				break
			}
			// occupy
			h.roomsMu.Lock()
			room = h.rooms[roomID] // re-get under write lock
			if room.PlayerIDs[seat] == "" {
				room.PlayerIDs[seat] = client.id
				// start when full
				full := true
				for i := 0; i < room.Seats; i++ {
					if room.PlayerIDs[i] == "" {
						full = false
						break
					}
				}
				if full && !room.Started {
					deal(room)
				}
			}
			h.roomsMu.Unlock()
			log.Printf("room %s join seat=%d client=%s", roomID, seat, client.id)
			// send per-seat state to the joiner
			h.sendState(client, room, seat)
			h.broadcastRooms()

		case "leave_table":
			// m.M: room
			roomID, _ := m.M["room"].(string)
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok {
				for i := 0; i < room.Seats; i++ {
					if room.PlayerIDs[i] == client.id {
						room.PlayerIDs[i] = ""
						break
					}
				}
			}
			h.roomsMu.Unlock()
			h.broadcastRooms()

		// ---- Table API ----

		case "state":
			// re-send state: m.M.room, m.M.seat
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendState(client, room, seat)
			}

		case "move":
			// m.M: room, seat, type="play_card", card:{Suit,Rank}
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			typ, _ := m.M["type"].(string)
			cardMap, _ := m.M["card"].(map[string]interface{})
			c := Card{Suit: cardMap["Suit"].(string), Rank: cardMap["Rank"].(string)}

			var room *Room
			h.roomsMu.Lock()
			room = h.rooms[roomID]
			if room != nil && room.Started && seat == room.Turn && typ == "play_card" {
				hand := room.Hands[seat]
				if nh, ok := removeCard(hand, c); ok {
					room.Hands[seat] = nh
					// TODO: trick collection & rules; advance turn for now
					room.Turn = (room.Turn + 1) % room.Seats
				}
			}
			h.roomsMu.Unlock()

			// per-seat state refresh to players in room
			if room != nil {
				h.sendStateToRoom(room)
			}

		case "chat":
			// m.M: room, text  ->  send only to that room
			roomID, _ := m.M["room"].(string)
			text, _ := m.M["text"].(string)
			if roomID == "" || text == "" {
				break
			}
			msg := Msg{T: "chat", M: map[string]interface{}{"room": roomID, "from": client.id, "text": text}}
			h.sendMsgToRoom(roomID, msg)

		case "join":
			h.sendTo(client, Msg{T: "joined", M: map[string]interface{}{"id": client.id}})

		case "pong":
			// ignore
		}
	}

	// disconnect
	h.mu.Lock()
	delete(h.clients, client)
	close(client.send)
	h.mu.Unlock()

	// free any seats the client held
	h.roomsMu.Lock()
	for _, room := range h.rooms {
		for i := 0; i < room.Seats; i++ {
			if room.PlayerIDs[i] == client.id {
				room.PlayerIDs[i] = ""
			}
		}
	}
	h.roomsMu.Unlock()
	h.broadcastRooms()

	log.Printf("client %s disconnected", client.id)
}

// ---------- helpers (send/broadcast/rooms/state) ----------

func randID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *Hub) sendTo(c *Client, msg Msg) {
	b, _ := json.Marshal(msg)
	select {
	case c.send <- b:
	default:
	}
}

func (h *Hub) roomMeta(r *Room) map[string]interface{} {
	occ := 0
	for _, id := range r.PlayerIDs {
		if id != "" {
			occ++
		}
	}
	return map[string]interface{}{
		"id": r.ID, "game": r.Game, "seats": r.Seats,
		"occupied": occ, "started": r.Started,
	}
}

func (h *Hub) roomsSnapshot() []map[string]interface{} {
	h.roomsMu.RLock()
	defer h.roomsMu.RUnlock()
	list := make([]map[string]interface{}, 0, len(h.rooms))
	for _, r := range h.rooms {
		list = append(list, h.roomMeta(r))
	}
	return list
}

func (h *Hub) sendRoomsSnapshotTo(c *Client) {
	msg := Msg{T: "rooms", M: map[string]interface{}{"list": h.roomsSnapshot()}}
	h.sendTo(c, msg)
}

func (h *Hub) broadcastRooms() {
	msg := Msg{T: "rooms", M: map[string]interface{}{"list": h.roomsSnapshot()}}
	b, _ := json.Marshal(msg)
	h.mu.RLock()
	for c := range h.clients {
		select {
		case c.send <- b:
		default:
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) sendMsgToRoom(roomID string, msg Msg) {
	h.roomsMu.RLock()
	room := h.rooms[roomID]
	h.roomsMu.RUnlock()
	if room == nil {
		return
	}
	b, _ := json.Marshal(msg)
	h.mu.RLock()
	for cli := range h.clients {
		for _, id := range room.PlayerIDs {
			if id == cli.id {
				select {
				case cli.send <- b:
				default:
				}
				break
			}
		}
	}
	h.mu.RUnlock()
}

func (h *Hub) sendStateToRoom(room *Room) {
	h.mu.RLock()
	for cli := range h.clients {
		seat := -1
		for i := 0; i < room.Seats; i++ {
			if room.PlayerIDs[i] == cli.id {
				seat = i
				break
			}
		}
		if seat >= 0 {
			h.sendState(cli, room, seat)
		}
	}
	h.mu.RUnlock()
}

func mkDeck33() []Card {
	ranks := []string{"A", "K", "O", "U", "10", "9", "8", "7"}
	suits := []string{"A", "L", "H", "B"} // Acorns, Leaves, Hearts, Bells
	var d []Card
	for _, s := range suits {
		for _, r := range ranks {
			d = append(d, Card{Suit: s, Rank: r})
		}
	}
	// Add Weli (Bells-6). Other 6s not included in ranks.
	d = append(d, Card{Suit: "B", Rank: "WELI"})
	return shuffle(d)
}

func shuffle(in []Card) []Card {
	out := append([]Card{}, in...)
	for i := len(out) - 1; i > 0; i-- {
		var b [8]byte
		_, _ = crand.Read(b[:])
		j := int(binary.BigEndian.Uint64(b[:]) % uint64(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func deal(r *Room) {
	d := mkDeck33()
	r.Hands = map[int][]Card{}
	const hands = 5
	for seat := 0; seat < r.Seats; seat++ {
		start := seat * hands
		end := start + hands
		if end > len(d) {
			end = len(d)
		}
		r.Hands[seat] = append([]Card{}, d[start:end]...)
	}
	r.Turn = 0
	r.Started = true
	r.Trump = "H" // default; real rules later
}

func removeCard(hand []Card, c Card) ([]Card, bool) {
	for i := range hand {
		if hand[i].Suit == c.Suit && hand[i].Rank == c.Rank {
			hand[i] = hand[len(hand)-1]
			return hand[:len(hand)-1], true
		}
	}
	return hand, false
}

func (h *Hub) sendState(c *Client, r *Room, seat int) {
	// your hand; others show counts only (client renders its own view)
	counts := make([]int, r.Seats)
	for s := 0; s < r.Seats; s++ {
		counts[s] = len(r.Hands[s])
	}
	msg := Msg{
		T: "state",
		M: map[string]interface{}{
			"room": r.ID, "seat": seat, "turn": r.Turn, "trump": r.Trump,
			"you": r.Hands[seat], "counts": counts, "started": r.Started,
		},
	}
	h.sendTo(c, msg)
}
