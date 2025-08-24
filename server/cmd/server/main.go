package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/youngZwiebelandtheGemuseBeat/reusable_online_card_game_framework/server/internal/ws"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	allowCSV := os.Getenv("WS_ALLOW_ORIGINS")
	var allow []string
	if allowCSV != "" {
		for _, s := range strings.Split(allowCSV, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				allow = append(allow, s)
			}
		}
	} else {
		allow = []string{
			"http://localhost:5173",
			"http://127.0.0.1:5173",
			"http://localhost:" + port,
			"http://127.0.0.1:" + port,
		}
	}

	hub := ws.NewHub(allow)
	mux := http.NewServeMux()

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(w, r)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Reusable Card Game Server running. WebSocket at /ws\n"))
	})

	addr := ":" + port
	log.Printf("Server listening on %s", addr)
	log.Printf("Allowed Origins: %v", allow)
	log.Printf("WebSocket endpoint: ws://localhost:%s/ws", port)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
