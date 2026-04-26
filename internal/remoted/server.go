package remoted

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
	"yadcc-go/internal/buildinfo"
	"yadcc-go/internal/compiler"
	"yadcc-go/internal/compress"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// taskRecord stores an in-flight or completed compilation task.
type taskRecord struct {
	mu     sync.Mutex
	done   chan struct{} // closed when compilation finishes
	result *pb.WaitForCompilationOutputResponse
}

// Server implements RemoteDaemonServiceServer and SchedulerService heartbeat.
type Server struct {
	pb.UnimplementedRemoteDaemonServiceServer

	// GRPCAddr is the address this worker listens on (e.g. "0.0.0.0:8335").
	GRPCAddr string
	// SchedulerAddr is the gRPC address of the scheduler (e.g. "host:8336").
	// Leave empty to skip registration.
	SchedulerAddr string
	// WorkerID uniquely identifies this worker instance.
	WorkerID string
	// CompilerPath is the local compiler binary to use for compilation.
	CompilerPath string
	// Capacity is the maximum number of concurrent compile tasks.
	Capacity uint32

	nextID atomic.Uint64
	mu     sync.Mutex
	tasks  map[uint64]*taskRecord

	environments    []*pb.EnvironmentDesc // advertised to scheduler; set at startup
	schedulerConn   *grpc.ClientConn
	schedulerClient pb.SchedulerServiceClient
}

// ListenAndServe starts the gRPC server and registers with the scheduler.
func (s *Server) ListenAndServe() error {
	if s.Capacity == 0 {
		s.Capacity = 4
	}
	s.tasks = make(map[uint64]*taskRecord)

	// Build the environment descriptor for this worker so the scheduler
	// can match tasks that require a specific compiler.
	s.environments = s.buildEnvironments()

	if s.SchedulerAddr != "" {
		if err := s.connectScheduler(); err != nil {
			slog.Warn("remoted: failed to connect to scheduler", "error", err)
		} else {
			go s.heartbeatLoop()
		}
	}

	lis, err := net.Listen("tcp", s.GRPCAddr)
	if err != nil {
		return fmt.Errorf("remoted: listen %s: %w", s.GRPCAddr, err)
	}
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(256 << 20))
	pb.RegisterRemoteDaemonServiceServer(gs, s)
	slog.Info("remoted: gRPC worker listening", "addr", s.GRPCAddr, "id", s.WorkerID,
		"version", buildinfo.String())
	return gs.Serve(lis)
}

// ---------- RemoteDaemonServiceServer ----------

// QueueCxxCompilationTask enqueues a compilation and returns a task ID
// immediately (async execution).
func (s *Server) QueueCxxCompilationTask(ctx context.Context, req *pb.QueueCxxCompilationTaskRequest) (*pb.QueueCxxCompilationTaskResponse, error) {
	id := s.nextID.Add(1)
	rec := &taskRecord{done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[id] = rec
	s.mu.Unlock()

	go func() {
		resp := s.runCompile(req)
		rec.mu.Lock()
		rec.result = resp
		rec.mu.Unlock()
		close(rec.done)
	}()

	return &pb.QueueCxxCompilationTaskResponse{TaskId: id}, nil
}

// ReferenceTask is a no-op reference bump (for future ref-counting).
func (s *Server) ReferenceTask(_ context.Context, req *pb.ReferenceTaskRequest) (*pb.ReferenceTaskResponse, error) {
	s.mu.Lock()
	_, ok := s.tasks[req.TaskId]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %d not found", req.TaskId)
	}
	return &pb.ReferenceTaskResponse{}, nil
}

// WaitForCompilationOutput polls for task completion.
func (s *Server) WaitForCompilationOutput(_ context.Context, req *pb.WaitForCompilationOutputRequest) (*pb.WaitForCompilationOutputResponse, error) {
	s.mu.Lock()
	rec, ok := s.tasks[req.TaskId]
	s.mu.Unlock()

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

// FreeRemoteTask removes a completed task record.
func (s *Server) FreeRemoteTask(_ context.Context, req *pb.FreeRemoteTaskRequest) (*pb.FreeRemoteTaskResponse, error) {
	s.mu.Lock()
	delete(s.tasks, req.TaskId)
	s.mu.Unlock()
	return &pb.FreeRemoteTaskResponse{}, nil
}

// ---------- compile logic ----------

func (s *Server) runCompile(req *pb.QueueCxxCompilationTaskRequest) *pb.WaitForCompilationOutputResponse {
	lang := inferLang(req.CompilerArguments)

	ext := ".i"
	if strings.Contains(lang, "++") {
		ext = ".ii"
	}

	// Decompress the preprocessed source.
	srcBytes, err := compress.Decompress(req.ZstdPreprocessedSource)
	if err != nil {
		return errResponse("decompress source: " + err.Error())
	}

	// Write preprocessed source to temp file.
	tmpSrc, err := os.CreateTemp("", "yadcc-remote-*"+ext)
	if err != nil {
		return errResponse("create temp source: " + err.Error())
	}
	tmpSrcPath := tmpSrc.Name()
	defer os.Remove(tmpSrcPath)

	if _, err := tmpSrc.Write(srcBytes); err != nil {
		tmpSrc.Close()
		return errResponse("write temp source: " + err.Error())
	}
	tmpSrc.Close()

	// Temp output file.
	tmpOut, err := os.CreateTemp("", "yadcc-remote-*.o")
	if err != nil {
		return errResponse("create temp output: " + err.Error())
	}
	tmpOutPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpOutPath)

	compilerBin := s.CompilerPath
	if compilerBin == "" {
		compilerBin = "cc"
	}

	args := buildCompileArgs(req.CompilerArguments, tmpSrcPath, tmpOutPath)
	slog.Debug("remoted: running compiler", "compiler", compilerBin, "args", args)

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
			return errResponse("run compiler: " + runErr.Error())
		}
	}

	resp := &pb.WaitForCompilationOutputResponse{
		Status:   pb.WaitForCompilationOutputResponse_STATUS_DONE,
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}

	if exitCode == 0 {
		objBytes, err := os.ReadFile(tmpOutPath)
		if err != nil {
			slog.Warn("remoted: failed to read output file", "error", err)
		} else {
			resp.Outputs = []*pb.FileBlob{{Name: "output.o", Data: objBytes}}
		}
	}

	return resp
}

