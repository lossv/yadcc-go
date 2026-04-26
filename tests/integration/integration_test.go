//go:build integration

// Package integration contains end-to-end tests that start real daemon
// processes (in-process via the public server structs) and compile actual C/C++
// source files through the full yadcc stack.
//
// Run with:
//
//	go test -v -tags integration ./tests/integration/
package integration

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"yadcc-go/internal/locald"
	"yadcc-go/internal/remoted"
	"yadcc-go/internal/scheduler"
)

// freePort returns a random available TCP port on localhost.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitHTTP polls addr/healthz until it responds 200 or the deadline passes.
func waitHTTP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := "http://" + addr + "/healthz"
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", url)
}

// waitTCP polls addr until a TCP connection succeeds or the deadline passes.
func waitTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for TCP %s", addr)
}

// resolveCompiler returns the absolute path of a C compiler available in PATH.
func resolveCompiler(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"clang", "gcc", "cc"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no C compiler found in PATH")
	return ""
}

// isObjectFile does a quick sanity-check that data looks like an object file.
func isObjectFile(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	// ELF magic
	if bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) {
		_, err := elf.NewFile(bytes.NewReader(data))
		return err == nil
	}
	// Mach-O magic (little-endian or fat)
	r := bytes.NewReader(data)
	_, errThin := macho.NewFile(r)
	r.Seek(0, 0)
	_, errFat := macho.NewFatFile(r)
	return errThin == nil || errFat == nil
}

// TestFullStack_LocalOnly starts only a locald (no scheduler/remoted) and
// verifies that a C source file compiles to a valid object file via the
// HTTP API.  This tests the local-fallback path.
func TestFullStack_LocalOnly(t *testing.T) {
	compilerPath := resolveCompiler(t)

	localdAddr := freePort(t)

	srv := &locald.Server{
		Addr:             localdAddr,
		MaxLocalParallel: 2,
		// No SchedulerAddr → pure local compile
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			// Server stops when test process exits; ignore the error.
		}
	}()
	waitHTTP(t, localdAddr, 5*time.Second)

	t.Run("compile_hello_c", func(t *testing.T) {
		runCompileTest(t, localdAddr, compilerPath, "c", helloC)
	})
}

// TestFullStack_Remote starts scheduler + remoted + locald and compiles a C
// source file through the full distributed path.
func TestFullStack_Remote(t *testing.T) {
	compilerPath := resolveCompiler(t)

	schedulerGRPC := freePort(t)
	schedulerHTTP := freePort(t)
	remotedGRPC := freePort(t)
	localdHTTP := freePort(t)

	// 1. Start scheduler.
	sched := &scheduler.Server{
		GRPCAddr: schedulerGRPC,
		HTTPAddr: schedulerHTTP,
	}
	go sched.ListenAndServe() //nolint:errcheck

	waitTCP(t, schedulerGRPC, 5*time.Second)

	// 2. Start remoted worker.
	workerID := fmt.Sprintf("test-worker-%d", os.Getpid())
	worker := &remoted.Server{
		GRPCAddr:      remotedGRPC,
		SchedulerAddr: schedulerGRPC,
		WorkerID:      workerID,
		CompilerPath:  compilerPath,
		Capacity:      4,
	}
	go worker.ListenAndServe() //nolint:errcheck

	waitTCP(t, remotedGRPC, 5*time.Second)

	// Give remoted time to send its first heartbeat so the scheduler
	// has at least one registered worker before we submit tasks.
	time.Sleep(300 * time.Millisecond)

	// 3. Start locald.
	local := &locald.Server{
		Addr:             localdHTTP,
		SchedulerAddr:    schedulerGRPC,
		MaxLocalParallel: 2,
	}
	go local.ListenAndServe() //nolint:errcheck

	waitHTTP(t, localdHTTP, 5*time.Second)

	t.Run("compile_hello_c_remote", func(t *testing.T) {
		runCompileTest(t, localdHTTP, compilerPath, "c", helloC)
	})

	if runtime.GOOS != "darwin" {
		// clang on macOS reports -x c++ when compiling C++ via -x c++-cpp-output;
		// the test works but skip on macOS CI to keep it simple.
		t.Run("compile_hello_cpp_remote", func(t *testing.T) {
			runCompileTest(t, localdHTTP, compilerPath, "c++", helloCPP)
		})
	}
}

