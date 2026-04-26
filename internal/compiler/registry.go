package compiler

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "yadcc-go/api/gen/yadcc/v1"
)

// Registry scans the system for available compilers, computes their binary
// digests, and exposes them as EnvironmentDesc slices for scheduler heartbeats.
//
// The scan runs once at construction time and then repeats every RescanInterval
// (default 60 s) in the background.
type Registry struct {
	// ExtraDirs are additional directories to scan beyond $PATH.
	ExtraDirs []string
	// RescanInterval controls how often the registry rescans for new compilers.
	// Zero means use the default (60 s).
	RescanInterval time.Duration

	mu       sync.RWMutex
	envDescs []*pb.EnvironmentDesc // protected by mu
	done     chan struct{}
}

// knownNames are the base names we look for.  The real binary may be any of
// these; variants like gcc-12, gcc12, g++12 are also matched by scanning
// directory listings.
var knownPrefixes = []string{"gcc", "g++", "clang", "clang++"}

// Start performs the initial scan synchronously, then launches a background
// goroutine that rescans on the configured interval.
func (r *Registry) Start() {
	r.done = make(chan struct{})
	r.scan()
	go r.loop()
}

// Stop terminates the background rescan goroutine.
func (r *Registry) Stop() {
	if r.done != nil {
		close(r.done)
	}
}

// Environments returns the current snapshot of discovered compiler environments.
func (r *Registry) Environments() []*pb.EnvironmentDesc {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*pb.EnvironmentDesc, len(r.envDescs))
	copy(out, r.envDescs)
	return out
}

func (r *Registry) loop() {
	interval := r.RescanInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			r.scan()
		}
	}
}

func (r *Registry) scan() {
	dirs := r.searchDirs()
	seen := make(map[string]bool) // canonical path → already added
	var descs []*pb.EnvironmentDesc

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !isCompilerName(name) {
				continue
			}
			fullPath := filepath.Join(dir, name)
			canonical, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				continue
			}
			if seen[canonical] {
				continue
			}
			seen[canonical] = true

			// Must be executable.
			info, err := os.Stat(canonical)
			if err != nil || info.Mode()&0111 == 0 {
				continue
			}

			digest, err := Digest(canonical)
			if err != nil {
				slog.Debug("compiler registry: cannot hash compiler", "path", canonical, "error", err)
				continue
			}

			descs = append(descs, &pb.EnvironmentDesc{
				CompilerDigest: digest,
			})
			slog.Debug("compiler registry: discovered compiler",
				"name", name, "path", canonical, "digest", digest[:8]+"…")
		}
	}

	r.mu.Lock()
	r.envDescs = descs
	r.mu.Unlock()
	slog.Info("compiler registry: scan complete", "compilers", len(descs))
}

// searchDirs returns the ordered list of directories to scan.
func (r *Registry) searchDirs() []string {
	seen := make(map[string]bool)
	var dirs []string

	add := func(d string) {
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}

	// $PATH first.
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		add(d)
	}

	// Caller-supplied extras.
	for _, d := range r.ExtraDirs {
		add(d)
	}

	// RHEL devtoolset paths.
	for i := 1; i <= 99; i++ {
		add(fmt.Sprintf("/opt/rh/devtoolset-%d/root/bin", i))
	}

	return dirs
}

// isCompilerName returns true if the base name looks like a C/C++ compiler.
// IsCompilerName reports whether name looks like a C/C++ compiler binary
// (e.g. "gcc", "g++-12", "clang++", "cc").
func IsCompilerName(name string) bool { return isCompilerName(name) }

func isCompilerName(name string) bool {
	for _, prefix := range knownPrefixes {
		if name == prefix {
			return true
		}
		// Accept versioned variants: gcc-12, gcc12, g++-12, clang++-15, etc.
		if strings.HasPrefix(name, prefix) {
			rest := name[len(prefix):]
			if len(rest) == 0 {
				return true
			}
			// rest must be only digits and hyphens
			ok := true
			for _, c := range rest {
				if c != '-' && (c < '0' || c > '9') {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// FindByName looks up the absolute path of a compiler by its basename in the
// same search order as the registry scan, skipping wrappers (ccache, distcc,
// icecc) and the yadcc binary itself (identified by selfPath).
//
// This is the Go equivalent of yadcc's FindExecutableInPath().
func FindByName(name, selfPath string) (string, error) {
	selfCanonical, _ := filepath.EvalSymlinks(selfPath)

	// Walk PATH.
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, name)
		canonical, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || info.Mode()&0111 == 0 {
			continue
		}
		base := filepath.Base(canonical)
		if base == "ccache" || base == "distcc" || base == "icecc" {
			continue
		}
		if canonical == selfCanonical {
			continue
		}
		return canonical, nil
	}
	return "", fmt.Errorf("compiler %q not found in PATH", name)
}

// LookupCompiler resolves a compiler reference to an absolute path.
//
//   - If ref is an absolute path, it is returned as-is (after verifying it exists).
//   - Otherwise FindByName is called.
func LookupCompiler(ref, selfPath string) (string, error) {
	if filepath.IsAbs(ref) {
		if _, err := os.Stat(ref); err != nil {
			return "", fmt.Errorf("compiler %q not found: %w", ref, err)
		}
		return ref, nil
	}
	return FindByName(ref, selfPath)
}

// LookupCompilerOrFallback is like LookupCompiler but falls back to exec.LookPath
// if FindByName returns nothing.
func LookupCompilerOrFallback(name, selfPath string) (string, error) {
	if p, err := LookupCompiler(name, selfPath); err == nil {
		return p, nil
	}
	return exec.LookPath(name)
}
