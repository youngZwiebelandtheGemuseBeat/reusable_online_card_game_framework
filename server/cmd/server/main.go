package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/youngZwiebelandtheGemuseBeat/reusable_online_card_game_framework/server/internal/ws"
)

func main() {
	port := getenv("PORT", "8080")
	allow := strings.Split(getenv("ORIGIN_ALLOWLIST", "http://localhost:"+port+",http://127.0.0.1:"+port), ",")

	hub := ws.NewHub(allow)
	go hub.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, cors(allow, mux)); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func cors(allow []string, next http.Handler) http.Handler {
	allowSet := map[string]struct{}{}
	for _, a := range allow {
		if a != "" {
			allowSet[a] = struct{}{}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := allowSet[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
