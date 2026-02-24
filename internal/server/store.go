package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"multiscan/internal/protocol"
)

var (
	errNoQueuedJobs = errors.New("no queued jobs")
)

// Config controls durability and coordination behavior.
type Config struct {
	DBPath          string
	LegacyStateFile string
	LeaseDuration   time.Duration
}

type agentRecord struct {
	LastSeen            time.Time `json:"last_seen"`
	CurrentJobID        string    `json:"current_job_id,omitempty"`
	AllowRestrictedNets bool      `json:"allow_restricted_nets"`
	CompletedJobs       int       `json:"completed_jobs"`
	FailedJobs          int       `json:"failed_jobs"`
}

type persistedState struct {
	Jobs    map[string]protocol.Job          `json:"jobs"`
	Order   []string                         `json:"order"`
	Results map[string][]protocol.ScanResult `json:"results"`
	Agents  map[string]agentRecord           `json:"agents"`
	NextID  int                              `json:"next_id"`
}

// Store holds job state and synchronization while persisting to SQLite.
type Store struct {
	mu            sync.Mutex
	jobs          map[string]protocol.Job
	order         []string
	results       map[string][]protocol.ScanResult
	agents        map[string]agentRecord
	nextID        int
	db            *sql.DB
	dbPath        string
	legacyState   string
	leaseDuration time.Duration
}

const portBatchSize = 1024

var restrictedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
}

func NewStore(cfg Config) (*Store, error) {
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/state.db"
	}
	if cfg.LegacyStateFile == "" {
		cfg.LegacyStateFile = "./data/state.json"
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 2 * time.Minute
	}

	s := &Store{
		jobs:          make(map[string]protocol.Job),
		order:         make([]string, 0),
		results:       make(map[string][]protocol.ScanResult),
		agents:        make(map[string]agentRecord),
		nextID:        1,
		dbPath:        cfg.DBPath,
		legacyState:   cfg.LegacyStateFile,
		leaseDuration: cfg.LeaseDuration,
	}
	db, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}
	s.db = db
	if err := s.initSQLite(); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	if err := s.load(); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) CreateJob(req protocol.SubmitJobRequest) (protocol.Job, error) {
	parent, _, err := s.CreateJobs(req)
	return parent, err
}

