package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"multiscan/internal/protocol"
	"multiscan/internal/scanner"
	"multiscan/internal/wsproto"
)

func main() {
	agentID := envOrDefault("AGENT_ID", "agent-1")
	serverURL := envOrDefault("SERVER_URL", "http://localhost:8080")
	clientKey := envOrDefault("CLIENT_KEY", "")
	allowRestrictedNets := envBoolOrDefault("ALLOW_RESTRICTED_NET_SCANS", false)
	wsURL := envOrDefault("WS_URL", serverToWSURL(serverURL))
	wsURL = appendClientKeyToURL(wsURL, clientKey)
	heartbeatURL := envOrDefault("HEARTBEAT_URL", serverToHeartbeatURL(serverURL))
	heartbeatInterval := envDurationOrDefault("HEARTBEAT_INTERVAL", 5*time.Second)
	wsReadTimeout := envDurationOrDefault("WS_READ_TIMEOUT", 40*time.Second)
	wsWriteTimeout := envDurationOrDefault("WS_WRITE_TIMEOUT", 10*time.Second)
	retryDelay := envDurationOrDefault("RETRY_DELAY", 2*time.Second)
	heartbeatClient := &http.Client{Timeout: 5 * time.Second}

	var completion *protocol.JobCompletion
	log.Printf("agent %s connecting websocket %s", agentID, wsURL)

	for {
		if err := runSession(agentID, wsURL, heartbeatURL, clientKey, allowRestrictedNets, heartbeatInterval, wsReadTimeout, wsWriteTimeout, heartbeatClient, &completion); err != nil {
			log.Printf("websocket session error: %v", err)
			time.Sleep(retryDelay)
		}
	}
}

func runSession(agentID, wsURL, heartbeatURL, clientKey string, allowRestrictedNets bool, heartbeatInterval, wsReadTimeout, wsWriteTimeout time.Duration, heartbeatClient *http.Client, completion **protocol.JobCompletion) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := wsproto.Dial(ctx, wsURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	for {
		req := protocol.SyncRequest{
			AgentID:             agentID,
			AllowRestrictedNets: allowRestrictedNets,
			Completion:          *completion,
		}
		if wsWriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		}
		if err := conn.WriteJSON(req); err != nil {
			return err
		}
		if wsWriteTimeout > 0 {
			_ = conn.SetWriteDeadline(time.Time{})
		}

		var resp protocol.SyncResponse
		if wsReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		}
		if err := conn.ReadJSON(&resp); err != nil {
			if wsReadTimeout > 0 && (errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout")) {
				return err
			}
			return err
		}
		if wsReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Time{})
		}

		if *completion != nil {
			if strings.HasPrefix(resp.Message, "sync error:") {
				log.Printf("completion rejected for %s: %s", (*completion).JobID, resp.Message)
			}
			*completion = nil
		}

		if resp.Action != protocol.SyncActionWork || resp.Job == nil {
			wait := time.Duration(resp.WaitSeconds) * time.Second
			if wait <= 0 {
				wait = 300 * time.Millisecond
			}
			time.Sleep(wait)
			continue
		}

		job := resp.Job
		if len(job.Ports) > 0 {
			log.Printf("received %s attempt %d/%d: %s-%s with %d explicit ports", job.ID, job.Attempts, job.MaxAttempts, job.StartIP, job.EndIP, len(job.Ports))
		} else {
			log.Printf("received %s attempt %d/%d: %s-%s ports %d-%d", job.ID, job.Attempts, job.MaxAttempts, job.StartIP, job.EndIP, job.StartPort, job.EndPort)
		}
		scanCtx, cancelScan := context.WithCancel(context.Background())
		stopHeartbeat := startHeartbeat(heartbeatClient, heartbeatURL, clientKey, agentID, job.ID, allowRestrictedNets, heartbeatInterval, func(reason string) {
			log.Printf("stop requested for %s: %s", job.ID, reason)
			cancelScan()
		})
		scanCfg := scanner.Config{Timeout: 600 * time.Millisecond, Concurrency: 256}
		var (
			results []protocol.ScanResult
			scanErr error
		)
		if len(job.Ports) > 0 {
			results, scanErr = scanner.ScanPortsContext(scanCtx, job.StartIP, job.EndIP, job.Ports, scanCfg)
		} else {
			results, scanErr = scanner.ScanRangeContext(scanCtx, job.StartIP, job.EndIP, job.StartPort, job.EndPort, scanCfg)
		}
		stopHeartbeat()
		cancelScan()
		if scanErr != nil {
			*completion = &protocol.JobCompletion{JobID: job.ID, Error: scanErr.Error()}
			log.Printf("scan failed for %s: %v", job.ID, scanErr)
			continue
		}

		openResults := filterOpenResults(results)
		*completion = &protocol.JobCompletion{JobID: job.ID, Results: openResults}
		log.Printf("scan complete for %s with %d open endpoints retained (from %d scanned)", job.ID, len(openResults), len(results))
	}
}

func startHeartbeat(client *http.Client, heartbeatURL, clientKey, agentID, jobID string, allowRestrictedNets bool, interval time.Duration, onStop func(reason string)) func() {
	if interval <= 0 || heartbeatURL == "" {
		return func() {}
	}
	if onStop == nil {
		onStop = func(string) {}
	}

	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(stopCh) })
	}

	sendHeartbeat(client, heartbeatURL, clientKey, agentID, jobID, allowRestrictedNets, onStop)
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sendHeartbeat(client, heartbeatURL, clientKey, agentID, jobID, allowRestrictedNets, onStop)
			case <-stopCh:
				return
			}
		}
	}()
	return stop
}

func sendHeartbeat(client *http.Client, heartbeatURL, clientKey, agentID, jobID string, allowRestrictedNets bool, onStop func(reason string)) {
	reqBody, err := json.Marshal(protocol.HeartbeatRequest{
		AgentID:             agentID,
		CurrentJobID:        jobID,
		AllowRestrictedNets: allowRestrictedNets,
	})
	if err != nil {
		log.Printf("heartbeat marshal error: %v", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, heartbeatURL, bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("heartbeat request build error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if clientKey != "" {
		req.Header.Set("X-Client-Key", clientKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("heartbeat post error: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("heartbeat rejected: %s", resp.Status)
		return
	}
	var hbResp protocol.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		log.Printf("heartbeat decode error: %v", err)
		return
	}
	if hbResp.StopJob {
		onStop(hbResp.Reason)
	}
}

func filterOpenResults(results []protocol.ScanResult) []protocol.ScanResult {
	out := make([]protocol.ScanResult, 0, len(results))
	for _, r := range results {
		if r.Open {
			out = append(out, r)
		}
	}
	return out
}

func serverToWSURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "ws://localhost:8080/ws"
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func serverToHeartbeatURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "http://localhost:8080/heartbeat"
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		u.Scheme = "http"
	}
	u.Path = "/heartbeat"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func appendClientKeyToURL(rawURL, clientKey string) string {
	if clientKey == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("client_key", clientKey)
	u.RawQuery = q.Encode()
	return u.String()
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
		log.Printf("invalid duration for %s=%q, using %s", name, v, fallback)
	}
	return fallback
}

func envBoolOrDefault(name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		log.Printf("invalid boolean for %s=%q, using %t", name, v, fallback)
		return fallback
	}
}
