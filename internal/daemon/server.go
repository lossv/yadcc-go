// Package daemon implements the yadcc unified daemon.
//
// A single yadcc-daemon process serves both roles simultaneously:
//
//  1. Local role (port 8334, HTTP, loopback-only): accepts compilation tasks
//     from the yadcc wrapper on the same machine, preprocesses them, submits
//     to remote workers via the scheduler, falls back to local execution.
//
//  2. Servant role (port 8335, gRPC, network-accessible): accepts remote
//     compilation tasks dispatched by other daemons and executes them with
//     the locally-installed compiler.
//
// The --servant_priority flag controls how aggressively the machine volunteers
// CPU resources for remote tasks:
//   - "user"      (default): up to 40% of logical CPUs for remote tasks.
//   - "dedicated": up to 95% of logical CPUs for remote tasks.
package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
	"yadcc-go/internal/auth"
	"yadcc-go/internal/buildinfo"
	"yadcc-go/internal/cache"
	"yadcc-go/internal/compiler"
	"yadcc-go/internal/compress"
	"yadcc-go/internal/metrics"
	"yadcc-go/internal/objpatch"
	"yadcc-go/internal/sysinfo"
	"yadcc-go/internal/taskgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ServantPriority controls CPU resource allocation for remote compile tasks.
type ServantPriority int

const (
	// ServantPriorityUser limits remote tasks to ~40% of logical CPUs.
	// Use this on developer workstations.
	ServantPriorityUser ServantPriority = iota
	// ServantPriorityDedicated allocates ~95% of logical CPUs to remote tasks.
	// Use this on build-farm machines with no local user workloads.
	ServantPriorityDedicated
)

// Server is the unified yadcc daemon.  It must be initialised via ListenAndServe.
type Server struct {
	// LocalAddr is the HTTP listen address for the wrapper-facing API.
	// Default: "127.0.0.1:8334"
	LocalAddr string
	// ServantAddr is the gRPC listen address for remote compilation.
	// Default: "0.0.0.0:8335"
	ServantAddr string
	// SchedulerAddr is the gRPC address of the scheduler.
	// Leave empty to disable distributed compilation.
	SchedulerAddr string
	// CacheAddr is the optional gRPC address of the yadcc-cache service.
	// Leave empty to use only the in-process L1 memory cache.
	CacheAddr string
	// Token is the authentication token sent to scheduler and cache.
	Token string
	// UserTokens is the whitelist of accepted user tokens (from wrappers).
	// Empty means accept all (open mode).
	UserTokens []string
	// ServantTokens is the whitelist of accepted servant tokens (from remote workers).
	// Empty means accept all (open mode).
	ServantTokens []string
	// ServantPriority controls how many CPUs are given to remote tasks.
	ServantPriority ServantPriority
	// WorkerID uniquely identifies this daemon (defaults to hostname:port).
	WorkerID string

	// Registry scans for locally available compilers.  If nil a default
	// registry is created.
	Registry *compiler.Registry

	// --- local (HTTP) side ---
	initOnce        sync.Once
	store           cache.Store
	tg              taskgroup.Group[compileResult]
	sem             chan struct{} // limits concurrent local fallback compiles
	ppSem           chan struct{} // limits concurrent preprocessing (P1-7)
	schedulerConn   *grpc.ClientConn
	schedulerClient pb.SchedulerServiceClient
	cacheConn       *grpc.ClientConn
	cacheClient     pb.CacheServiceClient
	// bloomMu guards bloomFilter below.
	bloomMu        sync.RWMutex
	bloomFilter    *cache.BloomFilter // may be nil if no external cache
	bloomLastFetch time.Time
	verifier       auth.Verifier

	// --- servant (gRPC) side ---
	pb.UnimplementedRemoteDaemonServiceServer
	nextTaskID atomic.Uint64
	tasksMu    sync.Mutex
	tasks      map[uint64]*taskRecord
	servantSem chan struct{} // limits concurrent remote compile tasks
}

type compileResult struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	ObjectFile []byte
	Outputs    []*pb.FileBlob
	CacheHit   bool
}

type taskRecord struct {
	mu     sync.Mutex
	done   chan struct{}
	result *pb.WaitForCompilationOutputResponse
}

// SubmitRequest is the JSON body posted by the wrapper to /local/submit_task.
type SubmitRequest struct {
	CompilerPath       string   `json:"compiler_path"`
	Args               []string `json:"args"`
	Language           string   `json:"language"`
	PreprocessedSource []byte   `json:"preprocessed_source"`
	OutputFile         string   `json:"output_file"`
	// SourcePath is the original source file path (before preprocessing).
	// It is used for path-rewriting in remote object files so debug info
	// points to the correct local path rather than the remote temp file.
	SourcePath string `json:"source_path,omitempty"`
	// Token is the authentication token presented by the wrapper.
	// The daemon validates it against the configured UserTokens whitelist.
	Token string `json:"token,omitempty"`
}

// SubmitResponse is the JSON response to the wrapper.
type SubmitResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	ObjectFile []byte `json:"object_file"`
	CacheHit   bool   `json:"cache_hit"`
}

// ---------- startup ----------

