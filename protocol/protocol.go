// Package protocol defines the messages that agents and gateways exchange with
// the Elprovo control plane over one WebSocket session. The JSON encoding is a
// compatibility contract: the control plane serves agents and gateways that
// customers have not upgraded.
package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the protocol version a client announces when it authenticates.
// The control plane accepts a range of versions that ends at this one.
const Version = 1

// SignaturePrefix is prepended to the server nonce before the client signs it.
const SignaturePrefix = "elprovo-session-v1:"

// HandoffPath is where a gateway redeems a handoff. The control plane puts the
// handoff secret in the HandoffTokenParam query parameter.
const (
	HandoffPath       = "/_elprovo/handoff"
	HandoffTokenParam = "token"
)

// Role says which kind of client holds a session.
type Role string

// Client roles.
const (
	RoleAgent   Role = "agent"
	RoleGateway Role = "gateway"
)

// Message types. The comment on each names the direction.
const (
	TypeChallenge = "challenge" // server to client
	TypeAuth      = "auth"      // client to server
	TypeWelcome   = "welcome"   // server to client
	TypeError     = "error"     // server to client, before it closes the session

	TypeJob        = "job"         // server to agent
	TypeLeaseRenew = "lease_renew" // agent to server
	TypeJobResult  = "job_result"  // agent to server

	TypeJobStep        = "job_step"         // agent to server
	TypeJobLog         = "job_log"          // agent to server
	TypeJobsInProgress = "jobs_in_progress" // agent to server
	TypeJobAdopted     = "job_adopted"      // server to agent
	TypeJobAbandoned   = "job_abandoned"    // server to agent
	TypeInventory      = "inventory"        // agent to server

	TypeRoutes           = "routes"            // server to gateway
	TypeRoutesApplied    = "routes_applied"    // gateway to server
	TypeHandoffExchange  = "handoff_exchange"  // gateway to server
	TypeHandoffResult    = "handoff_result"    // server to gateway
	TypeSessionConfirm   = "session_confirm"   // gateway to server
	TypeSessionConfirmed = "session_confirmed" // server to gateway
	TypeRouteHealth      = "route_health"      // gateway to server
)

// Envelope is one message on the wire.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Encode wraps payload in an envelope of the given type.
func Encode(msgType string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", msgType, err)
	}
	return json.Marshal(Envelope{Type: msgType, Payload: raw})
}

// Decode reads an envelope.
func Decode(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("decode envelope: missing type")
	}
	return env, nil
}

// Into decodes the envelope payload into v.
func (e Envelope) Into(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	return nil
}

// Challenge carries the nonce the client must sign.
type Challenge struct {
	Nonce []byte `json:"nonce"`
}

// Auth proves the client holds the private key for PublicKey. A new client
// also sends a one-time enrollment token.
type Auth struct {
	Role            Role   `json:"role"`
	ProtocolVersion int    `json:"protocol_version"`
	PublicKey       []byte `json:"public_key"`
	Signature       []byte `json:"signature"`
	EnrollmentToken string `json:"enrollment_token,omitempty"`
}

// Welcome confirms the session.
type Welcome struct {
	ClientID      string `json:"client_id"`
	EnvironmentID string `json:"environment_id"`
}

// Error explains why the server ends a session.
type Error struct {
	Message string `json:"message"`
}

// JobKind names what a job does.
type JobKind string

// Job kinds.
const (
	JobDeploy        JobKind = "deploy"
	JobDelete        JobKind = "delete"
	JobReset         JobKind = "reset"
	JobDeleteMachine JobKind = "delete_machine"
)

// Job is work leased to an agent. The agent reports its result under
// LeaseToken and renews the lease while it works.
type Job struct {
	ID         string      `json:"id"`
	LeaseToken string      `json:"lease_token"`
	Kind       JobKind     `json:"kind"`
	PreviewID  string      `json:"preview_id,omitempty"`
	Deploy     *DeploySpec `json:"deploy,omitempty"`
	Reset      *ResetSpec  `json:"reset,omitempty"`
	// Machine names the machine a DeleteMachine job removes.
	Machine string `json:"machine,omitempty"`
}

// DeploySpec is everything an agent needs to build and run one commit.
type DeploySpec struct {
	Repository  string `json:"repository"`
	Commit      string `json:"commit"`
	SourceToken string `json:"source_token"`
	// SourceAPIURL is the GitHub API base URL that SourceToken works with.
	SourceAPIURL  string   `json:"source_api_url"`
	ComposeFiles  []string `json:"compose_files"`
	PublicService string   `json:"public_service"`
	PublicPort    int      `json:"public_port"`
	HealthPath    string   `json:"health_path"`
	// MigrationCommand runs in the public service after the project starts
	// and before the health check.
	MigrationCommand string `json:"migration_command,omitempty"`
	// Config holds plain environment values for every service.
	Config map[string]string `json:"config,omitempty"`
	// Secrets name values the agent resolves from its own store.
	Secrets []SecretRef `json:"secrets,omitempty"`
}

// SecretRef asks the agent to set environment variable Name to the value it
// stores under Key. The control plane never sees the value.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ResetSpec reapplies the bundle of the preview's last applied deploy on
// its machine, without fetching source or running the migration again.
type ResetSpec struct {
	Bundle        json.RawMessage `json:"bundle"`
	PublicService string          `json:"public_service"`
	PublicPort    int             `json:"public_port"`
	HealthPath    string          `json:"health_path"`
	Secrets       []SecretRef     `json:"secrets,omitempty"`
}

