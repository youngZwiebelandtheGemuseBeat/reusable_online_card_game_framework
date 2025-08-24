package ws

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// ----------------------------- Types & Models -----------------------------

type Card struct {
	Suit string `json:"Suit"` // keep PascalCase to match client-side "you"
	Rank string `json:"Rank"`
}

type Room struct {
	ID    string
	Game  string
	Seats int

	// Connections
	Conns     map[int]*Client // seat -> client
	PlayerIDs []string        // seat -> client.id ("" if empty)

	// Hands are private: seat -> cards
	Hands map[int][]Card

	// Trick/play state
	Lead     string
	Trick    []Card
	TrickBy  []int
	Turn     int
	HandOver bool

	// Round meta
	Trump   string // "", hearts/spades/clubs/diamonds
	Started bool

	// Dealer / bidding
	Dealer      int    // rotates each hand; -1 before first deal
	FirstBidder int    // (Dealer+1)%Seats
	Phase       string // "start" | "cut" | "bidding" | "pick_trump" | "exchange" | "play"
	Actor       int    // whose turn to act (start/bidding/exchange)
	BestBid     int    // 0 = none; 1..5
	BestBy      int    // -1 = none
	Passed      map[int]bool
	RoundDouble bool // set by start_choice: knock

	// Exchange phase
	Stayed map[int]bool // seat -> chose to stay home
	Acted  map[int]bool // seat -> already acted in exchange

	// Cut preview
	CutPeek    Card
	HasCutPeek bool

	// Weli holder after cut (if bottom card was weli)
	WeliKeptBy int // -1 if none

	// Piles
	stock          []Card // talon (remaining deck after deal)
	swamp          []Card // face-down discards; shuffled when first used
	swampShuffled  bool
	exchangeMax    int // per current seats; 3 for 3p
	exchangeClosed bool
}

type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	id     string
	name   string
	roomID string
	seat   int
}

type Hub struct {
	allowOrigins map[string]bool

	clientsMu sync.RWMutex
	clients   map[*Client]struct{}

	roomsMu sync.RWMutex
	rooms   map[string]*Room

	namesMu sync.RWMutex
	names   map[string]string // clientID -> display name
}

// ----------------------------- Hub lifecycle -----------------------------

func NewHub(allowList []string) *Hub {
	allow := make(map[string]bool, len(allowList))
	for _, o := range allowList {
		o = strings.TrimSpace(o)
		if o != "" {
			allow[o] = true
		}
	}
	return &Hub{
		allowOrigins: allow,
		clients:      make(map[*Client]struct{}),
		rooms:        make(map[string]*Room),
		names:        make(map[string]string),
	}
}

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && !h.allowOrigins[origin] {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// dev-friendly; tighten via WS_ALLOW_ORIGINS for prod
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}

	client := &Client{
		hub:  h,
		conn: c,
		send: make(chan []byte, 32),
		id:   randID(),
		seat: -1,
	}
	h.addClient(client)
	h.sendRoomsList(client) // greet

	go client.writePump()
	client.readPump()
}

// ----------------------------- Client pumps -----------------------------

