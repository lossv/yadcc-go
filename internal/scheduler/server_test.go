package scheduler

import (
	"context"
	"testing"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
)

// newServer returns a fresh Server with init done (no network).
func newServer() *Server {
	s := &Server{}
	s.ensureInit()
	return s
}

// registerWorker sends a synthetic heartbeat so tests don't need a live socket.
func registerWorker(t *testing.T, s *Server, id, location string, capacity, load uint32, envs ...*pb.EnvironmentDesc) {
	t.Helper()
	_, err := s.Heartbeat(context.Background(), &pb.HeartbeatRequest{
		Token:        id,
		Location:     location,
		Capacity:     capacity,
		CurrentLoad:  load,
		Environments: envs,
	})
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
}

// ---------- Heartbeat ----------

func TestHeartbeat_registersWorker(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "127.0.0.1:9001", 4, 0)

	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workers["w1"]
	if !ok {
		t.Fatal("worker w1 not found after heartbeat")
	}
	if w.capacity != 4 {
		t.Errorf("capacity = %d, want 4", w.capacity)
	}
	if w.location != "127.0.0.1:9001" {
		t.Errorf("location = %q, want 127.0.0.1:9001", w.location)
	}
}

func TestHeartbeat_updatesExistingWorker(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "host:1", 2, 0)
	registerWorker(t, s, "w1", "host:2", 8, 3) // update

	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workers["w1"]
	if w.location != "host:2" {
		t.Errorf("location not updated: got %q", w.location)
	}
	if w.capacity != 8 {
		t.Errorf("capacity not updated: got %d", w.capacity)
	}
}

// ---------- WaitForStartingTask ----------

func TestWaitForStartingTask_getsGrant(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "127.0.0.1:9001", 4, 0)

	resp, err := s.WaitForStartingTask(context.Background(), &pb.WaitForStartingTaskRequest{
		ImmediateRequests: 1,
		MillisecondsToWait: 0,
	})
	if err != nil {
		t.Fatalf("WaitForStartingTask failed: %v", err)
	}
	if len(resp.Grants) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(resp.Grants))
	}
	if resp.Grants[0].ServantLocation != "127.0.0.1:9001" {
		t.Errorf("grant location = %q, want 127.0.0.1:9001", resp.Grants[0].ServantLocation)
	}
}

func TestWaitForStartingTask_noWorkerReturnsError(t *testing.T) {
	s := newServer()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := s.WaitForStartingTask(ctx, &pb.WaitForStartingTaskRequest{
		ImmediateRequests: 1,
		MillisecondsToWait: 0,
	})
	if err == nil {
		t.Fatal("expected error when no workers available")
	}
}

func TestWaitForStartingTask_capacityExhausted(t *testing.T) {
	s := newServer()
	// capacity=1, currentLoad=1 → no room
	registerWorker(t, s, "w1", "host:1", 1, 1)

	_, err := s.WaitForStartingTask(context.Background(), &pb.WaitForStartingTaskRequest{
		ImmediateRequests:  1,
		MillisecondsToWait: 0,
	})
	if err == nil {
		t.Fatal("expected ResourceExhausted when worker is full")
	}
}

func TestWaitForStartingTask_incrementsLoad(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "host:1", 4, 0)

	_, _ = s.WaitForStartingTask(context.Background(), &pb.WaitForStartingTaskRequest{
		ImmediateRequests:  1,
		MillisecondsToWait: 0,
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers["w1"].currentLoad != 1 {
		t.Errorf("expected currentLoad=1, got %d", s.workers["w1"].currentLoad)
	}
}

// ---------- KeepTaskAlive ----------

func TestKeepTaskAlive_refreshesGrant(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "host:1", 4, 0)
	resp, _ := s.WaitForStartingTask(context.Background(), &pb.WaitForStartingTaskRequest{
		ImmediateRequests:  1,
		MillisecondsToWait: 0,
	})
	grantID := resp.Grants[0].TaskGrantId

	// Backdate the keepAlive timestamp to simulate age.
	s.mu.Lock()
	s.grants[grantID].keepAlive = time.Now().Add(-time.Minute)
	s.mu.Unlock()

	kaResp, err := s.KeepTaskAlive(context.Background(), &pb.KeepTaskAliveRequest{
		TaskGrantIds: []uint64{grantID},
	})
	if err != nil {
		t.Fatalf("KeepTaskAlive error: %v", err)
	}
	if len(kaResp.Statuses) != 1 || !kaResp.Statuses[0] {
		t.Errorf("expected status[0]=true, got %v", kaResp.Statuses)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.grants[grantID].keepAlive) > 2*time.Second {
		t.Error("keepAlive not refreshed")
	}
}

func TestKeepTaskAlive_unknownGrantReturnsFalse(t *testing.T) {
	s := newServer()
	resp, err := s.KeepTaskAlive(context.Background(), &pb.KeepTaskAliveRequest{
		TaskGrantIds: []uint64{99999},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Statuses[0] {
		t.Error("expected false for unknown grant")
	}
}

// ---------- FreeTask ----------

func TestFreeTask_decrementsLoad(t *testing.T) {
	s := newServer()
	registerWorker(t, s, "w1", "host:1", 4, 0)
	grantResp, _ := s.WaitForStartingTask(context.Background(), &pb.WaitForStartingTaskRequest{
		ImmediateRequests:  1,
		MillisecondsToWait: 0,
	})
	grantID := grantResp.Grants[0].TaskGrantId

	_, err := s.FreeTask(context.Background(), &pb.FreeTaskRequest{
		TaskGrantIds: []uint64{grantID},
	})
	if err != nil {
		t.Fatalf("FreeTask error: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers["w1"].currentLoad != 0 {
		t.Errorf("expected currentLoad=0 after FreeTask, got %d", s.workers["w1"].currentLoad)
	}
	if _, ok := s.grants[grantID]; ok {
		t.Error("grant should be deleted after FreeTask")
	}
}

// ---------- workerSupportsEnv ----------

func TestWorkerSupportsEnv_emptyEnv(t *testing.T) {
	w := &workerEntry{}
	if !workerSupportsEnv(w, &pb.EnvironmentDesc{}) {
		t.Error("empty EnvironmentDesc should match any worker")
	}
}

func TestWorkerSupportsEnv_matchingDigest(t *testing.T) {
	w := &workerEntry{
		environments: []*pb.EnvironmentDesc{
			{CompilerDigest: "abc123", HostOs: "linux", HostArch: "amd64"},
		},
	}
	if !workerSupportsEnv(w, &pb.EnvironmentDesc{CompilerDigest: "abc123"}) {
		t.Error("worker should support matching digest")
	}
}

func TestWorkerSupportsEnv_missingDigest(t *testing.T) {
	w := &workerEntry{
		environments: []*pb.EnvironmentDesc{
			{CompilerDigest: "abc123"},
		},
	}
	if workerSupportsEnv(w, &pb.EnvironmentDesc{CompilerDigest: "xyz999"}) {
		t.Error("worker should NOT support non-matching digest")
	}
}

func TestWorkerSupportsEnv_archMismatch(t *testing.T) {
	w := &workerEntry{
		environments: []*pb.EnvironmentDesc{
			{CompilerDigest: "abc123", HostArch: "arm64"},
		},
	}
	env := &pb.EnvironmentDesc{CompilerDigest: "abc123", HostArch: "amd64"}
	if workerSupportsEnv(w, env) {
		t.Error("worker with arm64 should NOT match amd64 request")
	}
}