// LeaseRenew extends a job lease. The server answers a renewal it rejects
// with JobAbandoned.
type LeaseRenew struct {
	JobID      string `json:"job_id"`
	LeaseToken string `json:"lease_token"`
}

// JobResult reports how a job ended. Upstream is the address the gateway
// proxies to after a successful deploy or reset. Images maps each built
// Compose service to the image it runs, by digest. Bundle is the compiled
// Compose project a successful deploy applied, with every image named by
// digest; it holds plain values and secret names, never secret values.
// MachineChanged says that a deploy or reset started to apply its bundle
// on the preview's machine; when such a job fails, the machine may no
// longer run the app it ran before.
type JobResult struct {
	JobID          string            `json:"job_id"`
	LeaseToken     string            `json:"lease_token"`
	OK             bool              `json:"ok"`
	Error          string            `json:"error,omitempty"`
	Upstream       string            `json:"upstream,omitempty"`
	Images         map[string]string `json:"images,omitempty"`
	Bundle         json.RawMessage   `json:"bundle,omitempty"`
	MachineChanged bool              `json:"machine_changed,omitempty"`
}

// Step states.
const (
	StepStarted   = "started"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// JobStep reports that a step of a job changed state, such as "provision",
// "source", "build", "start", "migrate" or "health".
type JobStep struct {
	JobID      string    `json:"job_id"`
	LeaseToken string    `json:"lease_token"`
	Step       string    `json:"step"`
	State      string    `json:"state"`
	At         time.Time `json:"at"`
	Message    string    `json:"message,omitempty"`
}

// LogLine is one line of output from a job.
type LogLine struct {
	At     time.Time `json:"at"`
	Step   string    `json:"step"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// JobLog carries a batch of a job's output lines, oldest first. Agents never
// put secret values in them.
type JobLog struct {
	JobID      string    `json:"job_id"`
	LeaseToken string    `json:"lease_token"`
	Lines      []LogLine `json:"lines"`
}

// JobRef names a job an agent holds a lease for.
type JobRef struct {
	JobID      string `json:"job_id"`
	LeaseToken string `json:"lease_token"`
}

// JobsInProgress lists the jobs an agent kept running across a reconnect.
// The server answers each with JobAdopted or JobAbandoned.
type JobsInProgress struct {
	Jobs []JobRef `json:"jobs"`
}

// JobAdopted lets the agent continue a job under its lease.
type JobAdopted struct {
	JobID string `json:"job_id"`
}

// JobAbandoned tells the agent to stop a job: its lease expired or the
// server no longer wants it. Results under that lease will be rejected.
type JobAbandoned struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}

// Machine kinds.
const (
	MachineRuntime     = "runtime"
	MachineBuildWorker = "build_worker"
)

// Machine states as the agent knows them.
const (
	MachineCreating = "creating"
	MachineReady    = "ready"
	MachineDeleting = "deleting"
)

// Machine is one machine the agent created in the cloud. Status is
// MachineCreating, MachineReady or MachineDeleting.
type Machine struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	PreviewID string `json:"preview_id,omitempty"`
	Status    string `json:"status"`
}

// Inventory lists every machine the agent finds in the cloud with its own
// tag. Agents send it after connecting and then periodically.
type Inventory struct {
	Machines []Machine `json:"machines"`
}

// Route maps a preview host name to its upstream address. HealthPath is
// the path, starting with /, that answers with a 2xx status while the app
// is healthy; the gateway requests it to check the app.
type Route struct {
	PreviewID  string `json:"preview_id"`
	Host       string `json:"host"`
	Upstream   string `json:"upstream"`
	HealthPath string `json:"health_path,omitempty"`
}

// Routes is the full route table for a gateway.
type Routes struct {
	Routes []Route `json:"routes"`
}

// RoutesApplied lists the previews whose routes the gateway now serves.
type RoutesApplied struct {
	PreviewIDs []string `json:"preview_ids"`
}

// HandoffExchange asks the server to redeem a visitor's handoff token.
type HandoffExchange struct {
	RequestID string `json:"request_id"`
	Token     string `json:"token"`
}

// HandoffResult answers a HandoffExchange.
type HandoffResult struct {
	RequestID string    `json:"request_id"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	PreviewID string    `json:"preview_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// SessionConfirm asks which of the gateway's sessions are still valid.
type SessionConfirm struct {
	RequestID  string   `json:"request_id"`
	SessionIDs []string `json:"session_ids"`
}

// SessionConfirmed lists the sessions that are still valid. Error is set
// when the server could not check the sessions: the reply then confirms
// none of them and revokes none, and the gateway asks again later.
type SessionConfirmed struct {
	RequestID string   `json:"request_id"`
	Valid     []string `json:"valid"`
	Error     string   `json:"error,omitempty"`
}

// RouteState is what the gateway found when it checked one preview's
// route. Reachable means it opened a TCP connection to the upstream, and
// Error says why it could not. Healthy means the app answered a request for
// the route's health path with a 2xx status, and HealthError says why it
// did not. Upstream names the address the gateway checked, so a state for
// an upstream the preview no longer uses can be told apart.
type RouteState struct {
	PreviewID   string    `json:"preview_id"`
	Upstream    string    `json:"upstream"`
	Reachable   bool      `json:"reachable"`
	Error       string    `json:"error,omitempty"`
	Healthy     bool      `json:"healthy"`
	HealthError string    `json:"health_error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// RouteHealth reports the reachability and app health of the gateway's
// routes. On each session the gateway first reports every route, then only
// the routes that are new or whose reachability, health, error or reason
// changed. It may split a report into several messages.
type RouteHealth struct {
	Routes []RouteState `json:"routes"`
}