// inferLang returns the language value from a -x flag in args, or "" if absent.
func inferLang(args []string) string {
	for i, a := range args {
		if a == "-x" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// preprocessedLangFlag converts a source language to the preprocessed variant
// expected by gcc/clang when compiling an already-preprocessed file.
//
//	"c"   / ""           → "cpp-output"
//	"c++" / "c++-cpp-…"  → "c++-cpp-output"
func preprocessedLangFlag(lang string) string {
	if strings.Contains(lang, "++") {
		return "c++-cpp-output"
	}
	return "cpp-output"
}

// buildCompileArgs adapts original compiler args for compiling a preprocessed
// source file.  It:
//   - Drops input file tokens (replaced by tmpSrc).
//   - Replaces -o with tmpOut.
//   - Drops preprocessing-only / dependency flags.
//   - Drops any existing -x (replaced by the correct preprocessed-language value).
//   - Prepends -x <cpp-output|c++-cpp-output>.
func buildCompileArgs(originalArgs []string, tmpSrc, tmpOut string) []string {
	lang := inferLang(originalArgs)

	skipNext := false
	hasOutput := false
	var args []string

	for i, a := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if !strings.HasPrefix(a, "-") {
			continue // input file — replaced by tmpSrc
		}
		switch a {
		case "-o":
			if i+1 < len(originalArgs) {
				skipNext = true
			}
			args = append(args, "-o", tmpOut)
			hasOutput = true
		case "-x":
			// drop; we prepend the correct preprocessed-language value below
			if i+1 < len(originalArgs) {
				skipNext = true
			}
		case "-E", "-fdirectives-only", "-MD", "-MMD", "-MP", "-MG":
			// drop
		case "-MF", "-MT", "-MQ":
			skipNext = true // drop flag + value
		default:
			args = append(args, a)
		}
	}

	if !hasOutput {
		args = append(args, "-o", tmpOut)
	}

	// Prepend -x <preprocessed-lang> then append the source file.
	result := make([]string, 0, len(args)+3)
	result = append(result, "-x", preprocessedLangFlag(lang))
	result = append(result, args...)
	result = append(result, tmpSrc)
	return result
}

func errResponse(msg string) *pb.WaitForCompilationOutputResponse {
	return &pb.WaitForCompilationOutputResponse{
		Status:   pb.WaitForCompilationOutputResponse_STATUS_DONE,
		ExitCode: 1,
		Stderr:   []byte(msg),
	}
}

// ---------- scheduler registration / heartbeat ----------

func (s *Server) connectScheduler() error {
	conn, err := grpc.NewClient(s.SchedulerAddr, grpc.WithInsecure()) //nolint:staticcheck
	if err != nil {
		return err
	}
	s.schedulerConn = conn
	s.schedulerClient = pb.NewSchedulerServiceClient(conn)
	// Send first heartbeat immediately.
	return s.sendHeartbeat()
}

func (s *Server) heartbeatLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.sendHeartbeat(); err != nil {
			slog.Warn("remoted: heartbeat error", "error", err)
		}
	}
}

func (s *Server) sendHeartbeat() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.mu.Lock()
	load := uint32(len(s.tasks))
	s.mu.Unlock()

	_, err := s.schedulerClient.Heartbeat(ctx, &pb.HeartbeatRequest{
		Token:        s.WorkerID,
		Location:     s.GRPCAddr,
		Capacity:     s.Capacity,
		CurrentLoad:  load,
		Environments: s.environments,
	})
	return err
}

// buildEnvironments probes the local compiler and returns the EnvironmentDesc
// slice to advertise in heartbeats.  If the compiler binary cannot be hashed
// (e.g. it is a shell wrapper), an entry with an empty digest is returned so
// that the worker is still visible to the scheduler for digest-agnostic tasks.
func (s *Server) buildEnvironments() []*pb.EnvironmentDesc {
	bin := s.CompilerPath
	if bin == "" {
		bin = "cc"
	}

	digest, err := compiler.Digest(bin)
	if err != nil {
		slog.Warn("remoted: could not hash compiler binary; advertising empty digest",
			"compiler", bin, "error", err)
		digest = ""
	}

	env := &pb.EnvironmentDesc{
		CompilerDigest: digest,
	}
	return []*pb.EnvironmentDesc{env}
}
