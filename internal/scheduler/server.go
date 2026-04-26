package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
	"yadcc-go/internal/buildinfo"
	"yadcc-go/internal/metrics"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const workerHeartbeatTimeout = 30 * time.Second

// workerEntry tracks a registered remote worker.
type workerEntry struct {
	id              string
	location        string // "host:port" of the worker's gRPC endpoint
	capacity        uint32
	currentLoad     uint32
	isDedicated     bool
	availableMemory uint64
	lastSeen        time.Time
	environments    []*pb.EnvironmentDesc
}

// taskGrant tracks an issued task grant.
type taskGrant struct {
	id        uint64
	workerID  string
	issuedAt  time.Time
	keepAlive time.Time
}

// Server implements SchedulerServiceServer over gRPC, and also exposes a small
// HTTP debug/healthz endpoint on a separate port.
type Server struct {
	pb.UnimplementedSchedulerServiceServer

	// GRPCAddr is the address the gRPC server listens on (e.g. "0.0.0.0:8336").
	GRPCAddr string
	// HTTPAddr is the optional debug/healthz HTTP address (e.g. "0.0.0.0:8337").
	// Leave empty to disable.
	HTTPAddr string

	initOnce sync.Once
	mu       sync.Mutex
	cond     *sync.Cond // broadcast when worker state changes
	workers  map[string]*workerEntry
	grants   map[uint64]*taskGrant
	nextID   atomic.Uint64
}

// ensureInit initialises the maps and cond exactly once.  It is safe to call
// concurrently and from test code that never calls ListenAndServe.
func (s *Server) ensureInit() {
	s.initOnce.Do(func() {
		s.cond = sync.NewCond(&s.mu)
		s.workers = make(map[string]*workerEntry)
		s.grants = make(map[uint64]*taskGrant)
	})
}

// ListenAndServe starts both the gRPC server and (if HTTPAddr set) the HTTP
// debug server.  It blocks until the gRPC server stops.
func (s *Server) ListenAndServe() error {
	s.ensureInit()

	go s.evictLoop()

	if s.HTTPAddr != "" {
		go s.serveHTTP()
	}

	lis, err := net.Listen("tcp", s.GRPCAddr)
	if err != nil {
		return fmt.Errorf("scheduler: listen %s: %w", s.GRPCAddr, err)
	}
	gs := grpc.NewServer()
	pb.RegisterSchedulerServiceServer(gs, s)
	slog.Info("scheduler: gRPC server listening", "addr", s.GRPCAddr)
	return gs.Serve(lis)
}

// ---------- SchedulerServiceServer implementation ----------

// Heartbeat is called by remote workers to register themselves and report load.
func (s *Server) Heartbeat(_ context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	s.ensureInit()
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workers[req.Token]
	if !ok {
		w = &workerEntry{id: req.Token}
		s.workers[req.Token] = w
		slog.Info("scheduler: worker registered via heartbeat", "id", req.Token, "location", req.Location)
	}
	w.location = req.Location
	w.capacity = req.Capacity
	w.currentLoad = req.CurrentLoad
	w.isDedicated = req.IsDedicated
	w.availableMemory = req.AvailableMemoryBytes
	w.lastSeen = time.Now()
	w.environments = req.Environments

	metrics.SchedulerHeartbeatsTotal.Inc()
	metrics.SchedulerWorkersActive.Set(float64(len(s.workers)))

	// Collect expired grant IDs to notify the worker.
	var expired []uint64
	for gid, g := range s.grants {
		if g.workerID == req.Token && time.Since(g.keepAlive) > 2*time.Minute {
			expired = append(expired, gid)
			delete(s.grants, gid)
		}
	}

	// Wake any WaitForStartingTask callers that may now be satisfiable.
	s.cond.Broadcast()

	return &pb.HeartbeatResponse{
		ExpiredTaskGrantIds: expired,
	}, nil
}

