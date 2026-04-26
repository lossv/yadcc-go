package locald

import (
	"bytes"
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

	"yadcc-go/internal/buildinfo"
	"yadcc-go/internal/cache"
	"yadcc-go/internal/taskgroup"
)

// Server is the local daemon HTTP server.
// It receives compile tasks from the wrapper, optionally reads from cache,
// and dispatches to a remote worker via the scheduler.
type Server struct {
	Addr          string
	SchedulerAddr string // e.g. "http://host:8336"
	CacheAddr     string // e.g. "http://host:8337"  (empty = no cache)

	// concurrency limit for local compilation fallback
	MaxLocalParallel int

	once   sync.Once
	store  cache.Store
	tg     taskgroup.Group[compileResult]
	sem    chan struct{} // local parallel limit
}

type compileResult struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	ObjectFile []byte
	CacheHit   bool
}

// SubmitRequest mirrors client.SubmitRequest (copied here to avoid import cycle).
type SubmitRequest struct {
	CompilerPath       string   `json:"compiler_path"`
	Args               []string `json:"args"`
	Language           string   `json:"language"`
	PreprocessedSource []byte   `json:"preprocessed_source"`
	OutputFile         string   `json:"output_file"`
}

// SubmitResponse mirrors client.SubmitResponse.
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
		// Use in-process memory cache as L1 when no external cache is configured.
		s.store = cache.NewMemoryStore()
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20)) // 256 MiB limit
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req SubmitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Build cache key from preprocessed source digest + normalized args.
	cacheKey := buildCacheKey(req)

	// Use taskgroup to deduplicate concurrent tasks with the same cache key.
	result := s.tg.Do(cacheKey, func() (compileResult, error) {
		return s.execute(req, cacheKey)
	})

	if result.Err != nil {
		slog.Warn("locald: compile task failed", "error", result.Err)
		// Return the error; wrapper will fall back to local.
		http.Error(w, result.Err.Error(), http.StatusInternalServerError)
		return
	}

	resp := SubmitResponse{
		ExitCode:   result.Value.ExitCode,
		Stdout:     result.Value.Stdout,
		Stderr:     result.Value.Stderr,
		ObjectFile: result.Value.ObjectFile,
		CacheHit:   result.Value.CacheHit,
	}
	writeJSON(w, http.StatusOK, resp)
}

// execute performs the actual compilation, consulting cache first.
func (s *Server) execute(req SubmitRequest, cacheKey string) (compileResult, error) {
	// Try cache first.
	if cached, err := s.store.Get(cacheKey); err == nil {
		slog.Debug("locald: cache hit", "key", cacheKey)
		return compileResult{ExitCode: 0, ObjectFile: cached, CacheHit: true}, nil
	}

	// Try remote worker via scheduler.
	if s.SchedulerAddr != "" {
		if result, err := s.tryRemote(req); err == nil {
			// Store successful result in cache.
			if result.ExitCode == 0 && len(result.ObjectFile) > 0 {
				if putErr := s.store.Put(cacheKey, result.ObjectFile); putErr != nil {
					slog.Warn("locald: failed to store cache entry", "key", cacheKey, "error", putErr)
				}
			}
			return result, nil
		} else {
			slog.Debug("locald: remote compile failed, falling back to local", "error", err)
		}
	}

	// Fallback: local compilation.
	return s.localCompile(req)
}

// tryRemote asks the scheduler for a worker and submits compilation.
func (s *Server) tryRemote(req SubmitRequest) (compileResult, error) {
	// 1. Ask scheduler for a worker.
	workerAddr, err := s.getWorker()
	if err != nil {
		return compileResult{}, fmt.Errorf("get worker: %w", err)
	}

	// 2. Submit to worker (remoted).
	return s.submitToWorker(workerAddr, req)
}

