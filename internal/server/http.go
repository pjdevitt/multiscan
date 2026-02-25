package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"multiscan/internal/protocol"
	"multiscan/internal/wsproto"
)

var ipv4Pattern = regexp.MustCompile(`^((25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})\.){3}(25[0-5]|2[0-4][0-9]|1?[0-9]{1,2})$`)

// API serves the controller endpoints.
type API struct {
	store             *Store
	syncMaxWait       time.Duration
	requiredClientKey string
	uiBasicAuthUser   string
	uiBasicAuthPass   string
}

func NewAPI(store *Store, syncMaxWait time.Duration, requiredClientKey, uiBasicAuthUser, uiBasicAuthPass string) *API {
	if syncMaxWait <= 0 {
		syncMaxWait = 25 * time.Second
	}
	return &API{
		store:             store,
		syncMaxWait:       syncMaxWait,
		requiredClientKey: strings.TrimSpace(requiredClientKey),
		uiBasicAuthUser:   strings.TrimSpace(uiBasicAuthUser),
		uiBasicAuthPass:   uiBasicAuthPass,
	}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/jobs", a.handleJobs)
	mux.HandleFunc("/sync", a.handleSync)
	mux.HandleFunc("/ws", a.handleWS)
	mux.HandleFunc("/heartbeat", a.handleHeartbeat)
	mux.HandleFunc("/work/", a.handleWorkByID)
	mux.HandleFunc("/api/jobs", a.handleAPIJobs)
	mux.HandleFunc("/api/jobs/stop", a.handleAPIStopJob)
	mux.HandleFunc("/api/agents", a.handleAPIAgents)
	mux.HandleFunc("/", a.handleUI)
	return loggingMiddleware(mux)
}

func (a *API) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	a.createJobFromRequest(w, r)
}

func (a *API) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUIRequest(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string][]protocol.JobListItem{"jobs": a.store.ListJobs()})
	case http.MethodPost:
		a.createJobFromRequest(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (a *API) handleAPIAgents(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUIRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	offlineAfter := 2 * a.syncMaxWait
	writeJSON(w, http.StatusOK, map[string][]protocol.AgentStatus{"agents": a.store.ListAgents(offlineAfter)})
}

func (a *API) handleAPIStopJob(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUIRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req.JobID = strings.TrimSpace(req.JobID)
	if req.JobID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job_id is required"})
		return
	}
	if err := a.store.StopJob(req.JobID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) createJobFromRequest(w http.ResponseWriter, r *http.Request) {
	var req protocol.SubmitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req, err := normalizeSubmitRequest(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateJobRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	parent, subJobs, err := a.store.CreateJobs(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	jobIDs := make([]string, 0, len(subJobs))
	for _, job := range subJobs {
		jobIDs = append(jobIDs, job.ID)
	}
	resp := protocol.SubmitJobResponse{
		JobCount: len(jobIDs),
		JobIDs:   jobIDs,
	}
	resp.JobID = parent.ID
	writeJSON(w, http.StatusCreated, resp)
}

func (a *API) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !a.authorizeAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req protocol.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	resp, status, err := a.processSync(req)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !a.authorizeAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var req protocol.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}
	a.store.Heartbeat(req.AgentID, strings.TrimSpace(req.CurrentJobID), req.AllowRestrictedNets)
	stopJob, reason := a.store.ShouldStopJob(strings.TrimSpace(req.CurrentJobID))
	writeJSON(w, http.StatusOK, protocol.HeartbeatResponse{
		Status:  "ok",
		StopJob: stopJob,
		Reason:  reason,
	})
}

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !a.authorizeAgentRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	conn, err := wsproto.Upgrade(w, r)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	for {
		var req protocol.SyncRequest
		if err := conn.ReadJSON(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("websocket read error: %v", err)
			}
			return
		}

		resp, _, err := a.processSync(req)
		if err != nil {
			resp = protocol.SyncResponse{
				Action:      protocol.SyncActionWait,
				WaitSeconds: 1,
				Message:     "sync error: " + err.Error(),
			}
		}
		if err := conn.WriteJSON(resp); err != nil {
			log.Printf("websocket write error: %v", err)
			return
		}
	}
}