// WaitForStartingTask allocates task grants for a requester daemon.
//
// If no worker is immediately available it waits up to milliseconds_to_wait
// milliseconds (honouring the gRPC context deadline) for a worker to free up,
// then returns ResourceExhausted if still none available.
func (s *Server) WaitForStartingTask(ctx context.Context, req *pb.WaitForStartingTaskRequest) (*pb.WaitForStartingTaskResponse, error) {
	s.ensureInit()
	want := int(req.ImmediateRequests)
	if want <= 0 {
		want = 1
	}

	waitMs := req.MillisecondsToWait
	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)

	// Use a separate goroutine to broadcast the cond when the context or
	// deadline fires, so we don't block forever inside cond.Wait.
	stopWake := make(chan struct{})
	go func() {
		var d <-chan time.Time
		if waitMs > 0 {
			d = time.After(time.Duration(waitMs) * time.Millisecond)
		}
		select {
		case <-stopWake:
		case <-ctx.Done():
			s.cond.Broadcast()
		case <-d:
			s.cond.Broadcast()
		}
	}()
	defer close(stopWake)

	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		var grants []*pb.StartingTaskGrant
		for range want {
			w := s.pickWorker(req.Environment, req.RequesterLocation)
			if w == nil {
				break
			}
			w.currentLoad++
			id := s.nextID.Add(1)
			s.grants[id] = &taskGrant{
				id:        id,
				workerID:  w.id,
				issuedAt:  time.Now(),
				keepAlive: time.Now(),
			}
			grants = append(grants, &pb.StartingTaskGrant{
				TaskGrantId:     id,
				ServantLocation: w.location,
			})
		}

		if len(grants) > 0 {
			metrics.SchedulerGrantsActive.Set(float64(len(s.grants)))
			return &pb.WaitForStartingTaskResponse{Grants: grants}, nil
		}

		// Check if we should stop waiting.
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if waitMs == 0 || time.Now().After(deadline) {
			return nil, status.Error(codes.ResourceExhausted, "no available workers")
		}

		s.cond.Wait() // releases s.mu, reacquires on wake
	}
}

// KeepTaskAlive refreshes the keep-alive timestamp for the given grants.
func (s *Server) KeepTaskAlive(_ context.Context, req *pb.KeepTaskAliveRequest) (*pb.KeepTaskAliveResponse, error) {
	s.ensureInit()
	s.mu.Lock()
	defer s.mu.Unlock()

	statuses := make([]bool, len(req.TaskGrantIds))
	for i, gid := range req.TaskGrantIds {
		if g, ok := s.grants[gid]; ok {
			g.keepAlive = time.Now()
			statuses[i] = true
		}
	}
	return &pb.KeepTaskAliveResponse{Statuses: statuses}, nil
}

// FreeTask releases the given task grants.
func (s *Server) FreeTask(_ context.Context, req *pb.FreeTaskRequest) (*pb.FreeTaskResponse, error) {
	s.ensureInit()
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, gid := range req.TaskGrantIds {
		if g, ok := s.grants[gid]; ok {
			if w, ok := s.workers[g.workerID]; ok && w.currentLoad > 0 {
				w.currentLoad--
			}
			delete(s.grants, gid)
		}
	}
	metrics.SchedulerGrantsActive.Set(float64(len(s.grants)))
	s.cond.Broadcast()
	return &pb.FreeTaskResponse{}, nil
}

// ---------- helpers ----------

// pickWorker selects the best available worker for the given environment.
//
// Selection policy (matching C++ yadcc):
//  1. Dedicated workers are preferred over user-mode workers.
//  2. Among workers of equal priority, prefer the one with the most free
//     capacity (capacity - currentLoad).
//  3. Self-avoidance: a worker whose location matches requesterLocation is
//     only used as a last resort (when no other worker has capacity).
//
// Must be called with s.mu held.
func (s *Server) pickWorker(env *pb.EnvironmentDesc, requesterLocation string) *workerEntry {
	const minAvailableMemoryBytes = 10 << 30 // 10 GiB

	var bestDedicated, bestUser, bestSelf *workerEntry

	score := func(w *workerEntry) uint32 { return w.capacity - w.currentLoad }

	for _, w := range s.workers {
		if w.currentLoad >= w.capacity {
			continue
		}
		if env != nil && !workerSupportsEnv(w, env) {
			continue
		}
		// Skip workers with critically low memory.
		if w.availableMemory > 0 && w.availableMemory < minAvailableMemoryBytes {
			continue
		}

		// Self-avoidance bucket.
		if requesterLocation != "" && w.location == requesterLocation {
			if bestSelf == nil || score(w) > score(bestSelf) {
				bestSelf = w
			}
			continue
		}

		if w.isDedicated {
			if bestDedicated == nil || score(w) > score(bestDedicated) {
				bestDedicated = w
			}
		} else {
			if bestUser == nil || score(w) > score(bestUser) {
				bestUser = w
			}
		}
	}

	if bestDedicated != nil {
		return bestDedicated
	}
	if bestUser != nil {
		return bestUser
	}
	return bestSelf // last resort: assign to requester itself
}