// WorkerGrant is the scheduler's response to a worker request.
type workerGrant struct {
	WorkerAddr string `json:"worker_addr"`
	TaskToken  string `json:"task_token"`
}

func (s *Server) getWorker() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(s.SchedulerAddr + "/scheduler/acquire_worker")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("scheduler returned %d: %s", resp.StatusCode, string(body))
	}
	var grant workerGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		return "", fmt.Errorf("decode scheduler response: %w", err)
	}
	return grant.WorkerAddr, nil
}

// RemoteCompileRequest is the JSON body sent to a remote worker.
type remoteCompileRequest struct {
	CompilerPath       string   `json:"compiler_path"`
	Args               []string `json:"args"`
	Language           string   `json:"language"`
	PreprocessedSource []byte   `json:"preprocessed_source"`
}

// RemoteCompileResponse is the JSON response from a remote worker.
type remoteCompileResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	ObjectFile []byte `json:"object_file"`
}

func (s *Server) submitToWorker(workerAddr string, req SubmitRequest) (compileResult, error) {
	remoteReq := remoteCompileRequest{
		CompilerPath:       req.CompilerPath,
		Args:               req.Args,
		Language:           req.Language,
		PreprocessedSource: req.PreprocessedSource,
	}
	data, err := json.Marshal(remoteReq)
	if err != nil {
		return compileResult{}, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Post("http://"+workerAddr+"/remote/compile", "application/json", bytes.NewReader(data))
	if err != nil {
		return compileResult{}, fmt.Errorf("worker http post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return compileResult{}, fmt.Errorf("read worker response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return compileResult{}, fmt.Errorf("worker returned %d: %s", resp.StatusCode, string(body))
	}

	var remoteResp remoteCompileResponse
	if err := json.Unmarshal(body, &remoteResp); err != nil {
		return compileResult{}, fmt.Errorf("decode worker response: %w", err)
	}

	return compileResult{
		ExitCode:   remoteResp.ExitCode,
		Stdout:     remoteResp.Stdout,
		Stderr:     remoteResp.Stderr,
		ObjectFile: remoteResp.ObjectFile,
	}, nil
}

// localCompile runs the compiler locally (rate-limited by s.sem).
func (s *Server) localCompile(req SubmitRequest) (compileResult, error) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	// Write preprocessed source to a temp file.
	tmpFile, err := writeTempSource(req.Language, req.PreprocessedSource)
	if err != nil {
		return compileResult{}, fmt.Errorf("write temp source: %w", err)
	}
	// tmpFile is the temp path; clean it up after compilation.
	// (We don't defer-remove in production; the OS will clean it up anyway,
	// but let's be explicit.)
	defer removeFile(tmpFile)

	// Build local compile args: replace original input with tmp file, keep -o.
	args := buildLocalArgs(req.Args, tmpFile, req.OutputFile)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(req.CompilerPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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
		// Read the produced .o file.
		objBytes, err = readFile(req.OutputFile)
		if err != nil {
			slog.Warn("locald: failed to read output file", "path", req.OutputFile, "error", err)
		}
	}

	return compileResult{
		ExitCode:   exitCode,
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		ObjectFile: objBytes,
	}, nil
}

// buildCacheKey computes a cache key from the preprocessed source + normalized args.
func buildCacheKey(req SubmitRequest) string {
	h := sha256.New()
	fmt.Fprintf(h, "source:%x\n", sha256.Sum256(req.PreprocessedSource))
	fmt.Fprintf(h, "lang:%s\n", req.Language)
	// Include args that affect code generation (skip output/dep file args).
	for _, a := range req.Args {
		fmt.Fprintf(h, "arg:%s\n", a)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- helpers ----

func writeTempSource(lang string, source []byte) (string, error) {
	ext := ".i" // preprocessed C
	if lang == "c++" {
		ext = ".ii"
	}
	f, err := createTempFile("yadcc-*"+ext, source)
	if err != nil {
		return "", err
	}
	return f, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