func (s *Server) init() {
	s.initOnce.Do(func() {
		// Set up compiler registry.
		if s.Registry == nil {
			s.Registry = &compiler.Registry{}
		}
		s.Registry.Start()

		// Worker ID.
		if s.WorkerID == "" {
			hostname, _ := os.Hostname()
			port := portOf(s.servantAddr())
			s.WorkerID = fmt.Sprintf("%s:%s", hostname, port)
		}

		// Semaphore for local fallback compilations.
		s.sem = make(chan struct{}, s.maxLocalParallel())
		// Semaphore for preprocessing (P1-7): at most NumCPU concurrent.
		s.ppSem = make(chan struct{}, runtime.NumCPU())

		// Configure token auth.
		s.verifier.SetUserTokens(s.UserTokens)
		s.verifier.SetServantTokens(s.ServantTokens)

		// In-process L1 cache.
		s.store = cache.NewMemoryStore()

		// Task map for servant side.
		s.tasks = make(map[uint64]*taskRecord) // Servant semaphore: limits concurrent remote compile tasks.
		cap := s.capacity()
		if cap < 1 {
			cap = 1
		}
		s.servantSem = make(chan struct{}, cap)

		// Scheduler gRPC connection.
		if s.SchedulerAddr != "" {
			conn, err := grpc.NewClient(s.SchedulerAddr,
				grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				slog.Warn("daemon: failed to connect to scheduler", "error", err)
			} else {
				s.schedulerConn = conn
				s.schedulerClient = pb.NewSchedulerServiceClient(conn)
			}
		}

		// External cache gRPC connection.
		if s.CacheAddr != "" {
			conn, err := grpc.NewClient(s.CacheAddr,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(256<<20),
					grpc.MaxCallSendMsgSize(256<<20),
				))
			if err != nil {
				slog.Warn("daemon: failed to connect to external cache", "error", err)
			} else {
				s.cacheConn = conn
				s.cacheClient = pb.NewCacheServiceClient(conn)
				slog.Info("daemon: connected to external cache", "addr", s.CacheAddr)
				// Start background bloom filter refresh.
				go s.bloomRefreshLoop()
			}
		}
	})
}

// ListenAndServe starts both the local HTTP server and the servant gRPC server,
// registers with the scheduler, and blocks until the HTTP server stops.
func (s *Server) ListenAndServe() error {
	s.init()

	// Start the servant gRPC server in a goroutine.
	go func() {
		if err := s.serveGRPC(); err != nil {
			slog.Error("daemon: servant gRPC stopped", "error", err)
		}
	}()

	// Register with the scheduler and start heartbeat.
	if s.schedulerClient != nil {
		if err := s.sendHeartbeat(); err != nil {
			slog.Warn("daemon: initial heartbeat failed", "error", err)
		}
		go s.heartbeatLoop()
		go s.configKeeperLoop()
	}

	// Serve the local HTTP API (wrapper-facing).
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/local/get_version", s.handleVersion)
	mux.HandleFunc("/local/submit_task", s.handleSubmitTask)
	mux.Handle("/metrics", metrics.Handler())
	slog.Info("daemon: local HTTP listening", "addr", s.localAddr(),
		"servant", s.servantAddr(), "scheduler", s.SchedulerAddr,
		"worker_id", s.WorkerID, "version", buildinfo.String())
	return http.ListenAndServe(s.localAddr(), mux)
}

func (s *Server) serveGRPC() error {
	addr := s.servantAddr()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("daemon: servant listen %s: %w", addr, err)
	}
	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(256<<20),
		grpc.MaxSendMsgSize(256<<20),
	)
	pb.RegisterRemoteDaemonServiceServer(gs, s)
	slog.Info("daemon: servant gRPC listening", "addr", addr)
	return gs.Serve(lis)
}

// ---------- local HTTP handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": buildinfo.String()})
}

