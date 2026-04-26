package remoted

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"yadcc-go/internal/buildinfo"
)

// Server is the remote compilation worker HTTP server.
type Server struct {
	Addr string
	// SchedulerAddr is the scheduler to register with on startup.
	SchedulerAddr string
	// WorkerID uniquely identifies this worker.
	WorkerID string
	// CompilerPath is the absolute path to the compiler on this machine.
	CompilerPath string
	// Capacity is the max number of concurrent compile tasks.
	Capacity int
}

// CompileRequest mirrors locald.remoteCompileRequest.
type CompileRequest struct {
	CompilerPath       string   `json:"compiler_path"`
	Args               []string `json:"args"`
	Language           string   `json:"language"`
	PreprocessedSource []byte   `json:"preprocessed_source"`
}

// CompileResponse mirrors locald.remoteCompileResponse.
type CompileResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	ObjectFile []byte `json:"object_file"`
}

// registerRequest mirrors scheduler.RegisterRequest.
type registerRequest struct {
	ID           string `json:"id"`
	Addr         string `json:"addr"`
	CompilerPath string `json:"compiler_path"`
	Capacity     int    `json:"capacity"`
}

// heartbeatRequest mirrors scheduler.HeartbeatRequest.
type heartbeatRequest struct {
	ID      string `json:"id"`
	Running int    `json:"running"`
}

func (s *Server) ListenAndServe() error {
	if s.Capacity <= 0 {
		s.Capacity = 4
	}

	// Register with scheduler.
	if s.SchedulerAddr != "" {
		if err := s.registerWithScheduler(); err != nil {
			slog.Warn("remoted: failed to register with scheduler", "error", err)
		} else {
			slog.Info("remoted: registered with scheduler", "id", s.WorkerID)
			go s.heartbeatLoop()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/remote/compile", s.handleCompile)
	return http.ListenAndServe(s.Addr, mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": buildinfo.String(),
		"id":      s.WorkerID,
	})
}

func (s *Server) handleCompile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req CompileRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp := s.compile(req)
	writeJSON(w, http.StatusOK, resp)
}

// compile runs the actual compilation on the preprocessed source.
func (s *Server) compile(req CompileRequest) CompileResponse {
	// Write preprocessed source to a temp file.
	ext := ".i"
	if req.Language == "c++" {
		ext = ".ii"
	}
	tmpSrc, err := os.CreateTemp("", "yadcc-remote-*"+ext)
	if err != nil {
		return CompileResponse{ExitCode: 1, Stderr: []byte("create temp source: " + err.Error())}
	}
	tmpSrcPath := tmpSrc.Name()
	defer os.Remove(tmpSrcPath)

	if _, err := tmpSrc.Write(req.PreprocessedSource); err != nil {
		tmpSrc.Close()
		return CompileResponse{ExitCode: 1, Stderr: []byte("write temp source: " + err.Error())}
	}
	tmpSrc.Close()

	// Create a temp file for the output .o
	tmpOut, err := os.CreateTemp("", "yadcc-remote-*.o")
	if err != nil {
		return CompileResponse{ExitCode: 1, Stderr: []byte("create temp output: " + err.Error())}
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	// Determine which compiler binary to use.
	compilerBin := s.CompilerPath
	if compilerBin == "" {
		compilerBin = req.CompilerPath
	}

	// Build compile args from the original args, replacing input and output.
	args := buildRemoteArgs(req.Args, tmpSrcPath, tmpOutPath)

	slog.Debug("remoted: running compiler", "compiler", compilerBin, "args", args)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(compilerBin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()

	exitCode := 0
	if runErr := cmd.Run(); runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return CompileResponse{ExitCode: 1, Stderr: []byte("run compiler: " + runErr.Error())}
		}
	}

	var objBytes []byte
	if exitCode == 0 {
		objBytes, err = os.ReadFile(tmpOutPath)
		if err != nil {
			slog.Warn("remoted: failed to read output file", "path", tmpOutPath, "error", err)
		}
	}

	return CompileResponse{
		ExitCode:   exitCode,
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		ObjectFile: objBytes,
	}
}

// buildRemoteArgs builds compiler args for remote compilation, using tmpSrc as
// input and tmpOut as output.
func buildRemoteArgs(originalArgs []string, tmpSrc, tmpOut string) []string {
	skipNext := false
	hasOutput := false
	var args []string

	for i, a := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if !strings.HasPrefix(a, "-") {
			// input file — replaced by tmpSrc below
			continue
		}
		if a == "-o" {
			if i+1 < len(originalArgs) {
				skipNext = true
			}
			args = append(args, "-o", tmpOut)
			hasOutput = true
			continue
		}
		// Drop preprocessing artifacts.
		if a == "-E" || a == "-fdirectives-only" || a == "-MD" || a == "-MMD" || a == "-MP" || a == "-MG" {
			continue
		}
		if a == "-MF" || a == "-MT" || a == "-MQ" {
			skipNext = true
			continue
		}
		args = append(args, a)
	}

	if !hasOutput {
		args = append(args, "-o", tmpOut)
	}
	args = append(args, tmpSrc)
	return args
}

// registerWithScheduler sends a registration request to the scheduler.
func (s *Server) registerWithScheduler() error {
	req := registerRequest{
		ID:           s.WorkerID,
		Addr:         s.Addr,
		CompilerPath: s.CompilerPath,
		Capacity:     s.Capacity,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(s.SchedulerAddr+"/scheduler/register", "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &httpError{code: resp.StatusCode, body: string(body)}
	}
	return nil
}

// heartbeatLoop periodically sends heartbeats to the scheduler.
func (s *Server) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: 5 * time.Second}
	for range ticker.C {
		req := heartbeatRequest{ID: s.WorkerID}
		data, _ := json.Marshal(req)
		resp, err := client.Post(s.SchedulerAddr+"/scheduler/heartbeat", "application/json", bytes.NewReader(data))
		if err != nil {
			slog.Warn("remoted: heartbeat failed", "error", err)
			// Try re-registering.
			_ = s.registerWithScheduler()
			continue
		}
		resp.Body.Close()
	}
}

type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string {
	return "http " + http.StatusText(e.code) + ": " + e.body
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
