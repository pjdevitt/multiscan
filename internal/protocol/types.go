package protocol

import "time"

// JobStatus represents the lifecycle state of a scan job.
type JobStatus string

const (
	JobQueued     JobStatus = "queued"
	JobInProgress JobStatus = "in_progress"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
	JobStopped    JobStatus = "stopped"
)

// SubmitJobRequest is sent to the controller to enqueue scan work.
type SubmitJobRequest struct {
	Hostname    string `json:"hostname,omitempty"`
	StartIP     string `json:"start_ip"`
	EndIP       string `json:"end_ip"`
	StartPort   int    `json:"start_port"`
	EndPort     int    `json:"end_port"`
	Ports       []int  `json:"ports,omitempty"`
	Top1000     bool   `json:"top_1000,omitempty"`
	TopN        int    `json:"top_n,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
}

// SubmitJobResponse is returned after creating a job.
type SubmitJobResponse struct {
	JobID    string   `json:"job_id"`
	JobIDs   []string `json:"job_ids,omitempty"`
	JobCount int      `json:"job_count"`
}

// Job defines a package of IP/port range scan work.
type Job struct {
	ID             string     `json:"id"`
	ParentJobID    string     `json:"parent_job_id,omitempty"`
	StartIP        string     `json:"start_ip"`
	EndIP          string     `json:"end_ip"`
	StartPort      int        `json:"start_port"`
	EndPort        int        `json:"end_port"`
	Ports          []int      `json:"ports,omitempty"`
	PortCount      int        `json:"port_count,omitempty"`
	Status         JobStatus  `json:"status"`
	AssignedTo     string     `json:"assigned_to,omitempty"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ScanResult represents one scanned endpoint.
type ScanResult struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Open bool   `json:"open"`
	Err  string `json:"err,omitempty"`
}

// JobCompletion carries completion or failure metadata for a prior assignment.
type JobCompletion struct {
	JobID   string       `json:"job_id"`
	Results []ScanResult `json:"results,omitempty"`
	Error   string       `json:"error,omitempty"`
}

// SyncRequest is a synchronous bidirectional work exchange payload.
// Agent may include Completion for the previous job and request next work.
type SyncRequest struct {
	AgentID             string         `json:"agent_id"`
	AllowRestrictedNets bool           `json:"allow_restricted_nets,omitempty"`
	Completion          *JobCompletion `json:"completion,omitempty"`
}

// SyncAction describes server instruction after processing sync.
type SyncAction string

const (
	SyncActionWork SyncAction = "work"
	SyncActionWait SyncAction = "wait"
)

// SyncResponse contains either new work or wait instruction.
type SyncResponse struct {
	Action         SyncAction `json:"action"`
	Job            *Job       `json:"job,omitempty"`
	WaitSeconds    int        `json:"wait_seconds,omitempty"`
	Message        string     `json:"message,omitempty"`
	StopCurrentJob bool       `json:"stop_current_job,omitempty"`
	StopReason     string     `json:"stop_reason,omitempty"`
}

// JobDetails returns state plus any collected results.
type JobDetails struct {
	Job     Job          `json:"job"`
	Results []ScanResult `json:"results,omitempty"`
}

// JobListItem is the summary entry shown in the main job list.
type JobListItem struct {
	ID               string    `json:"id"`
	StartIP          string    `json:"start_ip"`
	EndIP            string    `json:"end_ip"`
	StartPort        int       `json:"start_port"`
	EndPort          int       `json:"end_port"`
	PortCount        int       `json:"port_count,omitempty"`
	Status           JobStatus `json:"status"`
	MaxAttempts      int       `json:"max_attempts"`
	OpenPortCount    int       `json:"open_port_count"`
	SubJobsPending   int       `json:"sub_jobs_pending"`
	SubJobsActive    int       `json:"sub_jobs_active"`
	SubJobsCompleted int       `json:"sub_jobs_completed"`
	SubJobsFailed    int       `json:"sub_jobs_failed"`
	SubJobsStopped   int       `json:"sub_jobs_stopped"`
	SubJobsTotal     int       `json:"sub_jobs_total"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	LastError        string    `json:"last_error,omitempty"`
}

// AgentStatus describes coordinator view of an agent heartbeat and workload.
type AgentStatus struct {
	AgentID             string    `json:"agent_id"`
	State               string    `json:"state"`
	LastSeen            time.Time `json:"last_seen"`
	CurrentJobID        string    `json:"current_job_id,omitempty"`
	AllowRestrictedNets bool      `json:"allow_restricted_nets"`
	CompletedJobs       int       `json:"completed_jobs"`
	FailedJobs          int       `json:"failed_jobs"`
}

// HeartbeatRequest updates coordinator liveness for an agent.
type HeartbeatRequest struct {
	AgentID             string `json:"agent_id"`
	CurrentJobID        string `json:"current_job_id,omitempty"`
	AllowRestrictedNets bool   `json:"allow_restricted_nets,omitempty"`
}

// HeartbeatResponse allows server-side control instructions for active agents.
type HeartbeatResponse struct {
	Status  string `json:"status"`
	StopJob bool   `json:"stop_job,omitempty"`
	Reason  string `json:"reason,omitempty"`
}