func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req SubmitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Verify user token.  Fall back to X-Yadcc-Token header if body omits it.
	tok := req.Token
	if tok == "" {
		tok = r.Header.Get("X-Yadcc-Token")
	}
	if !s.verifier.VerifyUser(tok) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	cacheKey := buildCacheKey(req)

	// P1-7: limit concurrent in-flight compile tasks on the local daemon to
	// avoid overloading the host when many wrapper processes submit at once.
	// We use ppSem (sized to NumCPU) as the overall pipeline gate.
	select {
	case s.ppSem <- struct{}{}:
		defer func() { <-s.ppSem }()
	default:
		// Overloaded: fall back to local execution without the task-group
		// deduplication, so the client can proceed immediately.
		res, err := s.localCompile(req)
		if err != nil {
			slog.Warn("daemon: local compile failed", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, SubmitResponse{
			ExitCode: res.ExitCode, Stdout: res.Stdout,
			Stderr: res.Stderr, ObjectFile: res.ObjectFile,
		})
		return
	}

	result := s.tg.Do(cacheKey, func() (compileResult, error) {
		return s.execute(req, cacheKey)
	})

	if result.Err != nil {
		slog.Warn("daemon: compile task failed", "error", result.Err)
		http.Error(w, result.Err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, SubmitResponse{
		ExitCode:   result.Value.ExitCode,
		Stdout:     result.Value.Stdout,
		Stderr:     result.Value.Stderr,
		ObjectFile: result.Value.ObjectFile,
		CacheHit:   result.Value.CacheHit,
	})
}

// ---------- execution pipeline ----------

func (s *Server) execute(req SubmitRequest, cacheKey string) (compileResult, error) {
	// Determine up-front whether the result can be cached.
	// Sources that use __TIME__ / __DATE__ / __TIMESTAMP__ produce
	// non-deterministic output; we must never serve stale object files for them.
	cacheable := isCacheable(req.Args, req.PreprocessedSource)

	// L1: in-process memory cache (only for cacheable tasks).
	if cacheable {
		if data, err := s.store.Get(cacheKey); err == nil {
			if entry, err := cache.UnmarshalEntry(data); err == nil {
				slog.Debug("daemon: L1 cache hit", "key", cacheKey[:8])
				metrics.DaemonTasksTotal.WithLabelValues("cache_hit_l1").Inc()
				return compileResult{
					ExitCode: int(entry.ExitCode), Stdout: entry.Stdout,
					Stderr: entry.Stderr, ObjectFile: entry.ObjectFile,
					Outputs:  []*pb.FileBlob{{Name: "output.o", Data: entry.ObjectFile}},
					CacheHit: true,
				}, nil
			}
		}
	}

	// L2: external gRPC cache (only for cacheable tasks).
	if cacheable && s.cacheClient != nil && s.bloomMayContain(cacheKey) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := s.cacheClient.TryGetEntry(ctx, &pb.TryGetEntryRequest{
			Token: s.token(),
			Key:   cacheKey,
		})
		cancel()
		if err == nil && len(resp.Value) > 0 {
			if entry, err := cache.UnmarshalEntry(resp.Value); err == nil {
				slog.Debug("daemon: L2 cache hit", "key", cacheKey[:8])
				metrics.DaemonTasksTotal.WithLabelValues("cache_hit_l2").Inc()
				_ = s.store.Put(cacheKey, resp.Value)
				return compileResult{
					ExitCode: int(entry.ExitCode), Stdout: entry.Stdout,
					Stderr: entry.Stderr, ObjectFile: entry.ObjectFile,
					Outputs:  []*pb.FileBlob{{Name: "output.o", Data: entry.ObjectFile}},
					CacheHit: true,
				}, nil
			}
		}
	}

	// Try remote compilation.
	var result compileResult
	var err error
	if s.schedulerClient != nil {
		t := time.Now()
		result, err = s.tryRemote(req)
		if err != nil {
			slog.Warn("daemon: remote compile failed, falling back to local", "error", err)
		} else {
			metrics.DaemonRemoteLatencySeconds.Observe(time.Since(t).Seconds())
			metrics.DaemonTasksTotal.WithLabelValues("remote").Inc()
		}
	}
	if err != nil || s.schedulerClient == nil {
		t := time.Now()
		result, err = s.localCompile(req)
		if err != nil {
			metrics.DaemonTasksTotal.WithLabelValues("error").Inc()
			return compileResult{}, err
		}
		metrics.DaemonLocalLatencySeconds.Observe(time.Since(t).Seconds())
		metrics.DaemonTasksTotal.WithLabelValues("local").Inc()
	}

	// Write back to caches (only when the result is deterministic).
	if cacheable && result.ExitCode == 0 && len(result.ObjectFile) > 0 {
		entryBytes := cache.MarshalEntry(cache.Entry{
			ExitCode:   int32(result.ExitCode),
			Stdout:     result.Stdout,
			Stderr:     result.Stderr,
			ObjectFile: result.ObjectFile,
		})
		_ = s.store.Put(cacheKey, entryBytes)
		if s.cacheClient != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = s.cacheClient.PutEntry(ctx, &pb.PutEntryRequest{
					Token: s.token(),
					Key:   cacheKey,
					Value: entryBytes,
				})
			}()
		}
	}
	if !cacheable {
		slog.Debug("daemon: skipping cache write for non-cacheable task (timestamp macros detected)")
	}
	return result, nil
}

