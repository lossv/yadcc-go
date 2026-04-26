package scheduler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"yadcc-go/internal/buildinfo"
)

const (
	workerHeartbeatTimeout = 30 * time.Second
)

// Worker represents a registered remote compilation worker.
type Worker struct {
	ID           string    `json:"id"`
	Addr         string    `json:"addr"`          // host:port
	CompilerPath string    `json:"compiler_path"` // path to compiler on worker
	Capacity     int       `json:"capacity"`      // max concurrent tasks
	Running      int       `json:"running"`       // current running tasks
	LastSeen     time.Time `json:"last_seen"`
}

// Server is the scheduler HTTP server.
type Server struct {
	Addr string

	mu      sync.Mutex
	workers map[string]*Worker // keyed by ID
}

// RegisterRequest is the JSON body for /scheduler/register.
type RegisterRequest struct {
	ID           string `json:"id"`
	Addr         string `json:"addr"`
	CompilerPath string `json:"compiler_path"`
	Capacity     int    `json:"capacity"`
}

// HeartbeatRequest is the JSON body for /scheduler/heartbeat.
type HeartbeatRequest struct {
	ID      string `json:"id"`
	Running int    `json:"running"`
}

// AcquireWorkerResponse is the JSON response for /scheduler/acquire_worker.
type AcquireWorkerResponse struct {
	WorkerAddr string `json:"worker_addr"`
	TaskToken  string `json:"task_token"`
}

func (s *Server) ListenAndServe() error {
	s.mu.Lock()
	s.workers = make(map[string]*Worker)
	s.mu.Unlock()

	// Start background goroutine to evict stale workers.
	go s.evictLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/scheduler/state", s.handleState)
	mux.HandleFunc("/scheduler/register", s.handleRegister)
	mux.HandleFunc("/scheduler/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("/scheduler/acquire_worker", s.handleAcquireWorker)
	mux.HandleFunc("/scheduler/release_worker", s.handleReleaseWorker)
	return http.ListenAndServe(s.Addr, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	workers := len(s.workers)
	running := 0
	for _, w := range s.workers {
		running += w.Running
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       buildinfo.String(),
		"workers":       workers,
		"running_tasks": running,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req RegisterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" || req.Addr == "" {
		http.Error(w, "id and addr are required", http.StatusBadRequest)
		return
	}
	if req.Capacity <= 0 {
		req.Capacity = 4
	}

	s.mu.Lock()
	s.workers[req.ID] = &Worker{
		ID:           req.ID,
		Addr:         req.Addr,
		CompilerPath: req.CompilerPath,
		Capacity:     req.Capacity,
		LastSeen:     time.Now(),
	}
	s.mu.Unlock()

	slog.Info("scheduler: worker registered", "id", req.ID, "addr", req.Addr, "capacity", req.Capacity)
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req HeartbeatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	worker, ok := s.workers[req.ID]
	if ok {
		worker.LastSeen = time.Now()
		worker.Running = req.Running
	}
	s.mu.Unlock()

	if !ok {
		http.Error(w, "worker not registered", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAcquireWorker picks a worker with available capacity and reserves a slot.
func (s *Server) handleAcquireWorker(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	worker := s.pickWorker()
	if worker != nil {
		worker.Running++
	}
	s.mu.Unlock()

	if worker == nil {
		http.Error(w, "no available workers", http.StatusServiceUnavailable)
		return
	}

	writeJSON(w, http.StatusOK, AcquireWorkerResponse{
		WorkerAddr: worker.Addr,
		TaskToken:  worker.ID, // simple token = worker ID for now
	})
}

// handleReleaseWorker decrements a worker's running count.
func (s *Server) handleReleaseWorker(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workerID := r.URL.Query().Get("id")
	if workerID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if worker, ok := s.workers[workerID]; ok {
		if worker.Running > 0 {
			worker.Running--
		}
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pickWorker selects a worker with available capacity using least-loaded strategy.
// Must be called with s.mu held.
func (s *Server) pickWorker() *Worker {
	var best *Worker
	for _, w := range s.workers {
		if w.Running >= w.Capacity {
			continue
		}
		if best == nil || w.Running < best.Running {
			best = w
		}
	}
	return best
}

// evictLoop removes workers that have not sent a heartbeat recently.
func (s *Server) evictLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, w := range s.workers {
			if now.Sub(w.LastSeen) > workerHeartbeatTimeout {
				slog.Info("scheduler: evicting stale worker", "id", id, "last_seen", w.LastSeen)
				delete(s.workers, id)
			}
		}
		s.mu.Unlock()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
