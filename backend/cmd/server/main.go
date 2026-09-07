package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"example.com/rock-paper-money/internal/room"
	"example.com/rock-paper-money/internal/web"
)

func main() {
	rooms := room.NewStore()
	server := &http.Server{
		Addr:              "0.0.0.0:8080",
		Handler:           web.NewRouter(rooms),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("server listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