func (s *Store) CreateJobs(req protocol.SubmitJobRequest) (protocol.Job, []protocol.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	portList := normalizeRequestedPorts(req)
	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	parentID := fmt.Sprintf("job-%04d", s.nextID)
	s.nextID++
	parent := protocol.Job{
		ID:          parentID,
		StartIP:     req.StartIP,
		EndIP:       req.EndIP,
		StartPort:   req.StartPort,
		EndPort:     req.EndPort,
		Ports:       append([]int(nil), portList...),
		PortCount:   portCountForJob(req.StartPort, req.EndPort, portList),
		Status:      protocol.JobQueued,
		MaxAttempts: maxAttempts,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.jobs[parentID] = parent
	s.order = append(s.order, parentID)

	var chunks [][]int
	if len(portList) > 0 {
		chunks = splitPortList(portList, portBatchSize)
	} else {
		for _, pr := range splitPortRanges(req.StartPort, req.EndPort, portBatchSize) {
			chunks = append(chunks, rangeToPorts(pr.start, pr.end))
		}
	}
	targetIPs, err := enumerateIPv4Range(req.StartIP, req.EndIP)
	if err != nil {
		return protocol.Job{}, nil, err
	}
	subJobs := make([]protocol.Job, 0, len(chunks)*len(targetIPs))
	for _, ip := range targetIPs {
		for _, chunk := range chunks {
			id := fmt.Sprintf("job-%04d", s.nextID)
			s.nextID++
			startPort, endPort := minMaxPorts(chunk)
			job := protocol.Job{
				ID:          id,
				ParentJobID: parentID,
				StartIP:     ip,
				EndIP:       ip,
				StartPort:   startPort,
				EndPort:     endPort,
				Ports:       append([]int(nil), chunk...),
				PortCount:   len(chunk),
				Status:      protocol.JobQueued,
				Attempts:    0,
				MaxAttempts: maxAttempts,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			s.jobs[id] = job
			s.order = append(s.order, id)
			subJobs = append(subJobs, job)
		}
	}

	if err := s.saveLocked(); err != nil {
		return protocol.Job{}, nil, err
	}
	return parent, subJobs, nil
}

func (s *Store) CompleteJob(jobID, agentID string, results []protocol.ScanResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completeJobLocked(jobID, agentID, results)
}

func (s *Store) FailJobAttempt(jobID, agentID, agentErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failJobAttemptLocked(jobID, agentID, agentErr)
}

func (s *Store) TouchAgent(agentID string, allowRestrictedNets bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchAgentLocked(agentID, "", allowRestrictedNets)
}

func (s *Store) Heartbeat(agentID, currentJobID string, allowRestrictedNets bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchAgentLocked(agentID, currentJobID, allowRestrictedNets)
}

func (s *Store) WaitAndAssign(agentID string, maxWait time.Duration) (protocol.Job, error) {
	if maxWait <= 0 {
		maxWait = 25 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for {
		s.mu.Lock()
		allowRestricted := s.agents[agentID].AllowRestrictedNets
		s.touchAgentLocked(agentID, "", allowRestricted)
		s.requeueExpiredLocked(time.Now().UTC())
		job, err := s.assignNextLocked(agentID)
		s.mu.Unlock()
		if err == nil {
			return job, nil
		}
		if !errors.Is(err, errNoQueuedJobs) {
			return protocol.Job{}, err
		}
		if time.Now().After(deadline) {
			return protocol.Job{}, errNoQueuedJobs
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (s *Store) GetJobDetails(jobID string) (protocol.JobDetails, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return protocol.JobDetails{}, fmt.Errorf("job %s not found", jobID)
	}

	childIdx := s.childIndexLocked()
	childIDs := childIdx[jobID]
	if len(childIDs) > 0 {
		aggJob, results := s.aggregateParentDetailsLocked(job, childIDs)
		return protocol.JobDetails{Job: aggJob, Results: results}, nil
	}

	res := append([]protocol.ScanResult(nil), s.results[jobID]...)
	sortResultsByIPThenPort(res)
	return protocol.JobDetails{Job: job, Results: res}, nil
}

func (s *Store) ListJobs() []protocol.JobListItem {
	s.mu.Lock()
	defer s.mu.Unlock()

	childIdx := s.childIndexLocked()
	items := make([]protocol.JobListItem, 0)
	for _, id := range s.order {
		job, ok := s.jobs[id]
		if !ok {
			continue
		}
		if job.ParentJobID != "" {
			continue
		}
		if childIDs := childIdx[id]; len(childIDs) > 0 {
			items = append(items, s.summarizeParentLocked(job, childIDs))
			continue
		}
		items = append(items, summarizeStandaloneJob(job, s.results[id]))
	}
	return items
}

func (s *Store) ListAgents(offlineAfter time.Duration) []protocol.AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	if offlineAfter <= 0 {
		offlineAfter = 45 * time.Second
	}
	now := time.Now().UTC()
	out := make([]protocol.AgentStatus, 0, len(s.agents))
	for id, rec := range s.agents {
		state := "idle"
		if rec.CurrentJobID != "" {
			state = "busy"
		} else if now.Sub(rec.LastSeen) > offlineAfter {
			state = "offline"
		}
		out = append(out, protocol.AgentStatus{
			AgentID:             id,
			State:               state,
			LastSeen:            rec.LastSeen,
			CurrentJobID:        rec.CurrentJobID,
			AllowRestrictedNets: rec.AllowRestrictedNets,
			CompletedJobs:       rec.CompletedJobs,
			FailedJobs:          rec.FailedJobs,
		})
	}
	return out
}

func (s *Store) assignNextLocked(agentID string) (protocol.Job, error) {
	allowRestricted := s.agents[agentID].AllowRestrictedNets
	childIdx := s.childIndexLocked()
	for _, id := range s.order {
		job, ok := s.jobs[id]
		if !ok {
			continue
		}
		if job.Status != protocol.JobQueued {
			continue
		}
		if job.ParentJobID == "" && len(childIdx[id]) > 0 {
			continue
		}
		if !allowRestricted && jobTargetsRestrictedNets(job) {
			continue
		}

		now := time.Now().UTC()
		lease := now.Add(s.leaseDuration)
		job.Status = protocol.JobInProgress
		job.AssignedTo = agentID
		job.Attempts++
		job.LeaseExpiresAt = &lease
		job.UpdatedAt = now
		s.jobs[id] = job

		ag := s.agents[agentID]
		ag.LastSeen = now
		ag.CurrentJobID = id
		s.agents[agentID] = ag

		if err := s.saveLocked(); err != nil {
			return protocol.Job{}, err
		}
		return job, nil
	}
	return protocol.Job{}, errNoQueuedJobs
}

func (s *Store) completeJobLocked(jobID, agentID string, results []protocol.ScanResult) error {
	job, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("job %s not found", jobID)
	}
	if job.Status == protocol.JobCompleted || job.Status == protocol.JobFailed {
		return nil
	}
	if job.Status != protocol.JobInProgress {
		return fmt.Errorf("job %s is not in progress", jobID)
	}
	if job.AssignedTo != agentID {
		return fmt.Errorf("job %s assigned to %s, not %s", jobID, job.AssignedTo, agentID)
	}

	now := time.Now().UTC()
	job.Status = protocol.JobCompleted
	job.UpdatedAt = now
	job.LeaseExpiresAt = nil
	job.LastError = ""
	s.jobs[jobID] = job
	openResults := retainOpenResults(results)
	sortResultsByIPThenPort(openResults)
	s.results[jobID] = append([]protocol.ScanResult(nil), openResults...)

	ag := s.agents[agentID]
	ag.LastSeen = now
	ag.CompletedJobs++
	if ag.CurrentJobID == jobID {
		ag.CurrentJobID = ""
	}
	s.agents[agentID] = ag

	return s.saveLocked()
}

func (s *Store) failJobAttemptLocked(jobID, agentID, agentErr string) error {
	job, ok := s.jobs[jobID]
	if !ok {
		return fmt.Errorf("job %s not found", jobID)
	}
	if job.Status == protocol.JobCompleted || job.Status == protocol.JobFailed {
		return nil
	}
	if job.Status != protocol.JobInProgress {
		return fmt.Errorf("job %s is not in progress", jobID)
	}
	if job.AssignedTo != agentID {
		return fmt.Errorf("job %s assigned to %s, not %s", jobID, job.AssignedTo, agentID)
	}

	now := time.Now().UTC()
	job.UpdatedAt = now
	job.LastError = agentErr
	if job.Attempts >= job.MaxAttempts {
		job.Status = protocol.JobFailed
		job.LeaseExpiresAt = nil
	} else {
		job.Status = protocol.JobQueued
		job.AssignedTo = ""
		job.LeaseExpiresAt = nil
	}
	s.jobs[jobID] = job

	ag := s.agents[agentID]
	ag.LastSeen = now
	ag.FailedJobs++
	if ag.CurrentJobID == jobID {
		ag.CurrentJobID = ""
	}
	s.agents[agentID] = ag

	return s.saveLocked()
}

func (s *Store) requeueExpiredLocked(now time.Time) {
	changed := false
	childIdx := s.childIndexLocked()
	for _, id := range s.order {
		job, ok := s.jobs[id]
		if !ok {
			continue
		}
		if job.ParentJobID == "" && len(childIdx[id]) > 0 {
			continue
		}
		if job.Status != protocol.JobInProgress || job.LeaseExpiresAt == nil || now.Before(*job.LeaseExpiresAt) {
			continue
		}
		expiredAgent := job.AssignedTo
		job.UpdatedAt = now
		job.LastError = "lease expired before completion"
		job.AssignedTo = ""
		job.LeaseExpiresAt = nil
		if job.Attempts >= job.MaxAttempts {
			job.Status = protocol.JobFailed
		} else {
			job.Status = protocol.JobQueued
		}
		s.jobs[id] = job

		if expiredAgent != "" {
			ag := s.agents[expiredAgent]
			if ag.CurrentJobID == id {
				ag.CurrentJobID = ""
				s.agents[expiredAgent] = ag
			}
		}
		changed = true
	}
	if changed {
		_ = s.saveLocked()
	}
}

func (s *Store) touchAgentLocked(agentID, currentJobID string, allowRestrictedNets bool) {
	rec := s.agents[agentID]
	rec.LastSeen = time.Now().UTC()
	rec.AllowRestrictedNets = allowRestrictedNets
	if currentJobID != "" {
		rec.CurrentJobID = currentJobID
	}
	s.agents[agentID] = rec
}

func (s *Store) load() error {
	s.jobs = make(map[string]protocol.Job)
	s.order = make([]string, 0)
	s.results = make(map[string][]protocol.ScanResult)
	s.agents = make(map[string]agentRecord)
	s.nextID = 1

	loaded, err := s.loadFromRelationalLocked()
	if err != nil {
		return err
	}
	if loaded {
		return nil
	}

	migrated, err := s.migrateFromBlobTableLocked()
	if err != nil {
		return err
	}
	if migrated {
		return nil
	}

	data, err := os.ReadFile(s.legacyState)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read legacy state file: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return fmt.Errorf("unmarshal legacy state: %w", err)
	}
	s.applyPersistedState(ps)
	return s.saveLocked()
}

func (s *Store) loadFromRelationalLocked() (bool, error) {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs;").Scan(&count); err != nil {
		return false, fmt.Errorf("count jobs: %w", err)
	}
	if count == 0 {
		return false, nil
	}

	jobRows, err := s.db.Query("SELECT id,parent_job_id,start_ip,end_ip,start_port,end_port,ports_json,port_count,status,assigned_to,attempts,max_attempts,lease_expires_at,last_error,created_at,updated_at FROM jobs;")
	if err != nil {
		return false, fmt.Errorf("query jobs: %w", err)
	}
	defer jobRows.Close()
	for jobRows.Next() {
		var id string
		var parentJobID sql.NullString
		var startIP string
		var endIP string
		var startPort int
		var endPort int
		var portsJSON sql.NullString
		var portCount int
		var status string
		var assignedTo sql.NullString
		var attempts int
		var maxAttempts int
		var leaseExpiresAt sql.NullString
		var lastError sql.NullString
		var createdAtRaw string
		var updatedAtRaw string
		if err := jobRows.Scan(
			&id, &parentJobID, &startIP, &endIP, &startPort, &endPort, &portsJSON, &portCount,
			&status, &assignedTo, &attempts, &maxAttempts, &leaseExpiresAt, &lastError, &createdAtRaw, &updatedAtRaw,
		); err != nil {
			return false, fmt.Errorf("scan jobs row: %w", err)
		}
		createdAt, err := parseTime(createdAtRaw)
		if err != nil {
			return false, err
		}
		updatedAt, err := parseTime(updatedAtRaw)
		if err != nil {
			return false, err
		}
		var lease *time.Time
		if leaseExpiresAt.Valid && strings.TrimSpace(leaseExpiresAt.String) != "" {
			t, err := parseTime(leaseExpiresAt.String)
			if err != nil {
				return false, err
			}
			lease = &t
		}
		job := protocol.Job{
			ID:             id,
			StartIP:        startIP,
			EndIP:          endIP,
			StartPort:      startPort,
			EndPort:        endPort,
			PortCount:      portCount,
			Status:         protocol.JobStatus(status),
			Attempts:       attempts,
			MaxAttempts:    maxAttempts,
			LeaseExpiresAt: lease,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
		}
		if portsJSON.Valid && strings.TrimSpace(portsJSON.String) != "" {
			var ports []int
			if err := json.Unmarshal([]byte(portsJSON.String), &ports); err == nil {
				job.Ports = ports
			}
		}
		if job.PortCount == 0 {
			job.PortCount = portCountForJob(job.StartPort, job.EndPort, job.Ports)
		}
		if parentJobID.Valid {
			job.ParentJobID = parentJobID.String
		}
		if assignedTo.Valid {
			job.AssignedTo = assignedTo.String
		}
		if lastError.Valid {
			job.LastError = lastError.String
		}
		s.jobs[job.ID] = job
	}
	if err := jobRows.Err(); err != nil {
		return false, fmt.Errorf("iterate jobs rows: %w", err)
	}

	orderRows, err := s.db.Query("SELECT job_id FROM job_order ORDER BY position ASC;")
	if err != nil {
		return false, fmt.Errorf("query job_order: %w", err)
	}
	defer orderRows.Close()
	for orderRows.Next() {
		var jobID string
		if err := orderRows.Scan(&jobID); err != nil {
			return false, fmt.Errorf("scan job_order row: %w", err)
		}
		s.order = append(s.order, jobID)
	}
	if err := orderRows.Err(); err != nil {
		return false, fmt.Errorf("iterate job_order rows: %w", err)
	}
	if len(s.order) == 0 {
		for id := range s.jobs {
			s.order = append(s.order, id)
		}
		sort.Strings(s.order)
	}

	resultRows, err := s.db.Query("SELECT job_id,ip,port,open,err FROM results ORDER BY job_id,ip,port;")
	if err != nil {
		return false, fmt.Errorf("query results: %w", err)
	}
	defer resultRows.Close()
	for resultRows.Next() {
		var jobID string
		var ip string
		var port int
		var open int
		var errMsg sql.NullString
		if err := resultRows.Scan(&jobID, &ip, &port, &open, &errMsg); err != nil {
			return false, fmt.Errorf("scan results row: %w", err)
		}
		sr := protocol.ScanResult{IP: ip, Port: port, Open: open != 0}
		if errMsg.Valid {
			sr.Err = errMsg.String
		}
		s.results[jobID] = append(s.results[jobID], sr)
	}
	if err := resultRows.Err(); err != nil {
		return false, fmt.Errorf("iterate results rows: %w", err)
	}

	agentRows, err := s.db.Query("SELECT agent_id,last_seen,current_job_id,allow_restricted_nets,completed_jobs,failed_jobs FROM agents;")
	if err != nil {
		return false, fmt.Errorf("query agents: %w", err)
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var agentID string
		var lastSeenRaw string
		var currentJobID sql.NullString
		var allowRestrictedNets int
		var completedJobs int
		var failedJobs int
		if err := agentRows.Scan(&agentID, &lastSeenRaw, &currentJobID, &allowRestrictedNets, &completedJobs, &failedJobs); err != nil {
			return false, fmt.Errorf("scan agents row: %w", err)
		}
		ls, err := parseTime(lastSeenRaw)
		if err != nil {
			return false, err
		}
		ag := agentRecord{
			LastSeen:            ls,
			AllowRestrictedNets: allowRestrictedNets != 0,
			CompletedJobs:       completedJobs,
			FailedJobs:          failedJobs,
		}
		if currentJobID.Valid {
			ag.CurrentJobID = currentJobID.String
		}
		s.agents[agentID] = ag
	}
	if err := agentRows.Err(); err != nil {
		return false, fmt.Errorf("iterate agents rows: %w", err)
	}

	var nextIDRaw sql.NullString
	if err := s.db.QueryRow("SELECT value FROM meta WHERE key='next_id';").Scan(&nextIDRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("query next_id meta: %w", err)
	}
	if nextIDRaw.Valid {
		if n, err := strconv.Atoi(nextIDRaw.String); err == nil && n > 0 {
			s.nextID = n
		}
	}
	if s.nextID <= 1 {
		s.nextID = inferNextID(s.jobs)
	}

	return true, nil
}

func (s *Store) migrateFromBlobTableLocked() (bool, error) {
	exists, err := s.tableExists("multiscan_state")
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var stateJSON sql.NullString
	if err := s.db.QueryRow("SELECT state_json FROM multiscan_state WHERE id=1;").Scan(&stateJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("query multiscan_state: %w", err)
	}
	if !stateJSON.Valid || strings.TrimSpace(stateJSON.String) == "" {
		return false, nil
	}
	var ps persistedState
	if err := json.Unmarshal([]byte(stateJSON.String), &ps); err != nil {
		return false, err
	}
	s.applyPersistedState(ps)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) saveLocked() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin sqlite tx: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range []string{
		"DELETE FROM job_order;",
		"DELETE FROM jobs;",
		"DELETE FROM results;",
		"DELETE FROM agents;",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("reset tables: %w", err)
		}
	}

	insertOrder, err := tx.Prepare("INSERT INTO job_order(position,job_id) VALUES(?,?);")
	if err != nil {
		return fmt.Errorf("prepare job_order insert: %w", err)
	}
	defer insertOrder.Close()
	for i, id := range s.order {
		if _, err := insertOrder.Exec(i+1, id); err != nil {
			return fmt.Errorf("insert job_order row: %w", err)
		}
	}

	insertJob, err := tx.Prepare(`INSERT INTO jobs(
id,parent_job_id,start_ip,end_ip,start_port,end_port,ports_json,port_count,status,assigned_to,attempts,max_attempts,lease_expires_at,last_error,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?);`)
	if err != nil {
		return fmt.Errorf("prepare jobs insert: %w", err)
	}
	defer insertJob.Close()
	for _, job := range s.jobs {
		var parentID any
		if job.ParentJobID != "" {
			parentID = job.ParentJobID
		}
		var portsJSON any
		if len(job.Ports) > 0 {
			encoded, _ := json.Marshal(job.Ports)
			portsJSON = string(encoded)
		}
		var assignedTo any
		if job.AssignedTo != "" {
			assignedTo = job.AssignedTo
		}
		var leaseExpiresAt any
		if job.LeaseExpiresAt != nil {
			leaseExpiresAt = job.LeaseExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		var lastError any
		if job.LastError != "" {
			lastError = job.LastError
		}
		if _, err := insertJob.Exec(
			job.ID,
			parentID,
			job.StartIP,
			job.EndIP,
			job.StartPort,
			job.EndPort,
			portsJSON,
			job.PortCount,
			string(job.Status),
			assignedTo,
			job.Attempts,
			job.MaxAttempts,
			leaseExpiresAt,
			lastError,
			job.CreatedAt.UTC().Format(time.RFC3339Nano),
			job.UpdatedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert jobs row: %w", err)
		}
	}

	insertResult, err := tx.Prepare("INSERT INTO results(job_id,ip,port,open,err) VALUES(?,?,?,?,?);")
	if err != nil {
		return fmt.Errorf("prepare results insert: %w", err)
	}
	defer insertResult.Close()
	for jobID, list := range s.results {
		for _, r := range list {
			openVal := 0
			if r.Open {
				openVal = 1
			}
			var errMsg any
			if r.Err != "" {
				errMsg = r.Err
			}
			if _, err := insertResult.Exec(jobID, r.IP, r.Port, openVal, errMsg); err != nil {
				return fmt.Errorf("insert results row: %w", err)
			}
		}
	}

	insertAgent, err := tx.Prepare("INSERT INTO agents(agent_id,last_seen,current_job_id,allow_restricted_nets,completed_jobs,failed_jobs) VALUES(?,?,?,?,?,?);")
	if err != nil {
		return fmt.Errorf("prepare agents insert: %w", err)
	}
	defer insertAgent.Close()
	for id, a := range s.agents {
		allowRestricted := 0
		if a.AllowRestrictedNets {
			allowRestricted = 1
		}
		var currentJobID any
		if a.CurrentJobID != "" {
			currentJobID = a.CurrentJobID
		}
		if _, err := insertAgent.Exec(
			id,
			a.LastSeen.UTC().Format(time.RFC3339Nano),
			currentJobID,
			allowRestricted,
			a.CompletedJobs,
			a.FailedJobs,
		); err != nil {
			return fmt.Errorf("insert agents row: %w", err)
		}
	}

	if _, err := tx.Exec("INSERT INTO meta(key,value) VALUES('next_id',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value;", strconv.Itoa(s.nextID)); err != nil {
		return fmt.Errorf("upsert meta next_id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite tx: %w", err)
	}
	return nil
}