// tryRemote acquires a worker grant from the scheduler and dispatches the task.
// On worker failure it retries up to maxRemoteRetries times before giving up.
func (s *Server) tryRemote(req SubmitRequest) (compileResult, error) {
	const maxRemoteRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRemoteRetries; attempt++ {
		result, err := s.tryRemoteOnce(req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		slog.Debug("daemon: remote attempt failed, retrying",
			"attempt", attempt+1, "max", maxRemoteRetries+1, "error", err)
	}
	return compileResult{}, fmt.Errorf("remote compile failed after %d attempts: %w", maxRemoteRetries+1, lastErr)
}

// tryRemoteOnce performs a single attempt to compile remotely.
func (s *Server) tryRemoteOnce(req SubmitRequest) (compileResult, error) {
	const waitForWorkerMs = 5_000
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	env := compiler.EnvironmentForCompiler(req.CompilerPath, compiler.Parse(req.Args))

	var grant *pb.StartingTaskGrant
	grantResp, err := s.schedulerClient.WaitForStartingTask(ctx, &pb.WaitForStartingTaskRequest{
		Token:              s.token(),
		ImmediateRequests:  1,
		MillisecondsToWait: waitForWorkerMs,
		RequesterLocation:  s.servantAddr(),
		Environment:        env,
	})
	if err != nil {
		return compileResult{}, fmt.Errorf("acquire worker: %w", err)
	}
	if len(grantResp.Grants) == 0 {
		return compileResult{}, fmt.Errorf("no worker grants returned")
	}
	grant = grantResp.Grants[0]

	workerConn, err := grpc.NewClient(grant.ServantLocation,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256<<20),
			grpc.MaxCallSendMsgSize(256<<20),
		))
	if err != nil {
		s.freeGrant(grant.TaskGrantId)
		return compileResult{}, fmt.Errorf("dial worker %s: %w", grant.ServantLocation, err)
	}
	defer workerConn.Close()

	wc := pb.NewRemoteDaemonServiceClient(workerConn)

	queueCtx, queueCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer queueCancel()

	queueResp, err := wc.QueueCxxCompilationTask(queueCtx, &pb.QueueCxxCompilationTaskRequest{
		Token:                  s.token(),
		TaskGrantId:            grant.TaskGrantId,
		Environment:            env,
		SourcePath:             req.SourcePath,
		CompilerArguments:      req.Args,
		ZstdPreprocessedSource: compress.Compress(req.PreprocessedSource),
	})
	if err != nil {
		s.freeGrant(grant.TaskGrantId)
		return compileResult{}, fmt.Errorf("queue task: %w", err)
	}

	// Keep the scheduler grant alive while we wait for the compilation result.
	// Without this, long compilations will have their grant expired by the
	// scheduler, which would free up the worker slot before we're done.
	keepAliveCtx, keepAliveCancel := context.WithCancel(context.Background())
	defer keepAliveCancel()
	go s.keepGrantAlive(keepAliveCtx, grant.TaskGrantId)

	for {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 65*time.Second)
		waitResp, waitErr := wc.WaitForCompilationOutput(waitCtx, &pb.WaitForCompilationOutputRequest{
			Token:              s.token(),
			TaskId:             queueResp.TaskId,
			MillisecondsToWait: 60000,
		})
		waitCancel()
		if waitErr != nil {
			s.freeGrant(grant.TaskGrantId)
			return compileResult{}, fmt.Errorf("wait output: %w", waitErr)
		}
		switch waitResp.Status {
		case pb.WaitForCompilationOutputResponse_STATUS_RUNNING:
			continue
		case pb.WaitForCompilationOutputResponse_STATUS_NOT_FOUND:
			s.freeGrant(grant.TaskGrantId)
			return compileResult{}, fmt.Errorf("task not found on worker")
		default:
			s.freeGrant(grant.TaskGrantId)
			freeCtx, freeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = wc.FreeRemoteTask(freeCtx, &pb.FreeRemoteTaskRequest{
				Token: s.token(), TaskId: queueResp.TaskId,
			})
			freeCancel()
			obj := primaryObject(waitResp.Outputs)
			return compileResult{
				ExitCode:   int(waitResp.ExitCode),
				Stdout:     waitResp.Stdout,
				Stderr:     waitResp.Stderr,
				ObjectFile: obj,
				Outputs:    waitResp.Outputs,
			}, nil
		}
	}
}

func (s *Server) freeGrant(grantID uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.schedulerClient.FreeTask(ctx, &pb.FreeTaskRequest{
		Token:        s.token(),
		TaskGrantIds: []uint64{grantID},
	})
}

// keepGrantAlive sends KeepTaskAlive RPCs to the scheduler every 30 seconds
// until ctx is cancelled.  This prevents the scheduler from expiring the grant
// during long compilations.
func (s *Server) keepGrantAlive(ctx context.Context, grantID uint64) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			kaCtx, kaCancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := s.schedulerClient.KeepTaskAlive(kaCtx, &pb.KeepTaskAliveRequest{
				Token:        s.token(),
				TaskGrantIds: []uint64{grantID},
			})
			kaCancel()
			if err != nil {
				slog.Warn("daemon: KeepTaskAlive failed", "grant", grantID, "error", err)
				return
			}
			if len(resp.Statuses) > 0 && !resp.Statuses[0] {
				// Grant already expired on the scheduler side.
				slog.Warn("daemon: grant expired during long compile", "grant", grantID)
				return
			}
		}
	}
}

// localCompile runs the compiler on this machine (semaphore-limited).
func (s *Server) localCompile(req SubmitRequest) (compileResult, error) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	ext := ".i"
	if req.Language == "c++" {
		ext = ".ii"
	}
	tmpFile, err := os.CreateTemp("", "yadcc-*"+ext)
	if err != nil {
		return compileResult{}, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.Write(req.PreprocessedSource); err != nil {
		tmpFile.Close()
		return compileResult{}, fmt.Errorf("write temp: %w", err)
	}
	tmpFile.Close()

	args := buildLocalArgs(req.Args, req.Language, tmpPath, req.OutputFile)
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(req.CompilerPath, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	exitCode := 0
	if runErr := cmd.Run(); runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return compileResult{}, fmt.Errorf("run compiler: %w", runErr)
		}
	}

	var obj []byte
	if exitCode == 0 {
		obj, _ = os.ReadFile(req.OutputFile)
	}
	return compileResult{
		ExitCode: exitCode, Stdout: outBuf.Bytes(),
		Stderr: errBuf.Bytes(), ObjectFile: obj,
		Outputs: []*pb.FileBlob{{Name: "output.o", Data: obj}},
	}, nil
}

// ---------- servant gRPC (RemoteDaemonServiceServer) ----------

