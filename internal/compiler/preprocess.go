package compiler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// PreprocessResult holds the result of a preprocessing run.
type PreprocessResult struct {
	Source []byte
	// Language as determined from the original args (e.g. "c", "c++").
	Language string
	// SourceDir is the directory of the original source file; used for
	// path normalisation in NormalizePreprocessed.
	SourceDir string
}

// Preprocess runs the C/C++ preprocessor for the given source file.
//
// It first attempts `-E -fdirectives-only` (faster, avoids macro expansion).
// If that fails, it falls back to plain `-E`.
//
// The returned Source is the preprocessed source in memory (not written to disk).
func Preprocess(compilerPath string, a Args) (PreprocessResult, error) {
	lang, ok := a.Language()
	if !ok {
		return PreprocessResult{}, fmt.Errorf("cannot determine language for preprocessing")
	}

	inputFile := ""
	if files := a.Files(); len(files) == 1 {
		inputFile = files[0]
	}
	if inputFile == "" {
		return PreprocessResult{}, fmt.Errorf("no input file for preprocessing")
	}

	// Build base args: keep defines, includes, sysroot, target, std, etc.
	// but strip -c, -o, dependency tracking flags, etc.
	baseArgs := buildPreprocessArgs(a, lang, inputFile)

	// Try -fdirectives-only first.
	if src, err := runPreprocessor(compilerPath, append(baseArgs, "-fdirectives-only")); err == nil {
		src = NormalizePreprocessed(src, filepath.Dir(inputFile), a.Sysroot())
		return PreprocessResult{Source: src, Language: lang, SourceDir: filepath.Dir(inputFile)}, nil
	}

	// Fall back to plain -E.
	src, err := runPreprocessor(compilerPath, baseArgs)
	if err != nil {
		return PreprocessResult{}, fmt.Errorf("preprocessing failed: %w", err)
	}
	src = NormalizePreprocessed(src, filepath.Dir(inputFile), a.Sysroot())
	return PreprocessResult{Source: src, Language: lang, SourceDir: filepath.Dir(inputFile)}, nil
}

// buildPreprocessArgs returns the argument list for a -E invocation based on
// the original compile args. It strips compilation-only flags (-c, -o,
// -fworking-directory) and adds -fno-working-directory so that line-marker
// paths in the preprocessed output are CWD-independent (matching the C++
// reference behaviour in rewrite_file.cc). Dependency flags (-MD/-MF/-MT/-MQ)
// are kept so build systems still receive dependency files from the local
// preprocessing step.
func buildPreprocessArgs(a Args, lang string, inputFile string) []string {
	skipKeys := map[string]bool{
		"-c": true, "-o": true,
		"-fworking-directory": true,
	}

	var args []string
	for _, opt := range a.options {
		if skipKeys[opt.Key] {
			continue
		}
		if opt.Joined && len(opt.Values) == 1 {
			// Reconstruct as a single token (e.g. "-std=gnu11", "-DFOO", "-Idir").
			args = append(args, opt.Key+opt.JoinSep+opt.Values[0])
		} else {
			args = append(args, opt.Key)
			args = append(args, opt.Values...)
		}
	}

	// Always emit CWD-independent line markers so the preprocessed source can
	// be compiled on a remote machine without CWD matching.
	args = append(args, "-fno-working-directory")

	// Ensure -x is explicit so the preprocessor knows the language.
	if !a.Has("-x") {
		args = append(args, "-x", lang)
	}

	args = append(args, "-E")
	args = append(args, "-o", "-") // output to stdout
	args = append(args, inputFile)
	return args
}

// runPreprocessor executes the compiler with the given args and captures stdout.
func runPreprocessor(compilerPath string, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(compilerPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compiler exited with error: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
