package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"multiscan/internal/server"
)

func main() {
	addr := envOrDefault("SERVER_ADDR", ":8080")
	dbPath := envOrDefault("DB_PATH", "./data/state.db")
	legacyStateFile := envOrDefault("LEGACY_STATE_FILE", "./data/state.json")
	leaseDuration := envDurationOrDefault("LEASE_DURATION", 2*time.Minute)
	syncWait := envDurationOrDefault("SYNC_WAIT", 25*time.Second)
	requiredClientKey := envOrDefault("REQUIRED_CLIENT_KEY", "")

	store, err := server.NewStore(server.Config{
		DBPath:          dbPath,
		LegacyStateFile: legacyStateFile,
		LeaseDuration:   leaseDuration,
	})
	if err != nil {
		log.Fatalf("failed to initialize store: %v", err)
	}

	api := server.NewAPI(store, syncWait, requiredClientKey)

	log.Printf("controller listening on %s", addr)
	log.Printf("sqlite db: %s", dbPath)
	log.Printf("legacy json import source: %s", legacyStateFile)
	if requiredClientKey != "" {
		log.Printf("agent client-key auth: enabled")
	} else {
		log.Printf("agent client-key auth: disabled")
	}
	log.Printf("lease duration: %s, sync wait: %s", leaseDuration, syncWait)
	if err := http.ListenAndServe(addr, api.Routes()); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func envOrDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envDurationOrDefault(name string, fallback time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}
