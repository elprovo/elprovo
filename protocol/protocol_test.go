package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestWireFormatIsStable(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		payload any
		want    string
	}{
		{
			name:    "deploy job",
			msgType: TypeJob,
			payload: Job{ID: "op1", LeaseToken: "lt", Kind: JobDeploy, PreviewID: "s1", Deploy: &DeploySpec{
				Repository: "acme/web", Commit: "abc", SourceToken: "tok", SourceAPIURL: "https://api.github.com", ComposeFiles: []string{"compose.yml"},
				PublicService: "web", PublicPort: 8080, HealthPath: "/healthz",
			}},
			want: `{"type":"job","payload":{"id":"op1","lease_token":"lt","kind":"deploy","preview_id":"s1","deploy":{"repository":"acme/web","commit":"abc","source_token":"tok","source_api_url":"https://api.github.com","compose_files":["compose.yml"],"public_service":"web","public_port":8080,"health_path":"/healthz"}}}`,
		},
		{
			name:    "job result",
			msgType: TypeJobResult,
			payload: JobResult{JobID: "op1", LeaseToken: "lt", OK: true, Upstream: "192.168.64.5:8080"},
			want:    `{"type":"job_result","payload":{"job_id":"op1","lease_token":"lt","ok":true,"upstream":"192.168.64.5:8080"}}`,
		},
		{
			name:    "routes",
			msgType: TypeRoutes,
			payload: Routes{Routes: []Route{{PreviewID: "s1", Host: "s1.previews.localhost", Upstream: "192.168.64.5:8080", HealthPath: "/healthz"}}},
			want:    `{"type":"routes","payload":{"routes":[{"preview_id":"s1","host":"s1.previews.localhost","upstream":"192.168.64.5:8080","health_path":"/healthz"}]}}`,
		},
		{
			name:    "auth",
			msgType: TypeAuth,
			payload: Auth{Role: RoleGateway, ProtocolVersion: 1, PublicKey: []byte{1, 2}, Signature: []byte{3}, EnrollmentToken: "et"},
			want:    `{"type":"auth","payload":{"role":"gateway","protocol_version":1,"public_key":"AQI=","signature":"Aw==","enrollment_token":"et"}}`,
		},
		{
			name:    "deploy job with config, secrets and migration",
			msgType: TypeJob,
			payload: Job{ID: "op1", LeaseToken: "lt", Kind: JobDeploy, PreviewID: "s1", Deploy: &DeploySpec{
				Repository: "acme/web", Commit: "abc", SourceToken: "tok", SourceAPIURL: "https://api.github.com", ComposeFiles: []string{"compose.yml"},
				PublicService: "web", PublicPort: 8080, HealthPath: "/healthz", MigrationCommand: "bin/migrate",
				Config: map[string]string{"MODE": "preview"}, Secrets: []SecretRef{{Name: "DB_PASSWORD", Key: "db"}},
			}},
			want: `{"type":"job","payload":{"id":"op1","lease_token":"lt","kind":"deploy","preview_id":"s1","deploy":{"repository":"acme/web","commit":"abc","source_token":"tok","source_api_url":"https://api.github.com","compose_files":["compose.yml"],"public_service":"web","public_port":8080,"health_path":"/healthz","migration_command":"bin/migrate","config":{"MODE":"preview"},"secrets":[{"name":"DB_PASSWORD","key":"db"}]}}}`,
		},
		{
			name:    "delete machine job",
			msgType: TypeJob,
			payload: Job{ID: "op3", LeaseToken: "lt", Kind: JobDeleteMachine, Machine: "elprovo-old"},
			want:    `{"type":"job","payload":{"id":"op3","lease_token":"lt","kind":"delete_machine","machine":"elprovo-old"}}`,
		},
		{
			name:    "job step",
			msgType: TypeJobStep,
			payload: JobStep{JobID: "op1", LeaseToken: "lt", Step: "build", State: StepStarted, At: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)},
			want:    `{"type":"job_step","payload":{"job_id":"op1","lease_token":"lt","step":"build","state":"started","at":"2026-10-10T12:00:00Z"}}`,
		},
		{
			name:    "job log",
			msgType: TypeJobLog,
			payload: JobLog{JobID: "op1", LeaseToken: "lt", Lines: []LogLine{{At: time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC), Step: "build", Stream: "stdout", Text: "Step 1/4"}}},
			want:    `{"type":"job_log","payload":{"job_id":"op1","lease_token":"lt","lines":[{"at":"2026-10-10T12:00:01Z","step":"build","stream":"stdout","text":"Step 1/4"}]}}`,
		},
		{
			name:    "jobs in progress",
			msgType: TypeJobsInProgress,
			payload: JobsInProgress{Jobs: []JobRef{{JobID: "op1", LeaseToken: "lt"}}},
			want:    `{"type":"jobs_in_progress","payload":{"jobs":[{"job_id":"op1","lease_token":"lt"}]}}`,
		},
		{
			name:    "job result with images",
			msgType: TypeJobResult,
			payload: JobResult{JobID: "op1", LeaseToken: "lt", OK: true, Upstream: "10.0.0.5:8080", Images: map[string]string{"web": "registry/web@sha256:aa"}},
			want:    `{"type":"job_result","payload":{"job_id":"op1","lease_token":"lt","ok":true,"upstream":"10.0.0.5:8080","images":{"web":"registry/web@sha256:aa"}}}`,
		},
		{
			name:    "reset job with a bundle",
			msgType: TypeJob,
			payload: Job{ID: "op2", LeaseToken: "lt", Kind: JobReset, PreviewID: "s1", Reset: &ResetSpec{Bundle: json.RawMessage(`{"services":{}}`), PublicService: "web", PublicPort: 8080, HealthPath: "/"}},
			want:    `{"type":"job","payload":{"id":"op2","lease_token":"lt","kind":"reset","preview_id":"s1","reset":{"bundle":{"services":{}},"public_service":"web","public_port":8080,"health_path":"/"}}}`,
		},
		{
			name:    "inventory",
			msgType: TypeInventory,
			payload: Inventory{Machines: []Machine{{Name: "elprovo-s1", Kind: MachineRuntime, PreviewID: "s1", Status: MachineReady}}},
			want:    `{"type":"inventory","payload":{"machines":[{"name":"elprovo-s1","kind":"runtime","preview_id":"s1","status":"ready"}]}}`,
		},
		{
			name:    "session confirmation the server could not check",
			msgType: TypeSessionConfirmed,
			payload: SessionConfirmed{RequestID: "r1", Error: "internal error"},
			want:    `{"type":"session_confirmed","payload":{"request_id":"r1","valid":null,"error":"internal error"}}`,
		},
		{
			name:    "route health",
			msgType: TypeRouteHealth,
			payload: RouteHealth{Routes: []RouteState{{PreviewID: "s1", Upstream: "10.0.0.5:8080", Reachable: false, Error: "connection refused", HealthError: "the gateway cannot reach the app", CheckedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}}},
			want:    `{"type":"route_health","payload":{"routes":[{"preview_id":"s1","upstream":"10.0.0.5:8080","reachable":false,"error":"connection refused","healthy":false,"health_error":"the gateway cannot reach the app","checked_at":"2026-10-10T12:00:00Z"}]}}`,
		},
		{
			name:    "route health of a reachable, healthy app",
			msgType: TypeRouteHealth,
			payload: RouteHealth{Routes: []RouteState{{PreviewID: "s1", Upstream: "10.0.0.5:8080", Reachable: true, Healthy: true, CheckedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}}},
			want:    `{"type":"route_health","payload":{"routes":[{"preview_id":"s1","upstream":"10.0.0.5:8080","reachable":true,"healthy":true,"checked_at":"2026-10-10T12:00:00Z"}]}}`,
		},
		{
			name:    "failed job result after the machine changed",
			msgType: TypeJobResult,
			payload: JobResult{JobID: "op1", LeaseToken: "lt", Error: "health: no answer", MachineChanged: true},
			want:    `{"type":"job_result","payload":{"job_id":"op1","lease_token":"lt","ok":false,"error":"health: no answer","machine_changed":true}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Encode(tt.msgType, tt.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("Encode =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestDecodeReadsPayload(t *testing.T) {
	env, err := Decode([]byte(`{"type":"lease_renew","payload":{"job_id":"op1","lease_token":"lt"}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var got LeaseRenew
	if err := env.Into(&got); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if want := (LeaseRenew{JobID: "op1", LeaseToken: "lt"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
}

func TestDecodeRejectsMissingType(t *testing.T) {
	if _, err := Decode([]byte(`{"payload":{}}`)); err == nil {
		t.Fatal("Decode accepted an envelope without a type")
	}
}
