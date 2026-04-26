package compiler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

// PreprocessResult holds the result of a preprocessing run.
type PreprocessResult struct {
	Source []byte
	// Language as determined from the original args (e.g. "c", "c++").
	Language string
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
		return PreprocessResult{Source: src, Language: lang}, nil
	}

	// Fall back to plain -E.
	src, err := runPreprocessor(compilerPath, baseArgs)
	if err != nil {
		return PreprocessResult{}, fmt.Errorf("preprocessing failed: %w", err)
	}
	return PreprocessResult{Source: src, Language: lang}, nil
}

// buildPreprocessArgs returns the argument list for a -E invocation based on
// the original compile args.  It strips compilation-only flags (-c, -o, -MF,
// -MT, -MQ, -MD, -MMD, -MP) and adds -E -x <lang> -o -.
func buildPreprocessArgs(a Args, lang string, inputFile string) []string {
	skipKeys := map[string]bool{
		"-c": true, "-o": true, "-MF": true, "-MT": true, "-MQ": true,
	}
	skipPrefixes := []string{"-MD", "-MMD", "-MP", "-MG"}

	var args []string
	for _, opt := range a.options {
		if skipKeys[opt.Key] {
			continue
		}
		skip := false
		for _, p := range skipPrefixes {
			if opt.Key == p {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		args = append(args, opt.Key)
		args = append(args, opt.Values...)
	}

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