func (c *Client) writePump() {
	defer c.conn.Close(websocket.StatusNormalClosure, "bye")
	for msg := range c.send {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.conn.Write(ctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			return
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.removeClient(c)
		c.conn.Close(websocket.StatusNormalClosure, "bye")
	}()
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		var env struct {
			T string                 `json:"t"`
			M map[string]interface{} `json:"m"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.T == "ping" {
			continue
		}
		c.hub.handleMessage(c, env.T, env.M)
	}
}

// ----------------------------- Helpers -----------------------------

func randID() string {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *Hub) addClient(c *Client) {
	h.clientsMu.Lock()
	h.clients[c] = struct{}{}
	h.clientsMu.Unlock()
}

func (h *Hub) removeClient(c *Client) {
	if c.roomID != "" && c.seat >= 0 {
		h.roomsMu.Lock()
		if room, ok := h.rooms[c.roomID]; ok {
			if room.Conns[c.seat] == c {
				room.Conns[c.seat] = nil
				room.PlayerIDs[c.seat] = ""
				delete(room.Hands, c.seat)
				allEmpty := true
				for _, pid := range room.PlayerIDs {
					if pid != "" {
						allEmpty = false
						break
					}
				}
				if allEmpty {
					delete(h.rooms, room.ID)
				}
			}
		}
		h.roomsMu.Unlock()
	}
	h.clientsMu.Lock()
	delete(h.clients, c)
	h.clientsMu.Unlock()
}

func (h *Hub) send(c *Client, t string, m any) {
	env := map[string]any{"t": t, "m": m}
	b, _ := json.Marshal(env)
	select {
	case c.send <- b:
	default:
	}
}

func (h *Hub) broadcastRoom(room *Room, t string, m any) {
	env := map[string]any{"t": t, "m": m}
	b, _ := json.Marshal(env)
	for _, cl := range room.Conns {
		if cl == nil {
			continue
		}
		select {
		case cl.send <- b:
		default:
		}
	}
}

func (h *Hub) sendRoomsList(to *Client) {
	type roomInfo struct {
		ID       string `json:"id"`
		Seats    int    `json:"seats"`
		Occupied int    `json:"occupied"`
		Started  bool   `json:"started"`
	}
	h.roomsMu.RLock()
	list := make([]roomInfo, 0, len(h.rooms))
	for _, r := range h.rooms {
		occ := 0
		for _, pid := range r.PlayerIDs {
			if pid != "" {
				occ++
			}
		}
		list = append(list, roomInfo{ID: r.ID, Seats: r.Seats, Occupied: occ, Started: r.Started})
	}
	h.roomsMu.RUnlock()
	h.send(to, "rooms", map[string]any{"list": list})
}

// ----------------------------- Message handling -----------------------------

func (h *Hub) handleMessage(c *Client, typ string, m map[string]interface{}) {
	switch typ {

	case "set_name":
		name := strings.TrimSpace(fmt.Sprint(m["name"]))
		if name != "" {
			h.namesMu.Lock()
			h.names[c.id] = name
			h.namesMu.Unlock()
		}

	case "create_table":
		seats := 3
		if v, ok := m["seats"].(float64); ok {
			seats = int(v)
		}
		if seats < 2 {
			seats = 2
		}
		id := randID()
		room := &Room{
			ID:          id,
			Game:        "mulatschak",
			Seats:       seats,
			Conns:       make(map[int]*Client, seats),
			PlayerIDs:   make([]string, seats),
			Hands:       make(map[int][]Card, seats),
			Dealer:      -1,
			FirstBidder: 0,
			Phase:       "",
			Actor:       -1,
			BestBy:      -1,
			Passed:      make(map[int]bool),
			WeliKeptBy:  -1,
			Stayed:      make(map[int]bool),
			Acted:       make(map[int]bool),
		}
		h.roomsMu.Lock()
		h.rooms[id] = room
		h.roomsMu.Unlock()
		h.send(c, "created", map[string]any{"room": id})
		h.sendRoomsList(c)

	case "join_table":
		roomID := fmt.Sprint(m["room"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room == nil {
			h.roomsMu.Unlock()
			h.send(c, "error", map[string]any{"msg": "room not found"})
			return
		}
		seat := -1
		for i := 0; i < room.Seats; i++ {
			if room.PlayerIDs[i] == "" && room.Conns[i] == nil {
				seat = i
				break
			}
		}
		if seat == -1 {
			h.roomsMu.Unlock()
			h.send(c, "error", map[string]any{"msg": "room full"})
			return
		}
		room.PlayerIDs[seat] = c.id
		room.Conns[seat] = c
		c.roomID = roomID
		c.seat = seat
		h.roomsMu.Unlock()

		h.sendRoomsList(c)
		h.sendStateTo(c, room)

		h.roomsMu.RLock()
		full := true
		for _, pid := range room.PlayerIDs {
			if pid == "" {
				full = false
				break
			}
		}
		h.roomsMu.RUnlock()
		if full && !room.Started && room.Dealer == -1 {
			h.startHand(room)
		} else {
			h.broadcastState(room)
		}

	case "leave_table":
		if c.roomID == "" {
			return
		}
		h.removeClient(c)

	case "chat":
		roomID := fmt.Sprint(m["room"])
		text := strings.TrimSpace(fmt.Sprint(m["text"]))
		if text == "" {
			return
		}
		h.roomsMu.RLock()
		room := h.rooms[roomID]
		h.roomsMu.RUnlock()
		if room == nil {
			return
		}
		h.namesMu.RLock()
		name := h.names[c.id]
		h.namesMu.RUnlock()
		h.broadcastRoom(room, "chat", map[string]any{
			"room":      roomID,
			"from":      c.id,
			"from_name": name,
			"text":      text,
		})

	case "new_hand":
		roomID := fmt.Sprint(m["room"])
		h.roomsMu.RLock()
		room := h.rooms[roomID]
		h.roomsMu.RUnlock()
		if room == nil {
			return
		}
		h.startHand(room)

	// ----- start / cut / bidding -----

	case "start_choice":
		roomID := fmt.Sprint(m["room"])
		choice := strings.TrimSpace(fmt.Sprint(m["choice"])) // "cut" or "knock"
		seat := toInt(m["seat"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "start" && seat == room.FirstBidder {
			if choice == "knock" {
				room.RoundDouble = true
			}
			performCut(room)
			room.Phase = "cut"
			room.Actor = room.FirstBidder
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "cut_proceed":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "cut" && seat == room.FirstBidder {
			deal(room)
			room.Phase = "bidding"
			room.Actor = room.FirstBidder
			room.BestBid = 0
			room.BestBy = -1
			room.Passed = make(map[int]bool)
			room.HasCutPeek = false
			room.CutPeek = Card{}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "pass":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "bidding" && !room.Passed[seat] {
			room.Passed[seat] = true
			// advance actor
			adv := nextSeat(room, room.Actor)
			for i := 0; i < room.Seats; i++ {
				if !room.Passed[adv] && room.PlayerIDs[adv] != "" {
					break
				}
				adv = nextSeat(room, adv)
			}
			room.Actor = adv

			// end condition: only one active & we have a bid
			active := 0
			for s := 0; s < room.Seats; s++ {
				if room.PlayerIDs[s] != "" && !room.Passed[s] {
					active++
				}
			}
			if active == 1 && room.BestBy != -1 {
				room.Actor = room.BestBy
				if room.BestBid == 1 {
					room.Trump = "hearts" // auto-hearts
					startExchange(room)   // >>> go to EXCHANGE <<<
				} else {
					room.Phase = "pick_trump"
				}
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "bid":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		bid := toInt(m["bid"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "bidding" && seat == room.Actor {
			if bid >= 1 && bid <= 5 && bid > room.BestBid {
				room.BestBid = bid
				room.BestBy = seat
				room.Actor = nextActiveBidder(room, seat)
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "pick_trump":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		tr := strings.TrimSpace(fmt.Sprint(m["trump"]))
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "pick_trump" && seat == room.BestBy {
			if tr == "hearts" || tr == "spades" || tr == "clubs" || tr == "diamonds" {
				room.Trump = tr
				startExchange(room) // >>> go to EXCHANGE after trump <<<
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	// ----- Exchange phase -----

	case "stay_home":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "exchange" && seat == room.Actor && !room.Acted[seat] {
			// declarer cannot stay home, clubs forbids stay home
			if seat != room.BestBy && room.Trump != "clubs" {
				room.Stayed[seat] = true
				room.Acted[seat] = true
				advanceExchangeOrStartPlay(room)
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "exchange":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		cardsAny, _ := m["cards"].([]interface{})
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "exchange" && seat == room.Actor && !room.Acted[seat] && !room.Stayed[seat] {
			maxN := room.exchangeMax
			n := len(cardsAny)
			if n >= 1 && n <= maxN {
				// discards -> swamp
				for _, v := range cardsAny {
					cardM, _ := v.(map[string]interface{})
					suit := strings.ToLower(fmt.Sprint(cardM["Suit"]))
					rank := strings.ToLower(fmt.Sprint(cardM["Rank"]))
					idx := -1
					for i, c := range room.Hands[seat] {
						if strings.ToLower(c.Suit) == suit && strings.ToLower(c.Rank) == rank {
							idx = i
							break
						}
					}
					if idx >= 0 {
						room.swamp = append(room.swamp, room.Hands[seat][idx])
						room.Hands[seat] = append(room.Hands[seat][:idx], room.Hands[seat][idx+1:]...)
					}
				}
				// replacements: talon first, then shuffled swamp
				need := n
				for need > 0 && len(room.stock) > 0 {
					room.Hands[seat] = append(room.Hands[seat], room.stock[0])
					room.stock = room.stock[1:]
					need--
				}
				if need > 0 && len(room.swamp) > 0 {
					if !room.swampShuffled {
						shuffle(room.swamp)
						room.swampShuffled = true
					}
					for need > 0 && len(room.swamp) > 0 {
						room.Hands[seat] = append(room.Hands[seat], room.swamp[0])
						room.swamp = room.swamp[1:]
						need--
					}
				}
				room.Acted[seat] = true
				advanceExchangeOrStartPlay(room)
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "exchange_done": // NEW: explicitly "no exchange"
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "exchange" && seat == room.Actor && !room.Acted[seat] {
			// neither stayed nor swapped -> just mark acted
			room.Acted[seat] = true
			advanceExchangeOrStartPlay(room)
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	// ----- Play -----

	case "move":
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		mv, _ := m["type"].(string)
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "play" && mv == "play_card" && seat == room.Turn && !room.HandOver && !room.Stayed[seat] {
			cardM, _ := m["card"].(map[string]interface{})
			card := Card{Suit: strings.ToLower(fmt.Sprint(cardM["Suit"])), Rank: strings.ToLower(fmt.Sprint(cardM["Rank"]))}
			hi := -1
			for i, c := range room.Hands[seat] {
				if strings.ToLower(c.Suit) == card.Suit && strings.ToLower(c.Rank) == card.Rank {
					hi = i
					break
				}
			}
			if hi >= 0 {
				room.Trick = append(room.Trick, room.Hands[seat][hi])
				room.TrickBy = append(room.TrickBy, seat)
				room.Hands[seat] = append(room.Hands[seat][:hi], room.Hands[seat][hi+1:]...)
				if len(room.Trick) == 1 {
					room.Lead = room.Trick[0].Suit
				}
				room.Turn = nextInSeat(room, seat)

				if len(room.TrickBy) == countInPlayers(room) {
					// TODO: real trick winner; placeholder = first
					winner := room.TrickBy[0]
					room.Turn = winner
					room.Trick = nil
					room.TrickBy = nil
					room.Lead = ""

					empty := true
					for s := 0; s < room.Seats; s++ {
						if room.PlayerIDs[s] != "" && !room.Stayed[s] && len(room.Hands[s]) > 0 {
							empty = false
							break
						}
					}
					if empty {
						room.HandOver = true
						room.Started = false
						room.Phase = ""
					}
				}
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}
	}
}

// ----------------------------- Game flow helpers -----------------------------

func (h *Hub) startHand(room *Room) {
	h.roomsMu.Lock()
	defer h.roomsMu.Unlock()

	// rotate dealer
	if room.Dealer == -1 {
		room.Dealer = 0
	} else {
		room.Dealer = (room.Dealer + 1) % room.Seats
	}
	room.FirstBidder = (room.Dealer + 1) % room.Seats

	// reset round state
	room.Hands = make(map[int][]Card, room.Seats)
	room.Trick = nil
	room.TrickBy = nil
	room.Turn = room.FirstBidder
	room.HandOver = false
	room.Trump = ""
	room.Started = false
	room.Actor = room.FirstBidder
	room.BestBid = 0
	room.BestBy = -1
	room.Passed = make(map[int]bool)
	room.RoundDouble = false
	room.HasCutPeek = false
	room.CutPeek = Card{}
	room.WeliKeptBy = -1

	room.stock = nil
	room.swamp = nil
	room.swampShuffled = false
	room.exchangeClosed = false
	room.Stayed = make(map[int]bool)
	room.Acted = make(map[int]bool)

	if room.Seats == 3 {
		room.exchangeMax = 3
	} else {
		room.exchangeMax = 3
	}

	room.Phase = "start"
	h.broadcastState(room)
}

func startExchange(room *Room) {
	room.Phase = "exchange"
	room.Actor = room.BestBy
	room.exchangeClosed = false
	room.Stayed = make(map[int]bool)
	room.Acted = make(map[int]bool)
}

func advanceExchangeOrStartPlay(room *Room) {
	allActed := true
	for s := 0; s < room.Seats; s++ {
		if room.PlayerIDs[s] == "" {
			continue
		}
		if !room.Acted[s] {
			allActed = false
			break
		}
	}
	if allActed {
		room.exchangeClosed = true
		room.Phase = "play"
		room.Turn = room.BestBy
		room.Started = true
		return
	}
	next := nextSeat(room, room.Actor)
	for i := 0; i < room.Seats; i++ {
		if room.PlayerIDs[next] != "" && !room.Acted[next] {
			room.Actor = next
			return
		}
		next = nextSeat(room, next)
	}
}

func performCut(room *Room) {
	deck := buildMulatschakDeck()
	shuffle(deck)
	if len(deck) < 2 {
		return
	}
	cut := rand.Intn(len(deck)-1) + 1
	bottom := deck[cut-1]
	room.CutPeek = bottom
	room.HasCutPeek = true
	room.WeliKeptBy = -1
	if isWeli(bottom) {
		room.WeliKeptBy = room.FirstBidder
		deck = append(deck[:cut-1], deck[cut:]...)
	}
	room.stock = deck
}

func deal(room *Room) {
	if room.stock == nil {
		deck := buildMulatschakDeck()
		shuffle(deck)
		room.stock = deck
	}
	// 5 cards to each seat
	for i := 0; i < 5; i++ {
		for s := 0; s < room.Seats; s++ {
			if room.PlayerIDs[s] == "" {
				continue
			}
			if len(room.stock) == 0 {
				break
			}
			room.Hands[s] = append(room.Hands[s], room.stock[0])
			room.stock = room.stock[1:]
		}
	}
	if room.WeliKeptBy >= 0 {
		hasW := false
		for _, c := range room.Hands[room.WeliKeptBy] {
			if isWeli(c) {
				hasW = true
				break
			}
		}
		if !hasW {
			room.Hands[room.WeliKeptBy] = append(room.Hands[room.WeliKeptBy], Card{Suit: "diamonds", Rank: "weli"})
		}
		if len(room.Hands[room.WeliKeptBy]) > 5 {
			room.Hands[room.WeliKeptBy] = room.Hands[room.WeliKeptBy][:5]
		}
	}
	// IMPORTANT: keep remaining room.stock as talon for exchange
}

func buildMulatschakDeck() []Card {
	ranks := []string{"ace", "king", "ober", "unter", "ten", "nine", "eight", "seven"}
	suits := []string{"hearts", "spades", "clubs", "diamonds"}
	deck := make([]Card, 0, 33)
	for _, s := range suits {
		for _, r := range ranks {
			deck = append(deck, Card{Suit: s, Rank: r})
		}
	}
	deck = append(deck, Card{Suit: "diamonds", Rank: "weli"})
	return deck
}

func shuffle(deck []Card) {
	rand.Seed(time.Now().UnixNano())
	n := len(deck)
	for i := n - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		deck[i], deck[j] = deck[j], deck[i]
	}
}

func isWeli(c Card) bool {
	if strings.ToLower(c.Rank) == "weli" {
		return true
	}
	return strings.ToLower(c.Suit) == "diamonds" && (strings.ToLower(c.Rank) == "six" || strings.ToLower(c.Rank) == "6")
}

func nextSeat(room *Room, s int) int {
	if room.Seats == 0 {
		return s
	}
	return (s + 1) % room.Seats
}

func nextActiveBidder(room *Room, from int) int {
	n := room.Seats
	for i := 1; i <= n; i++ {
		s := (from + i) % n
		if room.PlayerIDs[s] != "" && !room.Passed[s] {
			return s
		}
	}
	return from
}

func nextInSeat(room *Room, from int) int {
	n := room.Seats
	for i := 1; i <= n; i++ {
		s := (from + i) % n
		if room.PlayerIDs[s] != "" && !room.Stayed[s] {
			return s
		}
	}
	return from
}

func countInPlayers(room *Room) int {
	cnt := 0
	for s := 0; s < room.Seats; s++ {
		if room.PlayerIDs[s] != "" && !room.Stayed[s] {
			cnt++
		}
	}
	return cnt
}

func toInt(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return 0
	}
}

// ----------------------------- State sending -----------------------------

func (h *Hub) sendStateTo(to *Client, r *Room) {
	counts := make([]int, r.Seats)
	for s := 0; s < r.Seats; s++ {
		counts[s] = len(r.Hands[s])
	}
	names := make([]string, r.Seats)
	for s := 0; s < r.Seats; s++ {
		id := r.PlayerIDs[s]
		h.namesMu.RLock()
		names[s] = h.names[id]
		h.namesMu.RUnlock()
	}

	passed := make([]int, 0, len(r.Passed))
	for s := range r.Passed {
		if r.Passed[s] {
			passed = append(passed, s)
		}
	}
	stayed := make([]int, 0, len(r.Stayed))
	for s := range r.Stayed {
		if r.Stayed[s] {
			stayed = append(stayed, s)
		}
	}

	trick := make([]map[string]any, 0, len(r.Trick))
	for i, c := range r.Trick {
		trick = append(trick, map[string]any{
			"suit": strings.ToLower(c.Suit),
			"rank": strings.ToLower(c.Rank),
			"by":   r.TrickBy[i],
		})
	}

	var cutPeek any
	if r.Phase == "cut" && to.seat == r.FirstBidder && r.HasCutPeek {
		cutPeek = map[string]any{"suit": r.CutPeek.Suit, "rank": r.CutPeek.Rank}
	} else {
		cutPeek = nil
	}

	msg := map[string]any{
		"t": "state",
		"m": map[string]any{
			"room": r.ID,

			"phase": r.Phase,
			"actor": r.Actor,

			"dealer":      r.Dealer,
			"firstBidder": r.FirstBidder,
			"bestBid":     r.BestBid,
			"bestBy":      r.BestBy,
			"passed":      passed,
			"roundDouble": r.RoundDouble,
			"stayed":      stayed,

			"cutPeek": cutPeek,

			"turn":        r.Turn,
			"trump":       r.Trump,
			"lead":        r.Lead,
			"trick":       trick,
			"you":         r.Hands[to.seat],
			"counts":      counts,
			"talon":       len(r.stock),
			"swamp":       len(r.swamp),
			"exchangeMax": r.exchangeMax,
			"started":     r.Started,
			"handOver":    r.HandOver,
			"names":       names,
			"seat":        to.seat,
		},
	}
	b, _ := json.Marshal(msg)
	select {
	case to.send <- b:
	default:
	}
}

func (h *Hub) broadcastState(r *Room) {
	for _, cl := range r.Conns {
		if cl == nil {
			continue
		}
		h.sendStateTo(cl, r)
	}
}
