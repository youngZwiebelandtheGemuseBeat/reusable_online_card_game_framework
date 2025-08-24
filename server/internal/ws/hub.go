package ws

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Suit string `json:"Suit"` // keep PascalCase to match client-side expectations for "you"
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
	Trick    []Card // cards on table this trick (server stores Suit/Rank)
	TrickBy  []int  // seat index for each card in Trick
	Turn     int    // current turn seat
	HandOver bool   // round ended

	// Round meta
	Trump   string // "", or hearts/spades/clubs/diamonds
	Started bool   // during trick play

	// Dealer / bidding
	Dealer      int    // rotates each hand; -1 before first deal
	FirstBidder int    // (Dealer+1)%Seats
	Phase       string // "start" | "cut" | "bidding" | "pick_trump" | "play"
	Actor       int    // whose turn to act during start/bidding
	BestBid     int    // 0 = none; 1..5
	BestBy      int    // -1 = none
	Passed      map[int]bool
	RoundDouble bool // set by "start_choice: knock"

	// Cut preview
	CutPeek    Card
	HasCutPeek bool

	// Weli holder after cut (if bottom card was weli)
	WeliKeptBy int // -1 if none

	// internal undealt stock after cut
	stock []Card

	// names for seats (server fills from hub.names when sending)
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
		// local dev: accept all origins when running via --dart-define WS_URL
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

	// On connect, list rooms
	h.sendRoomsList(client)

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
			// ignore
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
	// remove from room if seated
	if c.roomID != "" && c.seat >= 0 {
		h.roomsMu.Lock()
		if room, ok := h.rooms[c.roomID]; ok {
			if room.Conns[c.seat] == c {
				room.Conns[c.seat] = nil
				room.PlayerIDs[c.seat] = ""
				// clear hand visibility on leave
				delete(room.Hands, c.seat)
				// if everyone left, delete room
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
		// pick first empty seat
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

		// greet and sync state
		h.sendRoomsList(c)
		h.sendStateTo(c, room)

		// auto-start when all seats filled
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
			// notify others about join
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
			// perform cut
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
			// start bidding
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

			// end condition: if only one active and we have a bid
			active := 0
			for s := 0; s < room.Seats; s++ {
				if room.PlayerIDs[s] != "" && !room.Passed[s] {
					active++
				}
			}
			if active == 1 && room.BestBy != -1 {
				room.Actor = room.BestBy
				if room.BestBid == 1 {
					room.Trump = "hearts"
					room.Phase = "play"
					room.Turn = room.BestBy
					room.Started = true
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
				// advance to next actor
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
				room.Phase = "play"
				room.Turn = room.BestBy
				room.Started = true
			}
		}
		h.roomsMu.Unlock()
		if room != nil {
			h.broadcastState(room)
		}

	case "move":
		// currently only type: play_card
		roomID := fmt.Sprint(m["room"])
		seat := toInt(m["seat"])
		mv, _ := m["type"].(string)
		h.roomsMu.Lock()
		room := h.rooms[roomID]
		if room != nil && room.Phase == "play" && mv == "play_card" && seat == room.Turn && !room.HandOver {
			cardM, _ := m["card"].(map[string]interface{})
			card := Card{Suit: strings.ToLower(fmt.Sprint(cardM["Suit"])), Rank: strings.ToLower(fmt.Sprint(cardM["Rank"]))}
			// locate card in player's hand
			hi := -1
			for i, c := range room.Hands[seat] {
				if strings.ToLower(c.Suit) == card.Suit && strings.ToLower(c.Rank) == card.Rank {
					hi = i
					break
				}
			}
			if hi >= 0 {
				// play it
				room.Trick = append(room.Trick, room.Hands[seat][hi])
				room.TrickBy = append(room.TrickBy, seat)
				room.Hands[seat] = append(room.Hands[seat][:hi], room.Hands[seat][hi+1:]...)

				// set lead on first card of trick
				if len(room.Trick) == 1 {
					room.Lead = room.Trick[0].Suit
				}

				// advance turn
				room.Turn = nextSeat(room, seat)

				// if trick complete (all players who still have cards played)
				if len(room.TrickBy) == countActivePlayers(room) {
					// naive: trick winner = first card's player (TODO: implement proper winner)
					winner := room.TrickBy[0]
					room.Turn = winner
					room.Trick = nil
					room.TrickBy = nil
					room.Lead = ""

					// check if round over: all hands empty
					empty := true
					for s := 0; s < room.Seats; s++ {
						if len(room.Hands[s]) > 0 && room.PlayerIDs[s] != "" {
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

	// new phase: start (first bidder decides knock or cut)
	room.Phase = "start"

	// names refresh on send
	h.broadcastState(room)
}

func performCut(room *Room) {
	deck := buildMulatschakDeck()
	shuffle(deck)
	// choose a cut point (at least 1 and at most len-1)
	if len(deck) < 2 {
		return
	}
	cut := rand.Intn(len(deck)-1) + 1
	// bottom of lower packet = deck[cut-1]
	bottom := deck[cut-1]
	room.CutPeek = bottom
	room.HasCutPeek = true
	room.WeliKeptBy = -1
	// if bottom is weli -> cutter (firstBidder) keeps it; remove from deck
	if isWeli(bottom) {
		room.WeliKeptBy = room.FirstBidder
		// remove bottom from deck
		deck = append(deck[:cut-1], deck[cut:]...)
	}

	// stock is the deck after cut (no dealing yet)
	room.stock = deck
}

func deal(room *Room) {
	if room.stock == nil {
		deck := buildMulatschakDeck()
		shuffle(deck)
		room.stock = deck
	}
	// give 5 to each player (Mulatschak)
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
	// if weli was kept by cutter, ensure it's in their hand (and trim extra)
	if room.WeliKeptBy >= 0 {
		// ensure weli present
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
		// trim to 5 if over-dealt
		if len(room.Hands[room.WeliKeptBy]) > 5 {
			room.Hands[room.WeliKeptBy] = room.Hands[room.WeliKeptBy][:5]
		}
	}
	// clear stock for now (no exchange implemented)
	room.stock = nil
}

func buildMulatschakDeck() []Card {
	// 33-card William Tell-like deck: A,K,O,U,10,9,8,7 in each suit plus Weli (Diamonds Six)
	// We'll map to rank strings commonly used in code: ace, king, ober, unter, ten, nine, eight, seven
	ranks := []string{"ace", "king", "ober", "unter", "ten", "nine", "eight", "seven"}
	suits := []string{"hearts", "spades", "clubs", "diamonds"}
	deck := make([]Card, 0, 33)
	for _, s := range suits {
		for _, r := range ranks {
			deck = append(deck, Card{Suit: s, Rank: r})
		}
	}
	// add weli (diamonds six)
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

func countActivePlayers(room *Room) int {
	cnt := 0
	for s := 0; s < room.Seats; s++ {
		if room.PlayerIDs[s] != "" {
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
	// names aligned to seats
	names := make([]string, r.Seats)
	for s := 0; s < r.Seats; s++ {
		id := r.PlayerIDs[s]
		h.namesMu.RLock()
		names[s] = h.names[id]
		h.namesMu.RUnlock()
	}

	// passed list for UI
	passed := make([]int, 0, len(r.Passed))
	for s := range r.Passed {
		if r.Passed[s] {
			passed = append(passed, s)
		}
	}

	// trick for UI uses lowercase keys and 'by'
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

			"cutPeek": cutPeek,

			"turn":     r.Turn,
			"trump":    r.Trump,
			"lead":     r.Lead,
			"trick":    trick,
			"you":      r.Hands[to.seat], // private hand
			"counts":   counts,
			"started":  r.Started,
			"handOver": r.HandOver,
			"names":    names,
			"seat":     to.seat,
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

// ----------------------------- Errors -----------------------------

var ErrBadState = errors.New("bad state")