func (s *Store) applyPersistedState(ps persistedState) {
	if ps.Jobs != nil {
		s.jobs = ps.Jobs
	}
	if ps.Order != nil {
		s.order = ps.Order
	}
	if ps.Results != nil {
		s.results = ps.Results
	}
	if ps.Agents != nil {
		s.agents = ps.Agents
	}
	if ps.NextID > 0 {
		s.nextID = ps.NextID
	}
}

func (s *Store) initSQLite() error {
	dir := filepath.Dir(s.dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create sqlite directory: %w", err)
	}
	const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  parent_job_id TEXT,
  start_ip TEXT NOT NULL,
  end_ip TEXT NOT NULL,
  start_port INTEGER NOT NULL,
  end_port INTEGER NOT NULL,
  ports_json TEXT,
  port_count INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  assigned_to TEXT,
  attempts INTEGER NOT NULL,
  max_attempts INTEGER NOT NULL,
  lease_expires_at TEXT,
  last_error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_order (
  position INTEGER PRIMARY KEY,
  job_id TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS results (
  job_id TEXT NOT NULL,
  ip TEXT NOT NULL,
  port INTEGER NOT NULL,
  open INTEGER NOT NULL,
  err TEXT,
  PRIMARY KEY(job_id, ip, port)
);
CREATE TABLE IF NOT EXISTS agents (
  agent_id TEXT PRIMARY KEY,
  last_seen TEXT NOT NULL,
  current_job_id TEXT,
  allow_restricted_nets INTEGER NOT NULL DEFAULT 0,
  completed_jobs INTEGER NOT NULL,
  failed_jobs INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_parent ON jobs(parent_job_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_results_job ON results(job_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	_, _ = s.db.Exec("ALTER TABLE jobs ADD COLUMN ports_json TEXT;")
	_, _ = s.db.Exec("ALTER TABLE jobs ADD COLUMN port_count INTEGER NOT NULL DEFAULT 0;")
	_, _ = s.db.Exec("ALTER TABLE agents ADD COLUMN allow_restricted_nets INTEGER NOT NULL DEFAULT 0;")
	return nil
}

func (s *Store) tableExists(name string) (bool, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?;", name).Scan(&n); err != nil {
		return false, fmt.Errorf("check table %q exists: %w", name, err)
	}
	return n > 0, nil
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", v, err)
	}
	return t, nil
}

func inferNextID(jobs map[string]protocol.Job) int {
	maxN := 0
	for id := range jobs {
		if !strings.HasPrefix(id, "job-") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(id, "job-"))
		if err == nil && n > maxN {
			maxN = n
		}
	}
	if maxN == 0 {
		return 1
	}
	return maxN + 1
}

func (s *Store) childIndexLocked() map[string][]string {
	idx := make(map[string][]string)
	for _, job := range s.jobs {
		if job.ParentJobID == "" {
			continue
		}
		idx[job.ParentJobID] = append(idx[job.ParentJobID], job.ID)
	}
	return idx
}

func (s *Store) summarizeParentLocked(parent protocol.Job, childIDs []string) protocol.JobListItem {
	item := protocol.JobListItem{
		ID:          parent.ID,
		StartIP:     parent.StartIP,
		EndIP:       parent.EndIP,
		StartPort:   parent.StartPort,
		EndPort:     parent.EndPort,
		PortCount:   parent.PortCount,
		MaxAttempts: parent.MaxAttempts,
		CreatedAt:   parent.CreatedAt,
		UpdatedAt:   parent.UpdatedAt,
	}

	for _, childID := range childIDs {
		child, ok := s.jobs[childID]
		if !ok {
			continue
		}
		if child.UpdatedAt.After(item.UpdatedAt) {
			item.UpdatedAt = child.UpdatedAt
		}
		if child.LastError != "" {
			item.LastError = child.LastError
		}
		item.OpenPortCount += len(s.results[childID])
		switch child.Status {
		case protocol.JobQueued:
			item.SubJobsPending++
		case protocol.JobInProgress:
			item.SubJobsActive++
		case protocol.JobCompleted:
			item.SubJobsCompleted++
		case protocol.JobFailed:
			item.SubJobsFailed++
		}
	}
	item.SubJobsTotal = item.SubJobsPending + item.SubJobsActive + item.SubJobsCompleted + item.SubJobsFailed
	item.Status = statusFromCounts(item.SubJobsPending, item.SubJobsActive, item.SubJobsCompleted, item.SubJobsFailed)
	return item
}

func summarizeStandaloneJob(job protocol.Job, results []protocol.ScanResult) protocol.JobListItem {
	item := protocol.JobListItem{
		ID:               job.ID,
		StartIP:          job.StartIP,
		EndIP:            job.EndIP,
		StartPort:        job.StartPort,
		EndPort:          job.EndPort,
		PortCount:        job.PortCount,
		Status:           job.Status,
		MaxAttempts:      job.MaxAttempts,
		OpenPortCount:    len(results),
		CreatedAt:        job.CreatedAt,
		UpdatedAt:        job.UpdatedAt,
		LastError:        job.LastError,
		SubJobsTotal:     1,
		SubJobsPending:   btoi(job.Status == protocol.JobQueued),
		SubJobsActive:    btoi(job.Status == protocol.JobInProgress),
		SubJobsCompleted: btoi(job.Status == protocol.JobCompleted),
		SubJobsFailed:    btoi(job.Status == protocol.JobFailed),
	}
	return item
}

func (s *Store) aggregateParentDetailsLocked(parent protocol.Job, childIDs []string) (protocol.Job, []protocol.ScanResult) {
	summary := s.summarizeParentLocked(parent, childIDs)
	aggJob := protocol.Job{
		ID:          parent.ID,
		StartIP:     parent.StartIP,
		EndIP:       parent.EndIP,
		StartPort:   parent.StartPort,
		EndPort:     parent.EndPort,
		Ports:       append([]int(nil), parent.Ports...),
		PortCount:   parent.PortCount,
		Status:      summary.Status,
		MaxAttempts: parent.MaxAttempts,
		CreatedAt:   summary.CreatedAt,
		UpdatedAt:   summary.UpdatedAt,
		LastError:   summary.LastError,
	}
	results := make([]protocol.ScanResult, 0, summary.OpenPortCount)
	for _, childID := range childIDs {
		results = append(results, s.results[childID]...)
	}
	sortResultsByIPThenPort(results)
	return aggJob, results
}

func statusFromCounts(pending, active, completed, failed int) protocol.JobStatus {
	total := pending + active + completed + failed
	if total == 0 {
		return protocol.JobQueued
	}
	if active > 0 {
		return protocol.JobInProgress
	}
	if pending > 0 {
		return protocol.JobQueued
	}
	if failed > 0 {
		return protocol.JobFailed
	}
	if completed == total {
		return protocol.JobCompleted
	}
	return protocol.JobQueued
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func retainOpenResults(results []protocol.ScanResult) []protocol.ScanResult {
	out := make([]protocol.ScanResult, 0, len(results))
	for _, r := range results {
		if r.Open {
			out = append(out, r)
		}
	}
	return out
}

func sortResultsByIPThenPort(results []protocol.ScanResult) {
	sort.Slice(results, func(i, j int) bool {
		ai, errA := netip.ParseAddr(results[i].IP)
		aj, errB := netip.ParseAddr(results[j].IP)
		if errA == nil && errB == nil {
			if ai.Compare(aj) != 0 {
				return ai.Compare(aj) < 0
			}
		} else if results[i].IP != results[j].IP {
			return results[i].IP < results[j].IP
		}
		return results[i].Port < results[j].Port
	})
}

type portRange struct {
	start int
	end   int
}

func splitPortRanges(startPort, endPort, chunkSize int) []portRange {
	if chunkSize <= 0 {
		chunkSize = 1024
	}
	out := make([]portRange, 0, ((endPort-startPort)/chunkSize)+1)
	for p := startPort; p <= endPort; p += chunkSize {
		end := p + chunkSize - 1
		if end > endPort {
			end = endPort
		}
		out = append(out, portRange{start: p, end: end})
	}
	return out
}

func enumerateIPv4Range(start, end string) ([]string, error) {
	startAddr, err := netip.ParseAddr(start)
	if err != nil {
		return nil, fmt.Errorf("invalid start IP: %w", err)
	}
	endAddr, err := netip.ParseAddr(end)
	if err != nil {
		return nil, fmt.Errorf("invalid end IP: %w", err)
	}
	if !startAddr.Is4() || !endAddr.Is4() {
		return nil, fmt.Errorf("only IPv4 ranges are supported")
	}
	startN := ipv4ToUint32(startAddr)
	endN := ipv4ToUint32(endAddr)
	if startN > endN {
		return nil, fmt.Errorf("start IP must be <= end IP")
	}
	out := make([]string, 0, int(endN-startN)+1)
	for i := startN; i <= endN; i++ {
		out = append(out, uint32ToIPv4(i).String())
	}
	return out, nil
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	b := addr.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToIPv4(i uint32) netip.Addr {
	b := [4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
	return netip.AddrFrom4(b)
}

func splitPortList(ports []int, chunkSize int) [][]int {
	if chunkSize <= 0 {
		chunkSize = 1024
	}
	out := make([][]int, 0, (len(ports)/chunkSize)+1)
	for i := 0; i < len(ports); i += chunkSize {
		end := i + chunkSize
		if end > len(ports) {
			end = len(ports)
		}
		chunk := make([]int, end-i)
		copy(chunk, ports[i:end])
		out = append(out, chunk)
	}
	return out
}

func minMaxPorts(ports []int) (int, int) {
	if len(ports) == 0 {
		return 0, 0
	}
	minV, maxV := ports[0], ports[0]
	for _, p := range ports[1:] {
		if p < minV {
			minV = p
		}
		if p > maxV {
			maxV = p
		}
	}
	return minV, maxV
}

func rangeToPorts(start, end int) []int {
	if end < start {
		return nil
	}
	out := make([]int, 0, end-start+1)
	for p := start; p <= end; p++ {
		out = append(out, p)
	}
	return out
}

func normalizeRequestedPorts(req protocol.SubmitJobRequest) []int {
	if req.TopN > 0 {
		return getTopNPorts(req.TopN)
	}
	if req.Top1000 {
		return getTop1000Ports()
	}
	if len(req.Ports) > 0 {
		seen := make(map[int]struct{}, len(req.Ports))
		out := make([]int, 0, len(req.Ports))
		for _, p := range req.Ports {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
		sort.Ints(out)
		return out
	}
	return nil
}

func portCountForJob(start, end int, ports []int) int {
	if len(ports) > 0 {
		return len(ports)
	}
	if end >= start && start > 0 {
		return end - start + 1
	}
	return 0
}

func jobTargetsRestrictedNets(job protocol.Job) bool {
	startAddr, err := netip.ParseAddr(job.StartIP)
	if err != nil || !startAddr.Is4() {
		return false
	}
	endAddr, err := netip.ParseAddr(job.EndIP)
	if err != nil || !endAddr.Is4() {
		return false
	}
	startNum := ipv4ToUint32(startAddr)
	endNum := ipv4ToUint32(endAddr)
	if startNum > endNum {
		startNum, endNum = endNum, startNum
	}
	for _, p := range restrictedPrefixes {
		if !p.Addr().Is4() {
			continue
		}
		pStart, pEnd := prefixRangeUint32(p)
		if startNum <= pEnd && endNum >= pStart {
			return true
		}
	}
	return false
}

func prefixRangeUint32(p netip.Prefix) (uint32, uint32) {
	base := ipv4ToUint32(p.Masked().Addr())
	bits := p.Bits()
	if bits <= 0 {
		return 0, ^uint32(0)
	}
	hostBits := 32 - bits
	if hostBits <= 0 {
		return base, base
	}
	mask := ^uint32(0) >> bits
	return base, base + mask
}