// TestFullStack_CacheHit verifies that a second identical compile returns a
// cache hit (CacheHit=true in the response).
func TestFullStack_CacheHit(t *testing.T) {
	compilerPath := resolveCompiler(t)
	localdAddr := freePort(t)

	srv := &locald.Server{
		Addr:             localdAddr,
		MaxLocalParallel: 2,
	}
	go srv.ListenAndServe() //nolint:errcheck
	waitHTTP(t, localdAddr, 5*time.Second)

	// Preprocess once so both submits use the exact same preprocessed bytes
	// (and therefore the same cache key).
	srcFile := writeTempFile(t, "yadcc-cache-test-*.c", []byte(helloC))
	defer os.Remove(srcFile)

	ppCmd := exec.Command(compilerPath, "-E", "-x", "c", srcFile, "-o", "-")
	var ppOut bytes.Buffer
	ppCmd.Stdout = &ppOut
	if err := ppCmd.Run(); err != nil {
		t.Fatalf("preprocess: %v", err)
	}
	preprocessed := ppOut.Bytes()

	compilerArgs := []string{"-c", "-x", "c", srcFile, "-o", srcFile + ".o"}
	defer os.Remove(srcFile + ".o")

	submit := func() locald.SubmitResponse {
		reqBody, _ := json.Marshal(locald.SubmitRequest{
			CompilerPath:       compilerPath,
			Args:               compilerArgs,
			Language:           "c",
			PreprocessedSource: preprocessed,
			OutputFile:         srcFile + ".o",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+localdAddr+"/local/submit_task", bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		var result locald.SubmitResponse
		json.NewDecoder(resp.Body).Decode(&result) //nolint:errcheck
		return result
	}

	// First compile — should be a cache miss.
	r1 := submit()
	if r1.CacheHit {
		t.Error("first compile should not be a cache hit")
	}
	if r1.ExitCode != 0 {
		t.Fatalf("first compile failed: exit=%d stderr=%s", r1.ExitCode, r1.Stderr)
	}

	// Second identical compile — must be a cache hit.
	r2 := submit()
	if !r2.CacheHit {
		t.Error("second compile should be a cache hit")
	}
}

// ---------- helpers ----------

// runCompileTest submits a compile request and asserts success + valid object.
func runCompileTest(t *testing.T, localdAddr, compilerPath, lang, src string) {
	t.Helper()
	result := submitCompile(t, localdAddr, compilerPath, lang, src)
	if result.ExitCode != 0 {
		t.Fatalf("compile failed exit=%d\nstdout: %s\nstderr: %s",
			result.ExitCode, result.Stdout, result.Stderr)
	}
	if len(result.ObjectFile) == 0 {
		t.Fatal("compile returned empty object file")
	}
	if !isObjectFile(result.ObjectFile) {
		t.Errorf("output does not look like a valid object file (len=%d)", len(result.ObjectFile))
	}
	t.Logf("object file size: %d bytes  cache_hit: %v", len(result.ObjectFile), result.CacheHit)
}

// submitCompile preprocesses src and POSTs it to locald, returning the response.
func submitCompile(t *testing.T, localdAddr, compilerPath, lang, src string) locald.SubmitResponse {
	t.Helper()

	// Write source to a temp file so we can preprocess it.
	ext := ".c"
	if strings.Contains(lang, "++") || lang == "c++" {
		ext = ".cpp"
	}
	srcFile := writeTempFile(t, "yadcc-test-*"+ext, []byte(src))
	defer os.Remove(srcFile)

	outFile := srcFile + ".o"
	defer os.Remove(outFile)

	// Preprocess: run compiler -E.
	ppArgs := []string{"-E", "-x", lang, srcFile, "-o", "-"}
	ppCmd := exec.Command(compilerPath, ppArgs...)
	var ppOut, ppErr bytes.Buffer
	ppCmd.Stdout = &ppOut
	ppCmd.Stderr = &ppErr
	if err := ppCmd.Run(); err != nil {
		t.Fatalf("preprocess failed: %v\nstderr: %s", err, ppErr.String())
	}
	preprocessed := ppOut.Bytes()

	// Build compiler args (as if the wrapper had produced them).
	compilerArgs := []string{"-c", "-x", lang, srcFile, "-o", outFile}

	reqBody, err := json.Marshal(locald.SubmitRequest{
		CompilerPath:       compilerPath,
		Args:               compilerArgs,
		Language:           lang,
		PreprocessedSource: preprocessed,
		OutputFile:         outFile,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+localdAddr+"/local/submit_task",
		bytes.NewReader(reqBody))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST submit_task: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		t.Fatalf("submit_task returned HTTP %d: %s", resp.StatusCode, buf.String())
	}

	var result locald.SubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return result
}

func writeTempFile(t *testing.T, pattern string, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

// ---------- test sources ----------

const helloC = `
#include <stdio.h>
int add(int a, int b) { return a + b; }
int main(void) {
    printf("hello from yadcc integration test: %d\n", add(1, 2));
    return 0;
}
`

const helloCPP = `
#include <iostream>
#include <string>
std::string greet(const std::string& name) {
    return "hello, " + name;
}
int main() {
    std::cout << greet("yadcc") << "\n";
    return 0;
}
`

// resolveCompilerCPP returns a C++ compiler.
func resolveCompilerCPP(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"clang++", "g++", "c++"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no C++ compiler found in PATH")
	return ""
}

// TestFullStack_CPP_LocalOnly tests C++ local compilation.
func TestFullStack_CPP_LocalOnly(t *testing.T) {
	_ = resolveCompiler(t) // ensure a compiler exists
	compilerPath := resolveCompilerCPP(t)

	localdAddr := freePort(t)
	srv := &locald.Server{
		Addr:             localdAddr,
		MaxLocalParallel: 2,
	}
	go srv.ListenAndServe() //nolint:errcheck
	waitHTTP(t, localdAddr, 5*time.Second)

	runCompileTest(t, localdAddr, compilerPath, "c++", helloCPP)
}

// ---------- script-based test (optional) ----------

// TestScript_BuildAndRun builds the binaries and runs a real compile using the
// yadcc wrapper binary.  It requires the binaries to be pre-built in bin/.
func TestScript_BuildAndRun(t *testing.T) {
	binDir := filepath.Join("..", "..", "bin")
	yadccBin := filepath.Join(binDir, "yadcc")
	daemonBin := filepath.Join(binDir, "yadcc-daemon")
	schedulerBin := filepath.Join(binDir, "yadcc-scheduler")

	for _, p := range []string{yadccBin, daemonBin, schedulerBin} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("binary not found (%s), run `make build` first", p)
		}
	}

	compilerPath := resolveCompiler(t)

	schedGRPC := freePort(t)
	schedHTTP := freePort(t)
	remotedPort := freePort(t)
	localdPort := freePort(t)

	// Start scheduler process.
	schedCmd := exec.Command(schedulerBin, "-addr="+schedGRPC, "-http-addr="+schedHTTP)
	schedCmd.Stdout = os.Stdout
	schedCmd.Stderr = os.Stderr
	if err := schedCmd.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(func() { schedCmd.Process.Kill() })
	waitTCP(t, schedGRPC, 5*time.Second)

	// Start remoted worker process.
	workerCmd := exec.Command(daemonBin,
		"-mode=remote",
		"-addr=127.0.0.1:"+strings.Split(remotedPort, ":")[1],
		"-scheduler="+schedGRPC,
		"-compiler="+compilerPath,
		"-capacity=2",
	)
	workerCmd.Stdout = os.Stdout
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("start remoted: %v", err)
	}
	t.Cleanup(func() { workerCmd.Process.Kill() })
	waitTCP(t, remotedPort, 5*time.Second)
	time.Sleep(400 * time.Millisecond) // let first heartbeat arrive

	// Start locald process.
	localCmd := exec.Command(daemonBin,
		"-mode=local",
		"-addr=127.0.0.1:"+strings.Split(localdPort, ":")[1],
		"-scheduler="+schedGRPC,
	)
	localCmd.Stdout = os.Stdout
	localCmd.Stderr = os.Stderr
	if err := localCmd.Start(); err != nil {
		t.Fatalf("start locald: %v", err)
	}
	t.Cleanup(func() { localCmd.Process.Kill() })
	waitHTTP(t, localdPort, 5*time.Second)

	// Write a temp C source file and compile it via the wrapper.
	srcFile := writeTempFile(t, "yadcc-script-*.c", []byte(helloC))
	defer os.Remove(srcFile)
	outFile := srcFile + ".o"
	defer os.Remove(outFile)

	env := append(os.Environ(), "YADCC_DAEMON_ADDR=http://127.0.0.1:"+strings.Split(localdPort, ":")[1])
	compileCmd := exec.Command(yadccBin, "-c", "-x", "c", srcFile, "-o", outFile)
	compileCmd.Env = env
	compileCmd.Stdout = os.Stdout
	compileCmd.Stderr = os.Stderr
	if err := compileCmd.Run(); err != nil {
		t.Fatalf("yadcc compile failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !isObjectFile(data) {
		t.Errorf("output is not a valid object file (size=%d)", len(data))
	}
	t.Logf("script test passed, object size=%d", len(data))
}
