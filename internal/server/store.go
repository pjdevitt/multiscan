package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	LastSeen      time.Time `json:"last_seen"`
	CurrentJobID  string    `json:"current_job_id,omitempty"`
	CompletedJobs int       `json:"completed_jobs"`
	FailedJobs    int       `json:"failed_jobs"`
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
	dbPath        string
	legacyState   string
	leaseDuration time.Duration
}

const portBatchSize = 1024

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
	if err := s.initSQLite(); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
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
	subJobs := make([]protocol.Job, 0, len(chunks))
	for _, chunk := range chunks {
		id := fmt.Sprintf("job-%04d", s.nextID)
		s.nextID++
		startPort, endPort := minMaxPorts(chunk)
		job := protocol.Job{
			ID:          id,
			ParentJobID: parentID,
			StartIP:     req.StartIP,
			EndIP:       req.EndIP,
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

func (s *Store) TouchAgent(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchAgentLocked(agentID, "")
}

func (s *Store) Heartbeat(agentID, currentJobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchAgentLocked(agentID, currentJobID)
}

func (s *Store) WaitAndAssign(agentID string, maxWait time.Duration) (protocol.Job, error) {
	if maxWait <= 0 {
		maxWait = 25 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	for {
		s.mu.Lock()
		s.touchAgentLocked(agentID, "")
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
			AgentID:       id,
			State:         state,
			LastSeen:      rec.LastSeen,
			CurrentJobID:  rec.CurrentJobID,
			CompletedJobs: rec.CompletedJobs,
			FailedJobs:    rec.FailedJobs,
		})
	}
	return out
}

func (s *Store) assignNextLocked(agentID string) (protocol.Job, error) {
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

func (s *Store) touchAgentLocked(agentID, currentJobID string) {
	rec := s.agents[agentID]
	rec.LastSeen = time.Now().UTC()
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
	type countRow struct {
		N int `json:"n"`
	}
	countRows, err := queryRows[countRow](s, "SELECT COUNT(*) AS n FROM jobs;")
	if err != nil {
		return false, err
	}
	if len(countRows) == 0 || countRows[0].N == 0 {
		return false, nil
	}

	type jobRow struct {
		ID             string  `json:"id"`
		ParentJobID    *string `json:"parent_job_id"`
		StartIP        string  `json:"start_ip"`
		EndIP          string  `json:"end_ip"`
		StartPort      int     `json:"start_port"`
		EndPort        int     `json:"end_port"`
		PortsJSON      *string `json:"ports_json"`
		PortCount      int     `json:"port_count"`
		Status         string  `json:"status"`
		AssignedTo     *string `json:"assigned_to"`
		Attempts       int     `json:"attempts"`
		MaxAttempts    int     `json:"max_attempts"`
		LeaseExpiresAt *string `json:"lease_expires_at"`
		LastError      *string `json:"last_error"`
		CreatedAt      string  `json:"created_at"`
		UpdatedAt      string  `json:"updated_at"`
	}
	jobRows, err := queryRows[jobRow](s, "SELECT id,parent_job_id,start_ip,end_ip,start_port,end_port,ports_json,port_count,status,assigned_to,attempts,max_attempts,lease_expires_at,last_error,created_at,updated_at FROM jobs;")
	if err != nil {
		return false, err
	}
	for _, r := range jobRows {
		createdAt, err := parseTime(r.CreatedAt)
		if err != nil {
			return false, err
		}
		updatedAt, err := parseTime(r.UpdatedAt)
		if err != nil {
			return false, err
		}
		var lease *time.Time
		if r.LeaseExpiresAt != nil && *r.LeaseExpiresAt != "" {
			t, err := parseTime(*r.LeaseExpiresAt)
			if err != nil {
				return false, err
			}
			lease = &t
		}
		job := protocol.Job{
			ID:             r.ID,
			StartIP:        r.StartIP,
			EndIP:          r.EndIP,
			StartPort:      r.StartPort,
			EndPort:        r.EndPort,
			PortCount:      r.PortCount,
			Status:         protocol.JobStatus(r.Status),
			Attempts:       r.Attempts,
			MaxAttempts:    r.MaxAttempts,
			LeaseExpiresAt: lease,
			CreatedAt:      createdAt,
			UpdatedAt:      updatedAt,
		}
		if r.PortsJSON != nil && strings.TrimSpace(*r.PortsJSON) != "" {
			var ports []int
			if err := json.Unmarshal([]byte(*r.PortsJSON), &ports); err == nil {
				job.Ports = ports
			}
		}
		if job.PortCount == 0 {
			job.PortCount = portCountForJob(job.StartPort, job.EndPort, job.Ports)
		}
		if r.ParentJobID != nil {
			job.ParentJobID = *r.ParentJobID
		}
		if r.AssignedTo != nil {
			job.AssignedTo = *r.AssignedTo
		}
		if r.LastError != nil {
			job.LastError = *r.LastError
		}
		s.jobs[job.ID] = job
	}

	type orderRow struct {
		JobID string `json:"job_id"`
	}
	orderRows, err := queryRows[orderRow](s, "SELECT job_id FROM job_order ORDER BY position ASC;")
	if err != nil {
		return false, err
	}
	for _, r := range orderRows {
		s.order = append(s.order, r.JobID)
	}
	if len(s.order) == 0 {
		for id := range s.jobs {
			s.order = append(s.order, id)
		}
		sort.Strings(s.order)
	}

	type resultRow struct {
		JobID string  `json:"job_id"`
		IP    string  `json:"ip"`
		Port  int     `json:"port"`
		Open  int     `json:"open"`
		Err   *string `json:"err"`
	}
	resultRows, err := queryRows[resultRow](s, "SELECT job_id,ip,port,open,err FROM results ORDER BY job_id,ip,port;")
	if err != nil {
		return false, err
	}
	for _, r := range resultRows {
		sr := protocol.ScanResult{IP: r.IP, Port: r.Port, Open: r.Open != 0}
		if r.Err != nil {
			sr.Err = *r.Err
		}
		s.results[r.JobID] = append(s.results[r.JobID], sr)
	}

	type agentRow struct {
		AgentID       string  `json:"agent_id"`
		LastSeen      string  `json:"last_seen"`
		CurrentJobID  *string `json:"current_job_id"`
		CompletedJobs int     `json:"completed_jobs"`
		FailedJobs    int     `json:"failed_jobs"`
	}
	agentRows, err := queryRows[agentRow](s, "SELECT agent_id,last_seen,current_job_id,completed_jobs,failed_jobs FROM agents;")
	if err != nil {
		return false, err
	}
	for _, r := range agentRows {
		ls, err := parseTime(r.LastSeen)
		if err != nil {
			return false, err
		}
		ag := agentRecord{LastSeen: ls, CompletedJobs: r.CompletedJobs, FailedJobs: r.FailedJobs}
		if r.CurrentJobID != nil {
			ag.CurrentJobID = *r.CurrentJobID
		}
		s.agents[r.AgentID] = ag
	}

	type metaRow struct {
		Value string `json:"value"`
	}
	metaRows, err := queryRows[metaRow](s, "SELECT value FROM meta WHERE key='next_id';")
	if err != nil {
		return false, err
	}
	if len(metaRows) > 0 {
		if n, err := strconv.Atoi(metaRows[0].Value); err == nil && n > 0 {
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
	type blobRow struct {
		StateJSON string `json:"state_json"`
	}
	rows, err := queryRows[blobRow](s, "SELECT state_json FROM multiscan_state WHERE id=1;")
	if err != nil {
		return false, err
	}
	if len(rows) == 0 || strings.TrimSpace(rows[0].StateJSON) == "" {
		return false, nil
	}
	var ps persistedState
	if err := json.Unmarshal([]byte(rows[0].StateJSON), &ps); err != nil {
		return false, err
	}
	s.applyPersistedState(ps)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) saveLocked() error {
	var b strings.Builder
	b.WriteString("BEGIN IMMEDIATE;\n")
	b.WriteString("DELETE FROM job_order;\n")
	b.WriteString("DELETE FROM jobs;\n")
	b.WriteString("DELETE FROM results;\n")
	b.WriteString("DELETE FROM agents;\n")

	for i, id := range s.order {
		b.WriteString("INSERT INTO job_order(position,job_id) VALUES(")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(",'" + escapeSQLString(id) + "');\n")
	}

	for _, job := range s.jobs {
		b.WriteString("INSERT INTO jobs(id,parent_job_id,start_ip,end_ip,start_port,end_port,ports_json,port_count,status,assigned_to,attempts,max_attempts,lease_expires_at,last_error,created_at,updated_at) VALUES(")
		b.WriteString("'" + escapeSQLString(job.ID) + "',")
		if job.ParentJobID != "" {
			b.WriteString("'" + escapeSQLString(job.ParentJobID) + "',")
		} else {
			b.WriteString("NULL,")
		}
		b.WriteString("'" + escapeSQLString(job.StartIP) + "',")
		b.WriteString("'" + escapeSQLString(job.EndIP) + "',")
		b.WriteString(strconv.Itoa(job.StartPort) + ",")
		b.WriteString(strconv.Itoa(job.EndPort) + ",")
		if len(job.Ports) > 0 {
			portsJSON, _ := json.Marshal(job.Ports)
			b.WriteString("'" + escapeSQLString(string(portsJSON)) + "',")
		} else {
			b.WriteString("NULL,")
		}
		b.WriteString(strconv.Itoa(job.PortCount) + ",")
		b.WriteString("'" + escapeSQLString(string(job.Status)) + "',")
		if job.AssignedTo != "" {
			b.WriteString("'" + escapeSQLString(job.AssignedTo) + "',")
		} else {
			b.WriteString("NULL,")
		}
		b.WriteString(strconv.Itoa(job.Attempts) + ",")
		b.WriteString(strconv.Itoa(job.MaxAttempts) + ",")
		if job.LeaseExpiresAt != nil {
			b.WriteString("'" + escapeSQLString(job.LeaseExpiresAt.UTC().Format(time.RFC3339Nano)) + "',")
		} else {
			b.WriteString("NULL,")
		}
		if job.LastError != "" {
			b.WriteString("'" + escapeSQLString(job.LastError) + "',")
		} else {
			b.WriteString("NULL,")
		}
		b.WriteString("'" + job.CreatedAt.UTC().Format(time.RFC3339Nano) + "',")
		b.WriteString("'" + job.UpdatedAt.UTC().Format(time.RFC3339Nano) + "');\n")
	}

	for jobID, list := range s.results {
		for _, r := range list {
			b.WriteString("INSERT INTO results(job_id,ip,port,open,err) VALUES(")
			b.WriteString("'" + escapeSQLString(jobID) + "',")
			b.WriteString("'" + escapeSQLString(r.IP) + "',")
			b.WriteString(strconv.Itoa(r.Port) + ",")
			if r.Open {
				b.WriteString("1,")
			} else {
				b.WriteString("0,")
			}
			if r.Err != "" {
				b.WriteString("'" + escapeSQLString(r.Err) + "');\n")
			} else {
				b.WriteString("NULL);\n")
			}
		}
	}

	for id, a := range s.agents {
		b.WriteString("INSERT INTO agents(agent_id,last_seen,current_job_id,completed_jobs,failed_jobs) VALUES(")
		b.WriteString("'" + escapeSQLString(id) + "',")
		b.WriteString("'" + a.LastSeen.UTC().Format(time.RFC3339Nano) + "',")
		if a.CurrentJobID != "" {
			b.WriteString("'" + escapeSQLString(a.CurrentJobID) + "',")
		} else {
			b.WriteString("NULL,")
		}
		b.WriteString(strconv.Itoa(a.CompletedJobs) + ",")
		b.WriteString(strconv.Itoa(a.FailedJobs) + ");\n")
	}

	b.WriteString("INSERT INTO meta(key,value) VALUES('next_id','" + strconv.Itoa(s.nextID) + "') ON CONFLICT(key) DO UPDATE SET value=excluded.value;\n")
	b.WriteString("COMMIT;\n")

	if err := s.execSQLite(b.String()); err != nil {
		_ = s.execSQLite("ROLLBACK;")
		return err
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
	if err := s.execSQLite(schema); err != nil {
		return err
	}
	_ = s.execSQLite("ALTER TABLE jobs ADD COLUMN ports_json TEXT;")
	_ = s.execSQLite("ALTER TABLE jobs ADD COLUMN port_count INTEGER NOT NULL DEFAULT 0;")
	return nil
}

func (s *Store) execSQLite(sql string) error {
	cmd := exec.Command("sqlite3", s.dbPath, sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("sqlite3 exec failed: %s", msg)
	}
	return nil
}

func (s *Store) querySQLiteJSON(sql string) ([]json.RawMessage, error) {
	cmd := exec.Command("sqlite3", "-json", s.dbPath, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("sqlite3 query failed: %s", msg)
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode sqlite JSON output: %w", err)
	}
	return rows, nil
}

func (s *Store) tableExists(name string) (bool, error) {
	type row struct {
		N int `json:"n"`
	}
	rows, err := queryRows[row](s, "SELECT COUNT(*) AS n FROM sqlite_master WHERE type='table' AND name='"+escapeSQLString(name)+"';")
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].N > 0, nil
}

func queryRows[T any](s *Store, sql string) ([]T, error) {
	raw, err := s.querySQLiteJSON(sql)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(raw))
	for _, r := range raw {
		var row T
		if err := json.Unmarshal(r, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
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

func escapeSQLString(v string) string {
	return strings.ReplaceAll(v, "'", "''")
}
