package locald

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
	"yadcc-go/internal/buildinfo"
	"yadcc-go/internal/cache"
	"yadcc-go/internal/compiler"
	"yadcc-go/internal/compress"
	"yadcc-go/internal/taskgroup"

	"google.golang.org/grpc"
)

// Server is the local daemon HTTP server (wrapper-facing).
type Server struct {
	Addr             string
	SchedulerAddr    string // gRPC addr of scheduler, e.g. "127.0.0.1:8336"
	CacheAddr        string // optional gRPC addr of yadcc-cache, e.g. "127.0.0.1:8338"
	MaxLocalParallel int

	once            sync.Once
	store           cache.Store
	tg              taskgroup.Group[compileResult]
	sem             chan struct{}
	schedulerConn   *grpc.ClientConn
	schedulerClient pb.SchedulerServiceClient
	cacheConn       *grpc.ClientConn
	cacheClient     pb.CacheServiceClient // nil when no external cache configured
}

type compileResult struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	ObjectFile []byte
	CacheHit   bool
}

// SubmitRequest is the JSON body sent by the wrapper.
type SubmitRequest struct {
	CompilerPath       string   `json:"compiler_path"`
	Args               []string `json:"args"`
	Language           string   `json:"language"`
	PreprocessedSource []byte   `json:"preprocessed_source"`
	OutputFile         string   `json:"output_file"`
}

// SubmitResponse is the JSON response to the wrapper.
type SubmitResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	ObjectFile []byte `json:"object_file"`
	CacheHit   bool   `json:"cache_hit"`
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.MaxLocalParallel <= 0 {
			s.MaxLocalParallel = 8
		}
		s.sem = make(chan struct{}, s.MaxLocalParallel)
		s.store = cache.NewMemoryStore()

		if s.SchedulerAddr != "" {
			conn, err := grpc.NewClient(s.SchedulerAddr,
				grpc.WithInsecure()) //nolint:staticcheck
			if err != nil {
				slog.Warn("locald: failed to connect to scheduler", "error", err)
			} else {
				s.schedulerConn = conn
				s.schedulerClient = pb.NewSchedulerServiceClient(conn)
			}
		}

		if s.CacheAddr != "" {
			conn, err := grpc.NewClient(s.CacheAddr,
				grpc.WithInsecure(), //nolint:staticcheck
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(256<<20),
					grpc.MaxCallSendMsgSize(256<<20),
				))
			if err != nil {
				slog.Warn("locald: failed to connect to external cache", "error", err)
			} else {
				s.cacheConn = conn
				s.cacheClient = pb.NewCacheServiceClient(conn)
				slog.Info("locald: connected to external cache", "addr", s.CacheAddr)
			}
		}
	})
}