// workerSupportsEnv checks whether the worker advertises the requested env.
// When env is empty (zero-value), any worker is accepted.
func workerSupportsEnv(w *workerEntry, env *pb.EnvironmentDesc) bool {
	if env == nil || (env.CompilerDigest == "" &&
		env.CompilerKind == "" &&
		env.CompilerVersion == "" &&
		env.HostOs == "" &&
		env.HostArch == "" &&
		env.TargetTriple == "" &&
		env.ObjectFormat == "" &&
		env.SysrootDigest == "" &&
		env.StdlibDigest == "" &&
		env.Abi == "" &&
		env.CacheFormatVersion == 0) {
		return true
	}
	for _, e := range w.environments {
		if !fieldMatches(env.CompilerDigest, e.CompilerDigest) {
			continue
		}
		// The binary digest is the strongest compiler identity. When it is
		// present, compiler kind/version may differ only because the same
		// binary was reached through an alias such as cc -> clang.
		compilerIdentityMatches := env.CompilerDigest != "" ||
			(fieldMatches(env.CompilerKind, e.CompilerKind) &&
				fieldMatches(env.CompilerVersion, e.CompilerVersion))
		if compilerIdentityMatches &&
			fieldMatches(env.HostOs, e.HostOs) &&
			fieldMatches(env.HostArch, e.HostArch) &&
			fieldMatches(env.TargetTriple, e.TargetTriple) &&
			fieldMatches(env.ObjectFormat, e.ObjectFormat) &&
			fieldMatches(env.SysrootDigest, e.SysrootDigest) &&
			fieldMatches(env.StdlibDigest, e.StdlibDigest) &&
			fieldMatches(env.Abi, e.Abi) &&
			versionMatches(env.CacheFormatVersion, e.CacheFormatVersion) {
			return true
		}
	}
	return false
}

func fieldMatches(want, got string) bool {
	return want == "" || want == got
}

func versionMatches(want, got uint32) bool {
	return want == 0 || want == got
}

// evictLoop removes workers whose heartbeat has timed out and cleans up their
// associated grants.
func (s *Server) evictLoop() {
	s.ensureInit()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, w := range s.workers {
			if now.Sub(w.lastSeen) > workerHeartbeatTimeout {
				slog.Info("scheduler: evicting stale worker", "id", id)
				// Clean up any grants associated with this worker.
				for gid, g := range s.grants {
					if g.workerID == id {
						delete(s.grants, gid)
					}
				}
				delete(s.workers, id)
			}
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// GetRunningTasks returns information about all currently in-flight task
// grants.  Any authenticated token is accepted (same as other RPCs).
func (s *Server) GetRunningTasks(_ context.Context, req *pb.GetRunningTasksRequest) (*pb.GetRunningTasksResponse, error) {
	s.ensureInit()
	if req.Token == "" {
		return nil, status.Error(codes.Unauthenticated, "token required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks := make([]*pb.RunningTaskInfo, 0, len(s.grants))
	for gid, g := range s.grants {
		loc := g.workerID
		if w, ok := s.workers[g.workerID]; ok {
			loc = w.location
		}
		tasks = append(tasks, &pb.RunningTaskInfo{
			TaskGrantId:    gid,
			WorkerLocation: loc,
			AgeSeconds:     uint32(time.Since(g.issuedAt).Seconds()),
		})
	}
	return &pb.GetRunningTasksResponse{
		Tasks:        tasks,
		TotalWorkers: uint32(len(s.workers)),
	}, nil
}

// ---------- HTTP debug endpoint ----------

func (s *Server) serveHTTP() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/scheduler/state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		workers := len(s.workers)
		grants := len(s.grants)
		tasks := make([]map[string]any, 0, len(s.grants))
		for gid, g := range s.grants {
			tasks = append(tasks, map[string]any{
				"task_grant_id":   gid,
				"worker_location": s.workerLocationLocked(g.workerID),
				"age_seconds":     uint32(time.Since(g.issuedAt).Seconds()),
			})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"version":       buildinfo.String(),
			"workers":       workers,
			"running_tasks": grants,
			"tasks":         tasks,
		})
	})
	mux.Handle("/metrics", metrics.Handler())
	slog.Info("scheduler: HTTP debug server listening", "addr", s.HTTPAddr)
	_ = http.ListenAndServe(s.HTTPAddr, mux)
}

// workerLocationLocked returns the servant location for a worker id.
// Must be called with s.mu held.
func (s *Server) workerLocationLocked(workerID string) string {
	if w, ok := s.workers[workerID]; ok {
		return w.location
	}
	return workerID
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
