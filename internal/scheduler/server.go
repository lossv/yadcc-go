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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const workerHeartbeatTimeout = 30 * time.Second

// workerEntry tracks a registered remote worker.
type workerEntry struct {
	id           string
	location     string // "host:port" of the worker's gRPC endpoint
	capacity     uint32
	currentLoad  uint32
	lastSeen     time.Time
	environments []*pb.EnvironmentDesc
}

// taskGrant tracks an issued task grant.
type taskGrant struct {
	id         uint64
	workerID   string
	issuedAt   time.Time
	keepAlive  time.Time
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

	mu      sync.Mutex
	workers map[string]*workerEntry
	grants  map[uint64]*taskGrant
	nextID  atomic.Uint64
}

// ListenAndServe starts both the gRPC server and (if HTTPAddr set) the HTTP
// debug server.  It blocks until the gRPC server stops.
func (s *Server) ListenAndServe() error {
	s.mu.Lock()
	s.workers = make(map[string]*workerEntry)
	s.grants = make(map[uint64]*taskGrant)
	s.mu.Unlock()

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
	w.lastSeen = time.Now()
	w.environments = req.Environments

	// Collect expired grant IDs to notify the worker.
	var expired []uint64
	for gid, g := range s.grants {
		if g.workerID == req.Token && time.Since(g.keepAlive) > 2*time.Minute {
			expired = append(expired, gid)
			delete(s.grants, gid)
		}
	}

	return &pb.HeartbeatResponse{
		ExpiredTaskGrantIds: expired,
	}, nil
}

// WaitForStartingTask allocates task grants for a requester daemon.
func (s *Server) WaitForStartingTask(_ context.Context, req *pb.WaitForStartingTaskRequest) (*pb.WaitForStartingTaskResponse, error) {
	want := int(req.ImmediateRequests)
	if want <= 0 {
		want = 1
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var grants []*pb.StartingTaskGrant
	for range want {
		w := s.pickWorker(req.Environment)
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

	if len(grants) == 0 {
		return nil, status.Error(codes.ResourceExhausted, "no available workers")
	}
	return &pb.WaitForStartingTaskResponse{Grants: grants}, nil
}

// KeepTaskAlive refreshes the keep-alive timestamp for the given grants.
func (s *Server) KeepTaskAlive(_ context.Context, req *pb.KeepTaskAliveRequest) (*pb.KeepTaskAliveResponse, error) {
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
	return &pb.FreeTaskResponse{}, nil
}

// ---------- helpers ----------

// pickWorker selects the least-loaded worker that supports the requested
// environment.  Must be called with s.mu held.
func (s *Server) pickWorker(env *pb.EnvironmentDesc) *workerEntry {
	var best *workerEntry
	for _, w := range s.workers {
		if w.currentLoad >= w.capacity {
			continue
		}
		if env != nil && !workerSupportsEnv(w, env) {
			continue
		}
		if best == nil || w.currentLoad < best.currentLoad {
			best = w
		}
	}
	return best
}

// workerSupportsEnv checks whether the worker advertises the requested env.
// When env is empty (zero-value), any worker is accepted.
func workerSupportsEnv(w *workerEntry, env *pb.EnvironmentDesc) bool {
	if env.CompilerDigest == "" {
		return true
	}
	for _, e := range w.environments {
		if e.CompilerDigest == env.CompilerDigest &&
			(env.HostOs == "" || e.HostOs == env.HostOs) &&
			(env.HostArch == "" || e.HostArch == env.HostArch) {
			return true
		}
	}
	return false
}

// evictLoop removes workers whose heartbeat has timed out.
func (s *Server) evictLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, w := range s.workers {
			if now.Sub(w.lastSeen) > workerHeartbeatTimeout {
				slog.Info("scheduler: evicting stale worker", "id", id)
				delete(s.workers, id)
			}
		}
		s.mu.Unlock()
	}
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
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"version":       buildinfo.String(),
			"workers":       workers,
			"running_tasks": grants,
		})
	})
	slog.Info("scheduler: HTTP debug server listening", "addr", s.HTTPAddr)
	_ = http.ListenAndServe(s.HTTPAddr, mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