func (s *Server) ListenAndServe() error {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/local/get_version", handleVersion)
	mux.HandleFunc("/local/submit_task", s.handleSubmitTask)
	return http.ListenAndServe(s.Addr, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
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

	cacheKey := buildCacheKey(req)

	result := s.tg.Do(cacheKey, func() (compileResult, error) {
		return s.execute(req, cacheKey)
	})

	if result.Err != nil {
		slog.Warn("locald: compile task failed", "error", result.Err)
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

// execute: L1 memory cache -> external gRPC cache -> remote gRPC -> local fallback.
func (s *Server) execute(req SubmitRequest, cacheKey string) (compileResult, error) {
	// L1: in-process memory cache.
	if cached, err := s.store.Get(cacheKey); err == nil {
		slog.Debug("locald: L1 cache hit", "key", cacheKey)
		return compileResult{ExitCode: 0, ObjectFile: cached, CacheHit: true}, nil
	}

	// L2: external gRPC cache.
	if s.cacheClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := s.cacheClient.TryGetEntry(ctx, &pb.TryGetEntryRequest{
			Token: "locald",
			Key:   cacheKey,
		})
		cancel()
		if err == nil && len(resp.Value) > 0 {
			slog.Debug("locald: L2 cache hit", "key", cacheKey)
			_ = s.store.Put(cacheKey, resp.Value) // populate L1
			return compileResult{ExitCode: 0, ObjectFile: resp.Value, CacheHit: true}, nil
		}
	}

	var result compileResult
	var err error

	if s.schedulerClient != nil {
		result, err = s.tryRemoteGRPC(req)
		if err != nil {
			slog.Warn("locald: remote failed, falling back to local", "error", err)
		}
	}
	if err != nil || s.schedulerClient == nil {
		result, err = s.localCompile(req)
		if err != nil {
			return compileResult{}, err
		}
	}

	// Store successful result in L1 and (asynchronously) L2.
	if result.ExitCode == 0 && len(result.ObjectFile) > 0 {
		if putErr := s.store.Put(cacheKey, result.ObjectFile); putErr != nil {
			slog.Warn("locald: L1 cache put failed", "error", putErr)
		}
		if s.cacheClient != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = s.cacheClient.PutEntry(ctx, &pb.PutEntryRequest{
					Token: "locald",
					Key:   cacheKey,
					Value: result.ObjectFile,
				})
			}()
		}
	}

	return result, nil
}

// tryRemoteGRPC acquires a worker grant and dispatches compilation.
func (s *Server) tryRemoteGRPC(req SubmitRequest) (compileResult, error) {
	// Give the scheduler up to 5 s to find an available worker before we
	// fall back to local compilation.  The gRPC context deadline must be
	// longer than MillisecondsToWait so the RPC itself doesn't time out first.
	const waitForWorkerMs = 5_000
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	grantResp, err := s.schedulerClient.WaitForStartingTask(ctx, &pb.WaitForStartingTaskRequest{
		ImmediateRequests:  1,
		MillisecondsToWait: waitForWorkerMs,
	})
	if err != nil {
		return compileResult{}, fmt.Errorf("acquire worker: %w", err)
	}
	if len(grantResp.Grants) == 0 {
		return compileResult{}, fmt.Errorf("no worker grants returned")
	}
	grant := grantResp.Grants[0]

	workerConn, err := grpc.NewClient(grant.ServantLocation,
		grpc.WithInsecure(), //nolint:staticcheck
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256<<20),
			grpc.MaxCallSendMsgSize(256<<20),
		))
	if err != nil {
		s.freeGrant(grant.TaskGrantId)
		return compileResult{}, fmt.Errorf("dial worker %s: %w", grant.ServantLocation, err)
	}
	defer workerConn.Close()

	workerClient := pb.NewRemoteDaemonServiceClient(workerConn)

	queueCtx, queueCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer queueCancel()

	compilerDigest, _ := compiler.Digest(req.CompilerPath)

	queueResp, err := workerClient.QueueCxxCompilationTask(queueCtx, &pb.QueueCxxCompilationTaskRequest{
		Token:       "locald",
		TaskGrantId: grant.TaskGrantId,
		Environment: &pb.EnvironmentDesc{
			CompilerDigest: compilerDigest,
		},
		CompilerArguments:      req.Args,
		ZstdPreprocessedSource: compress.Compress(req.PreprocessedSource),
	})
	if err != nil {
		s.freeGrant(grant.TaskGrantId)
		return compileResult{}, fmt.Errorf("queue task: %w", err)
	}

	// Poll for result.
	for {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 65*time.Second)
		waitResp, waitErr := workerClient.WaitForCompilationOutput(waitCtx, &pb.WaitForCompilationOutputRequest{
			Token:              "locald",
			TaskId:             queueResp.TaskId,
			MillisecondsToWait: 60000,
		})
		waitCancel()

		if waitErr != nil {
			s.freeGrant(grant.TaskGrantId)
			return compileResult{}, fmt.Errorf("wait for output: %w", waitErr)
		}

		switch waitResp.Status {
		case pb.WaitForCompilationOutputResponse_STATUS_RUNNING:
			continue
		case pb.WaitForCompilationOutputResponse_STATUS_NOT_FOUND:
			s.freeGrant(grant.TaskGrantId)
			return compileResult{}, fmt.Errorf("task not found on worker")
		default: // STATUS_DONE
			s.freeGrant(grant.TaskGrantId)
			freeCtx, freeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = workerClient.FreeRemoteTask(freeCtx, &pb.FreeRemoteTaskRequest{
				Token:  "locald",
				TaskId: queueResp.TaskId,
			})
			freeCancel()

			var objBytes []byte
			if len(waitResp.Outputs) > 0 {
				objBytes = waitResp.Outputs[0].Data
			}
			return compileResult{
				ExitCode:   int(waitResp.ExitCode),
				Stdout:     waitResp.Stdout,
				Stderr:     waitResp.Stderr,
				ObjectFile: objBytes,
			}, nil
		}
	}
}

