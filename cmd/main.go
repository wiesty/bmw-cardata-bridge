package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wiesty/bmw-cardata-bridge/internal/api"
	"github.com/wiesty/bmw-cardata-bridge/internal/bmw"
)

const banner = `
  ______
 /|_||_\'.__
(   _    _ _\
='-(_)--(_)-'

Unofficial BMW CarData Bridge
by wiesty

`

func main() {
	fmt.Print(banner)

	clientID := mustEnv("BMW_CLIENT_ID")
	port := envOr("REST_PORT", "8080")
	dataDir := envOr("DATA_DIR", "/data")
	pollMinutes := parseIntOr(envOr("POLL_INTERVAL_MINUTES", "30"), 30)
	if pollMinutes < 10 {
		pollMinutes = 10
	}

	sessionPath := filepath.Join(dataDir, "session.json")

	auth := bmw.NewAuth(clientID, sessionPath, func(verificationURI, userCode string) {
		fmt.Printf("\n============================================================\n")
		fmt.Printf("BMW Authentication Required\n")
		fmt.Printf("Visit:  %s\n", verificationURI)
		fmt.Printf("Code:   %s\n", userCode)
		fmt.Printf("Waiting for approval...\n")
		fmt.Printf("============================================================\n\n")
	})

	client := bmw.NewClient(auth)
	ctx := context.Background()

	vin, containerID, err := bmw.Bootstrap(ctx, client, dataDir)
	if err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	cache := bmw.NewCache()
	interval := time.Duration(pollMinutes) * time.Minute

	go bmw.StartPoller(ctx, client, vin, containerID, interval, cache, dataDir)

	mux := http.NewServeMux()
	api.RegisterHandlers(mux, cache)

	fmt.Printf("  VIN:      %s\n", vin)
	fmt.Printf("  Endpoint: http://0.0.0.0:%s/vehicle\n", port)
	fmt.Printf("  Poll:     every %d min\n\n", pollMinutes)
	log.Printf("[api] listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseIntOr(s string, fallback int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
