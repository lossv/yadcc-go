package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yadcc-go/internal/compiler"
	"yadcc-go/internal/platform"
)

const submitTimeout = 60 * time.Second

// daemonBaseURL returns the locald HTTP base URL.  The environment variable
// YADCC_DAEMON_ADDR overrides the default so tests and non-standard deployments
// can point at a different port without recompiling.
func daemonBaseURL() string {
	if v := os.Getenv("YADCC_DAEMON_ADDR"); v != "" {
		return v
	}
	return "http://127.0.0.1:8334"
}

func Run(argv []string) int {
	if len(argv) <= 1 {
		return 0
	}

	compilerPath, compilerArgs, err := resolveCompiler(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yadcc: failed to resolve compiler: %v\n", err)
		return 127
	}

	parsedArgs := compiler.Parse(compilerArgs)
	if !parsedArgs.IsDistributable() {
		// Not a distributable compile task — run locally.
		return platform.Passthrough(compilerPath, compilerArgs)
	}

	// Attempt distributed compilation via local daemon.
	exitCode, err := tryDistributed(compilerPath, compilerArgs, parsedArgs)
	if err != nil {
		slog.Debug("yadcc: distributed compilation failed, falling back to local", "error", err)
		return platform.Passthrough(compilerPath, compilerArgs)
	}
	return exitCode
}

// tryDistributed attempts to compile via the local daemon.
// It preprocesses the source locally, then submits to the daemon.
// Returns (exitCode, nil) on success, or (0, error) to trigger local fallback.
func tryDistributed(compilerPath string, compilerArgs []string, parsedArgs compiler.Args) (int, error) {
	// Step 1: preprocess locally.
	ppResult, err := compiler.Preprocess(compilerPath, parsedArgs)
	if err != nil {
		return 0, fmt.Errorf("preprocessing: %w", err)
	}

	// Step 2: submit to daemon.
	outputFile := parsedArgs.OutputFile()
	result, err := submitToDaemon(compilerPath, compilerArgs, ppResult, outputFile, parsedArgs.SourceFile())
	if err != nil {
		return 0, fmt.Errorf("daemon submission: %w", err)
	}

	// Step 3: write compiler stdout/stderr to our own stdout/stderr.
	if len(result.Stdout) > 0 {
		os.Stdout.Write(result.Stdout)
	}
	if len(result.Stderr) > 0 {
		os.Stderr.Write(result.Stderr)
	}

	// Step 4: write output file.
	if len(result.ObjectFile) > 0 {
		if err := os.WriteFile(outputFile, result.ObjectFile, 0644); err != nil {
			return 0, fmt.Errorf("writing output file %s: %w", outputFile, err)
		}
	}

	return result.ExitCode, nil
}

// SubmitRequest is the JSON body sent to /local/submit_task.
type SubmitRequest struct {
	// CompilerPath is the absolute path to the compiler on this machine.
	CompilerPath string `json:"compiler_path"`
	// Args is the full original compiler argument list (without compiler itself).
	Args []string `json:"args"`
	// Language is "c" or "c++".
	Language string `json:"language"`
	// PreprocessedSource is the preprocessed source file bytes.
	PreprocessedSource []byte `json:"preprocessed_source"`
	// OutputFile is the expected output .o path (for daemon reference only).
	OutputFile string `json:"output_file"`
	// SourcePath is the original source file path (before preprocessing).
	SourcePath string `json:"source_path,omitempty"`
}

// SubmitResponse is the JSON response from /local/submit_task.
type SubmitResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	ObjectFile []byte `json:"object_file"`
	// CacheHit indicates whether this result came from cache.
	CacheHit bool `json:"cache_hit"`
}

func submitToDaemon(compilerPath string, compilerArgs []string, ppResult compiler.PreprocessResult, outputFile, sourcePath string) (*SubmitResponse, error) {
	reqBody := SubmitRequest{
		CompilerPath:       compilerPath,
		Args:               compilerArgs,
		Language:           ppResult.Language,
		PreprocessedSource: ppResult.Source,
		OutputFile:         outputFile,
		SourcePath:         sourcePath,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: submitTimeout}
	resp, err := client.Post(daemonBaseURL()+"/local/submit_task", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned status %d: %s", resp.StatusCode, string(body))
	}

	var result SubmitResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &result, nil
}

func resolveCompiler(argv []string) (string, []string, error) {
	invokedAs := filepath.Base(argv[0])
	self := platform.ExecutablePath()

	if isDirectInvocation(invokedAs) {
		compiler := argv[1]
		if filepath.IsAbs(compiler) || strings.ContainsRune(compiler, filepath.Separator) {
			return compiler, argv[2:], nil
		}
		resolved, err := platform.LookupExecutableSkipping(compiler, self)
		return resolved, argv[2:], err
	}

	resolved, err := platform.LookupExecutableSkipping(invokedAs, self)
	return resolved, argv[1:], err
}

func isDirectInvocation(name string) bool {
	return name == "yadcc" || name == "yadcc-go" || name == "yadcc-cxx"
}
