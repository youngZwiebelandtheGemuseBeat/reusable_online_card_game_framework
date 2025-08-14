package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"example.com/reuasable_online_card_game_framework/internal/ws"
)

func main() {
	port := getenv("PORT", "8080")
	allow := strings.Split(getenv("ORIGIN_ALLOWLIST", "http://localhost:5173,http://localhost:"+port), ",")

	hub := ws.NewHub(allow)
	go hub.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		hub.ServeWS(ctx, w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok")) })

	srv := &http.Server{Addr: ":" + port, Handler: cors(allow, mux)}
	log.Printf("server listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func cors(allow []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, a := range allow {
			if a == origin {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				break
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
