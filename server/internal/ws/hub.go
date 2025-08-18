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

type Card struct{ Suit, Rank string } // Suit: hearts/spades/clubs/diamonds ; Rank: ace..seven or "weli" (diamonds-six)

// Room holds table + round state
type Room struct {
	ID        string
	Game      string
	Seats     int
	PlayerIDs []string       // seat -> client.id ("" if empty)
	Hands     map[int][]Card // seat -> hand

	// Trick/play state
	Lead     string
	Trick    []Card
	TrickBy  []int
	Turn     int
	HandOver bool

	// Round meta
	Trump   string // "", or hearts/spades/clubs/diamonds
	Started bool   // true only during trick play (phase == "play")

	// Dealer / bidding
	Dealer      int    // rotates each hand; -1 before first deal
	FirstBidder int    // (Dealer+1)%Seats
	Phase       string // "cut" | "bidding" | "pick_trump" | "play"
	Actor       int    // whose turn to act during bidding
	BestBid     int    // highest bid so far (0 = none, 1..5)
	BestBy      int    // seat who holds BestBid (-1 if none)
	Passed      map[int]bool
	RoundDouble bool // set by "knock" (Draufklopfen) — cutter only

	// Cut preview (visible ONLY to the cutter/first bidder during CUT phase)
	CutPeek    Card
	HasCutPeek bool

	// Internal: cutter kept Weli?
	weliKeptBy int // -1 if none; else seat index

	// Pending deck (between cut and deal)
	stock []Card
}