func (s *Server) QueueCxxCompilationTask(_ context.Context, req *pb.QueueCxxCompilationTaskRequest) (*pb.QueueCxxCompilationTaskResponse, error) {
	if !s.verifier.VerifyAny(req.Token) {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	// Enforce capacity limit — reject immediately if all slots are taken.
	select {
	case s.servantSem <- struct{}{}:
	default:
		return nil, status.Error(codes.ResourceExhausted, "servant at capacity")
	}

	id := s.nextTaskID.Add(1)
	rec := &taskRecord{done: make(chan struct{})}

	s.tasksMu.Lock()
	s.tasks[id] = rec
	s.tasksMu.Unlock()

	go func() {
		defer func() { <-s.servantSem }()
		rec.mu.Lock()
		rec.result = s.runCompile(req)
		close(rec.done)
		rec.mu.Unlock()
	}()
	return &pb.QueueCxxCompilationTaskResponse{TaskId: id}, nil
}

func (s *Server) WaitForCompilationOutput(_ context.Context, req *pb.WaitForCompilationOutputRequest) (*pb.WaitForCompilationOutputResponse, error) {
	s.tasksMu.Lock()
	rec, ok := s.tasks[req.TaskId]
	s.tasksMu.Unlock()
	if !ok {
		return &pb.WaitForCompilationOutputResponse{
			Status: pb.WaitForCompilationOutputResponse_STATUS_NOT_FOUND,
		}, nil
	}

	timeout := time.Duration(req.MillisecondsToWait) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	select {
	case <-rec.done:
		rec.mu.Lock()
		result := rec.result
		rec.mu.Unlock()
		return result, nil
	case <-time.After(timeout):
		return &pb.WaitForCompilationOutputResponse{
			Status: pb.WaitForCompilationOutputResponse_STATUS_RUNNING,
		}, nil
	}
}

func (s *Server) ReferenceTask(_ context.Context, req *pb.ReferenceTaskRequest) (*pb.ReferenceTaskResponse, error) {
	s.tasksMu.Lock()
	_, ok := s.tasks[req.TaskId]
	s.tasksMu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %d not found", req.TaskId)
	}
	return &pb.ReferenceTaskResponse{}, nil
}

func (s *Server) FreeRemoteTask(_ context.Context, req *pb.FreeRemoteTaskRequest) (*pb.FreeRemoteTaskResponse, error) {
	s.tasksMu.Lock()
	delete(s.tasks, req.TaskId)
	s.tasksMu.Unlock()
	return &pb.FreeRemoteTaskResponse{}, nil
}

// runCompile executes a compilation on behalf of a remote caller.
// On success, the result is asynchronously written to the distributed cache.
func (s *Server) runCompile(req *pb.QueueCxxCompilationTaskRequest) *pb.WaitForCompilationOutputResponse {
	// Decompress preprocessed source.
	srcBytes, err := compress.Decompress(req.ZstdPreprocessedSource)
	if err != nil {
		return errResp("decompress: " + err.Error())
	}

	lang := inferLang(req.CompilerArguments)
	ext := ".i"
	if lang == "c++" || lang == "c++-cpp-output" {
		ext = ".ii"
	}

	// Resolve the compiler.  The request carries a CompilerDigest; we look up
	// the matching binary from our local registry.  If not found, fall back to
	// the system default compiler.
	compilerBin := s.resolveCompilerForRequest(req)

	tmpSrc, err := os.CreateTemp("", "yadcc-remote-*"+ext)
	if err != nil {
		return errResp("create temp src: " + err.Error())
	}
	tmpSrcPath := tmpSrc.Name()
	defer os.Remove(tmpSrcPath)
	if _, err := tmpSrc.Write(srcBytes); err != nil {
		tmpSrc.Close()
		return errResp("write temp src: " + err.Error())
	}
	tmpSrc.Close()

	outputDir, err := os.MkdirTemp("", "yadcc-remote-out-*")
	if err != nil {
		return errResp("create temp output dir: " + err.Error())
	}
	defer os.RemoveAll(outputDir)
	tmpOutPath := filepath.Join(outputDir, "output.o")

	args := buildCompileArgs(req.CompilerArguments, tmpSrcPath, tmpOutPath)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(compilerBin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()

	exitCode := int32(0)
	if runErr := cmd.Run(); runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return errResp("run compiler: " + runErr.Error())
		}
	}

	resp := &pb.WaitForCompilationOutputResponse{
		Status:   pb.WaitForCompilationOutputResponse_STATUS_DONE,
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}
	if exitCode == 0 {
		outputs, err := collectOutputs(outputDir, tmpSrcPath, req.SourcePath)
		if err != nil {
			return errResp("collect outputs: " + err.Error())
		}
		resp.Outputs = outputs

		// P0-3: Servant-side async cache write-back (distributed_cache_writer).
		// Build a cache key from the request and write the result to the
		// distributed cache so other daemons can benefit without recompiling.
		if s.cacheClient != nil && req.Environment != nil {
			go s.servantWriteCache(req, resp)
		}
	}
	return resp
}

func primaryObject(outputs []*pb.FileBlob) []byte {
	for _, out := range outputs {
		if out.Name == "output.o" || strings.HasSuffix(out.Name, ".o") || strings.HasSuffix(out.Name, ".obj") {
			return out.Data
		}
	}
	if len(outputs) > 0 {
		return outputs[0].Data
	}
	return nil
}