// freeGrant releases a task grant back to the scheduler.
func (s *Server) freeGrant(grantID uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.schedulerClient.FreeTask(ctx, &pb.FreeTaskRequest{
		Token:        "locald",
		TaskGrantIds: []uint64{grantID},
	})
}

// localCompile runs the compiler locally (semaphore-limited).
func (s *Server) localCompile(req SubmitRequest) (compileResult, error) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	ext := ".i"
	if req.Language == "c++" {
		ext = ".ii"
	}
	tmpFile, err := createTempFile("yadcc-*"+ext, req.PreprocessedSource)
	if err != nil {
		return compileResult{}, fmt.Errorf("write temp source: %w", err)
	}
	defer removeFile(tmpFile)

	args := buildLocalArgs(req.Args, req.Language, tmpFile, req.OutputFile)

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

	var objBytes []byte
	if exitCode == 0 {
		objBytes, _ = readFile(req.OutputFile)
	}

	return compileResult{
		ExitCode:   exitCode,
		Stdout:     outBuf.Bytes(),
		Stderr:     errBuf.Bytes(),
		ObjectFile: objBytes,
	}, nil
}

// buildCacheKey produces a deterministic cache key for a compile request.
//
// It hashes:
//   - The compiler binary digest (so different compilers never share cache entries).
//   - The source language.
//   - Normalized compiler arguments (debug/path/dependency flags that affect
//     reproducibility are stripped; only flags that influence the object file
//     content are kept).
//   - The SHA-256 of the preprocessed source.
//
// A best-effort approach is used for the compiler digest: if the binary cannot
// be hashed (e.g. not found), the path itself is used as a fallback so that
// compilation can still proceed — just without cross-compiler cache sharing.
func buildCacheKey(req SubmitRequest) string {
	compilerDigest, err := compiler.Digest(req.CompilerPath)
	if err != nil {
		slog.Warn("locald: could not hash compiler binary, using path as fallback",
			"path", req.CompilerPath, "error", err)
		compilerDigest = req.CompilerPath
	}

	h := sha256.New()
	fmt.Fprintf(h, "compiler:%s\n", compilerDigest)
	fmt.Fprintf(h, "lang:%s\n", req.Language)
	for _, a := range normalizeArgs(req.Args) {
		fmt.Fprintf(h, "arg:%s\n", a)
	}
	fmt.Fprintf(h, "source:%x\n", sha256.Sum256(req.PreprocessedSource))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeArgs removes flags that do not affect the compiled object file
// content so that the cache key is stable across invocations with differing
// -o paths, dependency-file names, or debug-prefix-map values.
func normalizeArgs(args []string) []string {
	skipNext := false
	var out []string
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch a {
		case "-o", "-MF", "-MT", "-MQ":
			skipNext = true
		case "-MD", "-MMD", "-MP", "-MG":
			// drop
		default:
			out = append(out, a)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