func (a *API) processSync(req protocol.SyncRequest) (protocol.SyncResponse, int, error) {
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" {
		return protocol.SyncResponse{}, http.StatusBadRequest, errors.New("agent_id is required")
	}
	a.store.TouchAgent(req.AgentID, req.AllowRestrictedNets)

	if req.Completion != nil {
		if strings.TrimSpace(req.Completion.JobID) == "" {
			return protocol.SyncResponse{}, http.StatusBadRequest, errors.New("completion.job_id is required")
		}
		if strings.TrimSpace(req.Completion.Error) != "" {
			if err := a.store.FailJobAttempt(req.Completion.JobID, req.AgentID, req.Completion.Error); err != nil {
				return protocol.SyncResponse{}, http.StatusBadRequest, err
			}
		} else {
			if err := a.store.CompleteJob(req.Completion.JobID, req.AgentID, req.Completion.Results); err != nil {
				return protocol.SyncResponse{}, http.StatusBadRequest, err
			}
		}
	}

	job, err := a.store.WaitAndAssign(req.AgentID, a.syncMaxWait)
	if err != nil {
		if errors.Is(err, errNoQueuedJobs) {
			return protocol.SyncResponse{
				Action:      protocol.SyncActionWait,
				WaitSeconds: int(a.syncMaxWait.Seconds()),
				Message:     "no jobs available",
			}, http.StatusOK, nil
		}
		return protocol.SyncResponse{}, http.StatusInternalServerError, err
	}

	return protocol.SyncResponse{Action: protocol.SyncActionWork, Job: &job}, http.StatusOK, nil
}

func (a *API) handleWorkByID(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeUIRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/work/")
	jobID := strings.TrimSpace(path)
	if jobID == "" || strings.Contains(jobID, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	details, err := a.store.GetJobDetails(jobID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func validateJobRequest(req protocol.SubmitJobRequest) error {
	if strings.TrimSpace(req.StartIP) == "" || strings.TrimSpace(req.EndIP) == "" {
		return errors.New("provide hostname or both start_ip and end_ip")
	}
	if !ipv4Pattern.MatchString(req.StartIP) || !ipv4Pattern.MatchString(req.EndIP) {
		return errors.New("start_ip and end_ip must be valid IPv4 addresses")
	}
	if req.TopN < 0 {
		return errors.New("top_n must be >= 0")
	}
	if len(req.Ports) > 0 {
		for _, p := range req.Ports {
			if p <= 0 || p > 65535 {
				return errors.New("ports must be in range 1-65535")
			}
		}
	} else {
		if req.StartPort <= 0 || req.EndPort <= 0 || req.StartPort > req.EndPort || req.EndPort > 65535 {
			return errors.New("invalid port range")
		}
	}
	if req.MaxAttempts < 0 {
		return errors.New("max_attempts must be >= 0")
	}
	return nil
}

func normalizeSubmitRequest(req protocol.SubmitJobRequest) (protocol.SubmitJobRequest, error) {
	req.Hostname = strings.TrimSpace(req.Hostname)
	req.StartIP = strings.TrimSpace(req.StartIP)
	req.EndIP = strings.TrimSpace(req.EndIP)

	if req.Hostname != "" {
		ip, err := resolveHostnameIPv4(req.Hostname)
		if err != nil {
			return req, err
		}
		req.StartIP = ip
		req.EndIP = ip
	}

	if req.Top1000 && req.TopN == 0 {
		req.TopN = 1000
	}
	if req.TopN > 0 {
		req.Ports = getTopNPorts(req.TopN)
		req.Top1000 = req.TopN == 1000
	}
	if len(req.Ports) > 0 {
		seen := make(map[int]struct{}, len(req.Ports))
		dedup := make([]int, 0, len(req.Ports))
		for _, p := range req.Ports {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			dedup = append(dedup, p)
		}
		sort.Ints(dedup)
		req.Ports = dedup
		req.StartPort = dedup[0]
		req.EndPort = dedup[len(dedup)-1]
	}
	return req, nil
}

func resolveHostnameIPv4(hostname string) (string, error) {
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return "", errors.New("failed to resolve hostname: " + hostname)
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", errors.New("hostname did not resolve to an IPv4 address: " + hostname)
}

func (a *API) authorizeAgentRequest(r *http.Request) bool {
	if a.requiredClientKey == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("X-Client-Key"))
	if provided == "" {
		provided = strings.TrimSpace(r.URL.Query().Get("client_key"))
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.requiredClientKey)) == 1
}

func (a *API) authorizeUIRequest(w http.ResponseWriter, r *http.Request) bool {
	if a.uiBasicAuthUser == "" || a.uiBasicAuthPass == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="multiscan-ui"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.uiBasicAuthUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.uiBasicAuthPass)) == 1
	if !userOK || !passOK {
		w.Header().Set("WWW-Authenticate", `Basic realm="multiscan-ui"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