func collectOutputs(root, remotePath, localPath string) ([]*pb.FileBlob, error) {
	var outputs []*pb.FileBlob
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if localPath != "" {
			data = objpatch.Patch(data, remotePath, localPath)
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		outputs = append(outputs, &pb.FileBlob{
			Name: filepath.ToSlash(name),
			Data: data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outputs, nil
}

// resolveCompilerForRequest finds the local compiler binary to use for a
// remote task.  It matches the requested CompilerDigest against the registry;
// if no match is found it falls back to "cc".
func (s *Server) resolveCompilerForRequest(req *pb.QueueCxxCompilationTaskRequest) string {
	wantDigest := ""
	if req.Environment != nil {
		wantDigest = req.Environment.CompilerDigest
	}

	if wantDigest != "" && s.Registry != nil {
		if path, ok := s.Registry.PathByDigest(wantDigest); ok {
			return path
		}
		slog.Warn("daemon: no local compiler matches requested digest",
			"digest", wantDigest[:min(8, len(wantDigest))], "using", "cc")
	}
	return "cc"
}

// findCompilerByDigest walks PATH to find a compiler whose sha256 matches digest.
func (s *Server) findCompilerByDigest(digest string) string {
	// Quick scan: check all executables named like a compiler in PATH.
	for _, dir := range splitPath() {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !compiler.IsCompilerName(e.Name()) {
				continue
			}
			path := dir + "/" + e.Name()
			canonical, err := os.Readlink(path)
			if err != nil {
				canonical = path
			} else if len(canonical) == 0 || canonical[0] != '/' {
				canonical = dir + "/" + canonical
			}
			d, err := compiler.Digest(canonical)
			if err == nil && d == digest {
				return canonical
			}
		}
	}
	return ""
}

// bloomRefreshLoop periodically fetches the bloom filter from the external
// cache server so we can skip TryGetEntry when the key is definitely absent.
func (s *Server) bloomRefreshLoop() {
	// Initial fetch immediately, then every 60 seconds.
	s.fetchBloom()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.fetchBloom()
	}
}

func (s *Server) fetchBloom() {
	s.bloomMu.RLock()
	lastFetch := s.bloomLastFetch
	s.bloomMu.RUnlock()

	var secondsSince uint32
	if !lastFetch.IsZero() {
		d := time.Since(lastFetch)
		if d > 0 {
			secondsSince = uint32(d.Seconds())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := s.cacheClient.FetchBloomFilter(ctx, &pb.FetchBloomFilterRequest{
		Token:                 s.token(),
		SecondsSinceLastFetch: secondsSince,
	})
	if err != nil {
		slog.Debug("daemon: bloom filter fetch failed", "error", err)
		return
	}

	rawBytes, err := compress.Decompress(resp.BloomFilter)
	if err != nil || len(rawBytes) == 0 {
		slog.Debug("daemon: bloom filter decompress failed", "error", err)
		return
	}

	s.bloomMu.Lock()
	defer s.bloomMu.Unlock()
	if resp.Incremental && s.bloomFilter != nil {
		// Incremental update: add newly populated keys to the existing filter.
		for _, k := range resp.NewlyPopulatedKeys {
			s.bloomFilter.Add(k)
		}
	} else {
		// Full refresh.
		s.bloomFilter = cache.BloomFilterFromBytes(rawBytes, resp.NumHashes)
	}
	s.bloomLastFetch = time.Now()
}

// bloomMayContain returns true if key is possibly in the external cache, or
// true if we have no bloom filter yet (fail-open so we don't skip real hits).
func (s *Server) bloomMayContain(key string) bool {
	s.bloomMu.RLock()
	defer s.bloomMu.RUnlock()
	if s.bloomFilter == nil {
		return true
	}
	return s.bloomFilter.MayContain(key)
}

// ---------- scheduler heartbeat ----------

func (s *Server) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.sendHeartbeat(); err != nil {
			slog.Warn("daemon: heartbeat error", "error", err)
		}
	}
}

// configKeeperLoop periodically pulls dynamic cluster configuration from the
// scheduler (GetConfig RPC) and applies changes such as token whitelist updates
// and capacity overrides.  This is the Go equivalent of config_keeper.cc.
func (s *Server) configKeeperLoop() {
	// Initial pull immediately, then every 60 seconds.
	s.pullConfig()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.pullConfig()
	}
}

func (s *Server) pullConfig() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := s.schedulerClient.GetConfig(ctx, &pb.GetConfigRequest{
		Token: s.token(),
	})
	if err != nil {
		slog.Debug("daemon: GetConfig failed", "error", err)
		return
	}

	// Update token whitelist if the scheduler provides one.
	if len(resp.AcceptableUserTokens) > 0 {
		s.verifier.SetUserTokens(resp.AcceptableUserTokens)
	}
	if len(resp.AcceptableServantTokens) > 0 {
		s.verifier.SetServantTokens(resp.AcceptableServantTokens)
	}

	// Update servant capacity if the scheduler overrides it.
	if resp.MaxServantTasks > 0 {
		newCap := int(resp.MaxServantTasks)
		// Resize servantSem by draining and recreating.
		// This is a best-effort update; in-flight tasks are not affected.
		old := s.servantSem
		s.servantSem = make(chan struct{}, newCap)
		// Drain old semaphore tokens that were unused.
		for {
			select {
			case <-old:
			default:
				goto done
			}
		}
	done:
		slog.Info("daemon: servant capacity updated from scheduler",
			"capacity", newCap)
	}
	slog.Debug("daemon: config pulled from scheduler")
}

func (s *Server) sendHeartbeat() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.tasksMu.Lock()
	load := uint32(len(s.tasks))
	s.tasksMu.Unlock()

	envs := s.Registry.Environments()
	si := sysinfo.Get()

	_, err := s.schedulerClient.Heartbeat(ctx, &pb.HeartbeatRequest{
		Token:                s.WorkerID,
		Location:             s.servantAddr(),
		Capacity:             s.capacity(),
		CurrentLoad:          load,
		Environments:         envs,
		IsDedicated:          s.ServantPriority == ServantPriorityDedicated,
		TotalMemoryBytes:     si.TotalMemoryBytes,
		AvailableMemoryBytes: si.AvailableMemoryBytes,
	})
	return err
}

// ---------- helpers ----------

func (s *Server) localAddr() string {
	if s.LocalAddr != "" {
		return s.LocalAddr
	}
	return "127.0.0.1:8334"
}