type Hub struct {
	allowOrigins map[string]bool
	clients      map[*Client]struct{}
	mu           sync.RWMutex
	broadcast    chan []byte

	roomsMu sync.RWMutex
	rooms   map[string]*Room

	// player names: clientID -> display name
	namesMu sync.RWMutex
	names   map[string]string
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
		names:        map[string]string{},
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

		// ---- Identity ----
		case "set_name":
			name, _ := m.M["name"].(string)
			if name != "" {
				h.namesMu.Lock()
				h.names[client.id] = name
				h.namesMu.Unlock()
				// refresh tables this client sits in
				h.roomsMu.RLock()
				for _, room := range h.rooms {
					for _, id := range room.PlayerIDs {
						if id == client.id {
							h.sendStateToRoom(room)
							break
						}
					}
				}
				h.roomsMu.RUnlock()
			}

		// ---- Lobby ----
		case "list_rooms":
			h.sendRoomsSnapshotTo(client)

		case "create_table":
			seats := 3
			if v, ok := m.M["seats"].(float64); ok && v >= 2 && v <= 6 {
				seats = int(v)
			}
			roomID := randID()
			room := &Room{
				ID: roomID, Game: "mulatschak", Seats: seats,
				PlayerIDs: make([]string, seats),
				Dealer:    -1, BestBy: -1, weliKeptBy: -1,
			}
			h.roomsMu.Lock()
			h.rooms[roomID] = room
			h.roomsMu.Unlock()
			log.Printf("room %s created seats=%d", roomID, seats)
			h.sendTo(client, Msg{T: "created", M: map[string]interface{}{"room": roomID, "seats": seats}})
			h.broadcastRooms()

		case "join_table":
			roomID, _ := m.M["room"].(string)
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room == nil {
				h.sendTo(client, Msg{T: "error", M: map[string]interface{}{"code": "NO_ROOM"}})
				break
			}
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

			h.roomsMu.Lock()
			room = h.rooms[roomID]
			if room.PlayerIDs[seat] == "" {
				room.PlayerIDs[seat] = client.id
				// Auto-start a new hand when table full and idle
				full := true
				for i := 0; i < room.Seats; i++ {
					if room.PlayerIDs[i] == "" {
						full = false
						break
					}
				}
				if full && (room.Phase == "" || room.HandOver || (!room.Started && len(room.Trick) == 0)) {
					startHand(room) // rotates dealer, sets CUT phase with peek
				}
			}
			h.roomsMu.Unlock()
			log.Printf("room %s join seat=%d client=%s", roomID, seat, client.id)

			h.sendState(client, room, seat)
			h.sendStateToRoom(room)
			h.broadcastRooms()

		case "leave_table":
			roomID, _ := m.M["room"].(string)
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok {
				for i := 0; i < room.Seats; i++ {
					if room.PlayerIDs[i] == client.id {
						room.PlayerIDs[i] = ""
					}
				}
			}
			h.roomsMu.Unlock()
			// refresh remaining
			h.roomsMu.RLock()
			r := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if r != nil {
				h.sendStateToRoom(r)
			}
			h.broadcastRooms()

		// ---- Round control ----

		case "new_hand":
			roomID, _ := m.M["room"].(string)
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok {
				// only start if not mid-trick
				if !room.Started || room.HandOver || len(room.Trick) == 0 {
					startHand(room)
				}
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		// Proceed from CUT -> deal -> bidding
		case "cut_proceed":
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok && room.Phase == "cut" && seat == room.FirstBidder {
				// Deal from room.stock
				const handSize = 5
				room.Hands = map[int][]Card{}
				d := append([]Card{}, room.stock...)

				for s := 0; s < room.Seats; s++ {
					start := s * handSize
					end := start + handSize
					if end > len(d) {
						end = len(d)
					}
					room.Hands[s] = append([]Card{}, d[start:end]...)
				}
				// ensure weli goes to cutter if reserved
				if room.weliKeptBy >= 0 {
					has := false
					for _, c := range room.Hands[room.weliKeptBy] {
						if c.Rank == "weli" && c.Suit == "diamonds" {
							has = true
							break
						}
					}
					if !has {
						room.Hands[room.weliKeptBy] = append(room.Hands[room.weliKeptBy], Card{Suit: "diamonds", Rank: "weli"})
						if len(room.Hands[room.weliKeptBy]) > handSize {
							room.Hands[room.weliKeptBy] = room.Hands[room.weliKeptBy][:handSize]
						}
					}
				}

				// clear cut UI
				room.HasCutPeek = false
				room.CutPeek = Card{}
				room.stock = nil

				// move to bidding
				room.Phase = "bidding"
				room.Actor = room.FirstBidder
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		// ---- Bidding ----

		case "knock": // Draufklopfen — only cutter (first bidder), during bidding, once
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok && room.Phase == "bidding" && seat == room.FirstBidder && !room.RoundDouble {
				room.RoundDouble = true
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		case "pass":
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			advance := false
			endAuction := false
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok && room.Phase == "bidding" && seat == room.Actor {
				if room.Passed == nil {
					room.Passed = map[int]bool{}
				}
				room.Passed[seat] = true
				advance = true

				// count active (not passed)
				active := 0
				lastActive := -1
				for s := 0; s < room.Seats; s++ {
					if !room.Passed[s] && room.PlayerIDs[s] != "" {
						active++
						lastActive = s
					}
				}
				if active == 1 && room.BestBy != -1 {
					// winner known
					room.Actor = room.BestBy
					if room.BestBid == 1 {
						room.Trump = "hearts"
						room.Phase = "play"
						room.Started = true
						room.Turn = room.BestBy
					} else {
						room.Phase = "pick_trump"
					}
					endAuction = true
				}
				// if no one bid and one active remains → lead hearts
				if active == 1 && room.BestBy == -1 {
					room.Actor = lastActive
					room.Trump = "hearts"
					room.Phase = "play"
					room.Started = true
					room.Turn = lastActive
					endAuction = true
				}
			}
			if ok := h.rooms[roomID] != nil; ok && advance && !endAuction {
				// advance actor to next not-passed
				room := h.rooms[roomID]
				for i := 1; i <= room.Seats; i++ {
					n := (room.Actor + 1) % room.Seats
					room.Actor = n
					if !room.Passed[n] && room.PlayerIDs[n] != "" {
						break
					}
				}
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		case "bid":
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			bid := int(m.M["bid"].(float64)) // 1..5
			advance := false
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok && room.Phase == "bidding" && seat == room.Actor {
				if bid < 1 || bid > 5 {
					// ignore illegal
				} else if bid == 1 {
					if room.BestBid < 1 {
						room.BestBid = 1
						room.BestBy = seat
						advance = true
					}
				} else if bid > room.BestBid {
					room.BestBid = bid
					room.BestBy = seat
					advance = true
				}
			}
			if ok := h.rooms[roomID] != nil; ok && advance {
				room := h.rooms[roomID]
				// move actor to next not-passed seat
				for i := 1; i <= room.Seats; i++ {
					n := (room.Actor + 1) % room.Seats
					room.Actor = n
					if !room.Passed[n] && room.PlayerIDs[n] != "" {
						break
					}
				}
				// finish if everyone else passed
				active := 0
				for s := 0; s < room.Seats; s++ {
					if !room.Passed[s] && room.PlayerIDs[s] != "" {
						active++
					}
				}
				if active == 1 && room.BestBy != -1 {
					room.Actor = room.BestBy
					if room.BestBid == 1 {
						room.Trump = "hearts"
						room.Phase = "play"
						room.Started = true
						room.Turn = room.BestBy
					} else {
						room.Phase = "pick_trump"
					}
				}
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		case "pick_trump":
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			tr, _ := m.M["trump"].(string) // hearts/spades/clubs/diamonds
			h.roomsMu.Lock()
			if room, ok := h.rooms[roomID]; ok && room.Phase == "pick_trump" && seat == room.BestBy {
				if tr == "hearts" || tr == "spades" || tr == "clubs" || tr == "diamonds" {
					room.Trump = tr
					room.Phase = "play"
					room.Started = true
					room.Turn = seat // winner leads
				}
			}
			h.roomsMu.Unlock()
			h.roomsMu.RLock()
			room := h.rooms[roomID]
			h.roomsMu.RUnlock()
			if room != nil {
				h.sendStateToRoom(room)
			}

		// ---- Play ----

		case "move":
			// m.M: room, seat, type="play_card", card:{Suit,Rank}
			roomID, _ := m.M["room"].(string)
			seat := int(m.M["seat"].(float64))
			typ, _ := m.M["type"].(string)
			cardMap, _ := m.M["card"].(map[string]interface{})
			c := Card{Suit: cardMap["Suit"].(string), Rank: cardMap["Rank"].(string)}

			var room *Room
			var ok bool
			h.roomsMu.Lock()
			room, ok = h.rooms[roomID]
			if ok && room.Phase == "play" && room.Started && !room.HandOver && seat == room.Turn && typ == "play_card" {
				hand := room.Hands[seat]

				// must-follow enforcement
				if len(room.Trick) > 0 && hasSuit(hand, room.Lead, room.Trump) {
					if !followsSuit(c, room.Lead, room.Trump) {
						h.roomsMu.Unlock()
						break
					}
				}

				if nh, owned := removeCard(hand, c); owned {
					room.Hands[seat] = nh

					if len(room.Trick) == 0 {
						if c.Rank == "weli" {
							room.Lead = room.Trump
						} else {
							room.Lead = c.Suit
						}
					}
					room.Trick = append(room.Trick, c)
					room.TrickBy = append(room.TrickBy, seat)

					if len(room.Trick) == room.Seats {
						winner := trickWinner(room.Trick, room.TrickBy, room.Trump, room.Lead)
						room.Trick, room.TrickBy, room.Lead = nil, nil, ""
						room.Turn = winner

						// hand over if all empty
						empty := true
						for s := 0; s < room.Seats; s++ {
							if len(room.Hands[s]) > 0 {
								empty = false
								break
							}
						}
						if empty {
							room.HandOver = true
							room.Started = false
						}
					} else {
						room.Turn = (room.Turn + 1) % room.Seats
					}
				}
			}
			h.roomsMu.Unlock()

			if room != nil {
				h.sendStateToRoom(room)
			}

		case "chat":
			roomID, _ := m.M["room"].(string)
			text, _ := m.M["text"].(string)
			if roomID == "" || text == "" {
				break
			}
			h.namesMu.RLock()
			name := h.names[client.id]
			h.namesMu.RUnlock()
			msg := Msg{T: "chat", M: map[string]interface{}{"room": roomID, "from": client.id, "from_name": name, "text": text}}
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
		"occupied": occ, "started": r.Started || r.Phase != "",
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

// ---- Deck & rules (English suit/rank names) ----

func mkDeck33() []Card {
	// 8 ranks per suit (no sixes) + Weli (diamonds-six special)
	ranks := []string{"ace", "king", "queen", "jack", "ten", "nine", "eight", "seven"}
	suits := []string{"clubs", "spades", "hearts", "diamonds"}
	var d []Card
	for _, s := range suits {
		for _, r := range ranks {
			d = append(d, Card{Suit: s, Rank: r})
		}
	}
	// Weli (Schelle-6): special trump in diamonds, outranks all trumps except trump ace
	d = append(d, Card{Suit: "diamonds", Rank: "weli"})
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

func startHand(r *Room) {
	// rotate dealer
	if r.Dealer < 0 {
		r.Dealer = 0
	} else {
		r.Dealer = (r.Dealer + 1) % r.Seats
	}
	r.FirstBidder = (r.Dealer + 1) % r.Seats

	// reset round state
	r.RoundDouble = false
	r.HandOver = false
	r.Trump = ""
	r.Started = false
	r.Lead = ""
	r.Trick, r.TrickBy = nil, nil
	r.BestBid, r.BestBy = 0, -1
	r.Passed = map[int]bool{}
	r.weliKeptBy = -1
	r.HasCutPeek = false
	r.CutPeek = Card{}
	r.Hands = map[int][]Card{} // empty hands during cut
	r.stock = nil

	// fresh deck & cut
	d := mkDeck33()
	// simulate a cut: choose cut point and note the bottom card of the lifted stack
	if len(d) > 2 {
		var b [8]byte
		_, _ = crand.Read(b[:])
		cut := int(binary.BigEndian.Uint64(b[:]) % uint64(len(d)-1))
		if cut <= 0 {
			cut = 1
		}
		// bottom of lifted stack is d[cut-1]
		r.CutPeek = d[cut-1]
		r.HasCutPeek = true

		// If it's the Weli, cutter (first bidder) keeps it; remove from stock now
		if r.CutPeek.Rank == "weli" && r.CutPeek.Suit == "diamonds" {
			r.weliKeptBy = r.FirstBidder
			// remove a weli from the deck to avoid duplicates
			idx := -1
			for i := range d {
				if d[i].Rank == "weli" && d[i].Suit == "diamonds" {
					idx = i
					break
				}
			}
			if idx >= 0 {
				d[idx] = d[len(d)-1]
				d = d[:len(d)-1]
			}
		}
	}

	// keep deck for later dealing
	r.stock = d

	// phase: CUT (only cutter sees CutPeek)
	r.Phase = "cut"
	r.Actor = r.FirstBidder
	r.Turn = r.FirstBidder // UI arrow can point to cutter until play starts
}

func deal(r *Room) { // kept for compatibility; use startHand()
	startHand(r)
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

// must the player follow suit?
func hasSuit(hand []Card, suit, trump string) bool {
	for _, x := range hand {
		if suit == trump {
			if x.Rank == "weli" || x.Suit == trump {
				return true
			}
		} else {
			if x.Suit == suit && x.Rank != "weli" {
				return true
			}
		}
	}
	return false
}

func followsSuit(c Card, lead, trump string) bool {
	if lead == trump {
		return c.Rank == "weli" || c.Suit == trump
	}
	return c.Suit == lead && c.Rank != "weli"
}

// trump: ace > weli > king > queen > jack > ten > nine > eight > seven
// non-trump lead: ace > king > queen > jack > ten > nine > eight > seven
var rankVal = map[string]int{"ace": 7, "king": 6, "queen": 5, "jack": 4, "ten": 3, "nine": 2, "eight": 1, "seven": 0}

func trickWinner(trick []Card, by []int, trump, lead string) int {
	bestIdx := -1
	bestScore := -1
	for i, c := range trick {
		if c.Rank == "weli" || c.Suit == trump {
			score := 0
			if c.Rank == "weli" {
				score = 99
			} else if c.Rank == "ace" && c.Suit == trump {
				score = 100
			} else {
				score = 50 + rankVal[c.Rank]
			}
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
	}
	if bestIdx >= 0 {
		return by[bestIdx]
	}
	for i, c := range trick {
		if lead != "" && c.Suit == lead && c.Rank != "weli" {
			score := rankVal[c.Rank]
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
	}
	if bestIdx < 0 {
		bestIdx = 0
	}
	return by[bestIdx]
}

func (h *Hub) sendState(c *Client, r *Room, seat int) {
	// names
	names := make([]string, r.Seats)
	h.namesMu.RLock()
	for i := 0; i < r.Seats; i++ {
		id := r.PlayerIDs[i]
		if id != "" {
			names[i] = h.names[id]
		}
	}
	h.namesMu.RUnlock()

	// counts
	counts := make([]int, r.Seats)
	for s := 0; s < r.Seats; s++ {
		counts[s] = len(r.Hands[s])
	}

	// trick public
	trick := make([]map[string]interface{}, len(r.Trick))
	for i := range r.Trick {
		trick[i] = map[string]interface{}{"suit": r.Trick[i].Suit, "rank": r.Trick[i].Rank, "by": r.TrickBy[i]}
	}

	// passed list (for UI)
	var passed []int
	if r.Passed != nil {
		for s := 0; s < r.Seats; s++ {
			if r.Passed[s] {
				passed = append(passed, s)
			}
		}
	}

	// cut peek (ONLY for the cutter during CUT phase)
	var cutPeek map[string]interface{}
	if r.Phase == "cut" && r.HasCutPeek && seat == r.FirstBidder {
		cutPeek = map[string]interface{}{"suit": r.CutPeek.Suit, "rank": r.CutPeek.Rank}
	}

	msg := Msg{
		T: "state",
		M: map[string]interface{}{
			"room": r.ID, "seat": seat,
			"phase": r.Phase, "actor": r.Actor,
			"dealer": r.Dealer, "firstBidder": r.FirstBidder,
			"bestBid": r.BestBid, "bestBy": r.BestBy, "passed": passed,
			"roundDouble": r.RoundDouble,
			"cutPeek":     cutPeek, // null for everyone except cutter during CUT

			"turn": r.Turn, "trump": r.Trump,
			"you": r.Hands[seat], "counts": counts,
			"started": r.Started, "handOver": r.HandOver,
			"lead": r.Lead, "trick": trick,
			"names": names,
		},
	}
	h.sendTo(c, msg)
}