func (s *Server) servantAddr() string {
	if s.ServantAddr != "" {
		return s.ServantAddr
	}
	return "0.0.0.0:8335"
}

func (s *Server) token() string {
	if s.Token != "" {
		return s.Token
	}
	return "yadcc"
}

func (s *Server) maxLocalParallel() int {
	// Default: half the logical CPUs.
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	return n
}

func (s *Server) capacity() uint32 {
	cpus := float64(runtime.NumCPU())
	switch s.ServantPriority {
	case ServantPriorityDedicated:
		return uint32(cpus * 0.95)
	default: // user
		return uint32(cpus * 0.40)
	}
}

func splitPath() []string {
	return filepath.SplitList(os.Getenv("PATH"))
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func errResp(msg string) *pb.WaitForCompilationOutputResponse {
	return &pb.WaitForCompilationOutputResponse{
		Status:   pb.WaitForCompilationOutputResponse_STATUS_DONE,
		ExitCode: 1,
		Stderr:   []byte(msg),
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// isCacheable reports whether a compilation task's output is deterministic and
// safe to store in cache.
//
// Tasks that use __TIME__, __DATE__, or __TIMESTAMP__ produce different output
// on every run and must never be cached.
//
// Exception: if all three macros are overridden by -D flags in the compiler
// arguments (e.g. -D__TIME__="redacted"), the output is deterministic.
func isCacheable(args []string, preprocessed []byte) bool {
	const macros = "__TIME__\x00__DATE__\x00__TIMESTAMP__"
	// Quick check: if all three are redefined via -D on the command line,
	// the result is always deterministic regardless of the source.
	allOverridden := true
	for _, m := range []string{"__TIME__", "__DATE__", "__TIMESTAMP__"} {
		found := false
		for _, a := range args {
			if len(a) > len(m)+2 && a[:2] == "-D" && a[2:2+len(m)] == m && a[2+len(m)] == '=' {
				found = true
				break
			}
		}
		if !found {
			allOverridden = false
			break
		}
	}
	if allOverridden {
		return true
	}
	_ = macros
	// Scan preprocessed source for timestamp macro names.
	for _, needle := range [][]byte{[]byte("__TIME__"), []byte("__DATE__"), []byte("__TIMESTAMP__")} {
		if bytes.Contains(preprocessed, needle) {
			return false
		}
	}
	return true
}

// buildCacheKey produces a stable cache key from the compile request.
// It populates the full cache.KeyInput so that cross-environment collisions are
// impossible (different target triple, sysroot, stdlib, or ABI → different key).
func buildCacheKey(req SubmitRequest) string {
	// Use the per-process digest cache to avoid rehashing the compiler binary
	// on every request (equivalent to C++ file_digest_cache.cc).
	compilerDigest, err := compiler.DigestCached(req.CompilerPath)
	if err != nil {
		compilerDigest = req.CompilerPath
	}

	parsed := compiler.Parse(req.Args)

	sourceDigest := sha256.Sum256(req.PreprocessedSource)
	env := compiler.EnvironmentForCompiler(req.CompilerPath, parsed)

	input := cache.KeyInput{
		CompilerDigest:           compilerDigest,
		CompilerKind:             env.CompilerKind,
		CompilerVersion:          env.CompilerVersion,
		HostOS:                   env.HostOs,
		HostArch:                 env.HostArch,
		TargetTriple:             env.TargetTriple,
		ObjectFormat:             env.ObjectFormat,
		ABI:                      env.Abi,
		SysrootDigest:            env.SysrootDigest,
		StdlibDigest:             env.StdlibDigest,
		NormalizedArguments:      normalizeArgs(req.Args),
		PreprocessedSourceDigest: hex.EncodeToString(sourceDigest[:]),
		OutputKind:               "object",
	}
	return cache.BuildKey(input)
}

func normalizeArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "" {
			continue
		}
		switch a {
		case "-o", "-MF", "-MT", "-MQ":
			i++
		case "-MD", "-MMD", "-MP", "-MG":
		default:
			if isJoinedOutputArg(a) || isJoinedDependencyArg(a) {
				continue
			}
			if a[0] != '-' {
				continue
			}
			out = append(out, a)
		}
	}
	return out
}

// buildLocalArgs builds compiler args for local execution from a preprocessed file.
// It uses compiler.Parse so that two-word options (e.g. -arch arm64, -isysroot ...)
// are correctly preserved; the old raw-string loop silently dropped their values.
func buildLocalArgs(originalArgs []string, lang, tmpFile, outputFile string) []string {
	parsed := compiler.Parse(originalArgs)
	var args []string
	hasOutput := false
	for _, opt := range parsed.Options() {
		switch opt.Key {
		case "-o":
			args = append(args, "-o", outputFile)
			hasOutput = true
		case "-x":
			// will be replaced by the -x cpp-output below
		case "-E", "-fdirectives-only", "-MD", "-MMD", "-MP", "-MG":
			// strip preprocessor / dependency flags
		case "-MF", "-MT", "-MQ":
			// strip dependency target/file flags (and their values)
		default:
			if opt.Joined && len(opt.Values) == 1 {
				args = append(args, opt.Key+opt.JoinSep+opt.Values[0])
			} else {
				args = append(args, opt.Key)
				args = append(args, opt.Values...)
			}
		}
	}
	if !hasOutput {
		args = append(args, "-o", outputFile)
	}
	result := make([]string, 0, len(args)+3)
	result = append(result, "-x", preprocessedLangFlag(lang))
	result = append(result, args...)
	result = append(result, tmpFile)
	return result
}

// remoteStripPrefix reports whether an option key should be stripped when
// sending args to a remote servant.  These flags reference local include paths
// that don't exist on the remote machine; the preprocessed source already
// has the headers inlined so they are not needed.
// This mirrors C++ kIgnoredArgPrefixes in compilation_saas.cc.
func remoteStripPrefix(key string) bool {
	switch key {
	case "-I", "-include", "-isystem", "-Wmissing-include-dirs",
		"-Wp,-MMD", "-Wp,-MF", "-Wp,-MD", "-Wp,-MP":
		return true
	}
	return false
}

// buildCompileArgs adapts original args for remote execution from a preprocessed file.
// Include-path flags (-I, -include, -isystem, -Wmissing-include-dirs) are
// stripped because the remote machine's filesystem layout differs; the source
// has already been preprocessed so the headers are inlined.
func buildCompileArgs(originalArgs []string, tmpSrc, tmpOut string) []string {
	parsed := compiler.Parse(originalArgs)
	lang, _ := parsed.Language()
	var args []string
	hasOutput := false
	for _, opt := range parsed.Options() {
		switch opt.Key {
		case "-o":
			args = append(args, "-o", tmpOut)
			hasOutput = true
		case "-x":
		case "-E", "-fdirectives-only", "-MD", "-MMD", "-MP", "-MG":
		case "-MF", "-MT", "-MQ":
		default:
			if remoteStripPrefix(opt.Key) {
				continue
			}
			if opt.Joined && len(opt.Values) == 1 {
				args = append(args, opt.Key+opt.JoinSep+opt.Values[0])
			} else {
				args = append(args, opt.Key)
				args = append(args, opt.Values...)
			}
		}
	}
	if !hasOutput {
		args = append(args, "-o", tmpOut)
	}
	result := make([]string, 0, len(args)+3)
	result = append(result, "-x", preprocessedLangFlag(lang))
	result = append(result, args...)
	result = append(result, tmpSrc)
	return result
}

func inferLang(args []string) string {
	for i, a := range args {
		if a == "-x" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-x") && len(a) > 2 && !strings.HasPrefix(a, "-x=") {
			return strings.TrimPrefix(a, "-x")
		}
	}
	return ""
}

func preprocessedLangFlag(lang string) string {
	if lang == "c++" || lang == "c++-cpp-output" || lang == "c++header" {
		return "c++-cpp-output"
	}
	return "cpp-output"
}

func isJoinedOutputArg(arg string) bool {
	return strings.HasPrefix(arg, "-o") && len(arg) > 2
}

func isJoinedDependencyArg(arg string) bool {
	for _, prefix := range []string{"-MF", "-MT", "-MQ"} {
		if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
			return true
		}
	}
	return false
}

// servantWriteCache computes the cache key for a remotely-compiled task and
// writes the result to the distributed cache server asynchronously.
// This is the Go equivalent of C++ distributed_cache_writer.cc.
func (s *Server) servantWriteCache(req *pb.QueueCxxCompilationTaskRequest, resp *pb.WaitForCompilationOutputResponse) {
	// Decompress preprocessed source to compute source digest.
	srcBytes, err := compress.Decompress(req.ZstdPreprocessedSource)
	if err != nil {
		slog.Debug("servant cache write: decompress failed", "error", err)
		return
	}

	parsed := compiler.Parse(req.CompilerArguments)
	sourceDigest := sha256.Sum256(srcBytes)

	// Re-use the existing cache key construction logic.
	compilerDigest := ""
	if req.Environment != nil {
		compilerDigest = req.Environment.CompilerDigest
	}

	input := cache.KeyInput{
		CompilerDigest:           compilerDigest,
		CompilerKind:             req.Environment.GetCompilerKind(),
		CompilerVersion:          req.Environment.GetCompilerVersion(),
		HostOS:                   req.Environment.GetHostOs(),
		HostArch:                 req.Environment.GetHostArch(),
		TargetTriple:             req.Environment.GetTargetTriple(),
		ObjectFormat:             req.Environment.GetObjectFormat(),
		ABI:                      req.Environment.GetAbi(),
		SysrootDigest:            req.Environment.GetSysrootDigest(),
		StdlibDigest:             req.Environment.GetStdlibDigest(),
		NormalizedArguments:      normalizeArgs(req.CompilerArguments),
		PreprocessedSourceDigest: hex.EncodeToString(sourceDigest[:]),
		OutputKind:               "object",
	}
	_ = parsed // parsed used implicitly via normalizeArgs above
	cacheKey := cache.BuildKey(input)

	// Do not cache non-deterministic results (timestamp macros).
	if !isCacheable(req.CompilerArguments, srcBytes) {
		return
	}

	obj := primaryObject(resp.Outputs)
	if len(obj) == 0 {
		return
	}

	entryBytes := cache.MarshalEntry(cache.Entry{
		ExitCode:   resp.ExitCode,
		Stdout:     resp.Stdout,
		Stderr:     resp.Stderr,
		ObjectFile: obj,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.cacheClient.PutEntry(ctx, &pb.PutEntryRequest{
		Token: s.token(),
		Key:   cacheKey,
		Value: entryBytes,
	})
	if err != nil {
		slog.Debug("servant cache write: PutEntry failed", "error", err)
	} else {
		slog.Debug("servant cache write: stored result", "key", cacheKey[:8])
		metrics.DaemonTasksTotal.WithLabelValues("servant_cache_write").Inc()
	}
}
