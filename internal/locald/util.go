package locald

import (
	"os"
	"strings"
)

// createTempFile creates a temp file with the given pattern and writes content to it.
// Returns the file path.
func createTempFile(pattern string, content []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// removeFile removes a file, ignoring errors.
func removeFile(path string) {
	_ = os.Remove(path)
}

// readFile reads a file and returns its content.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// buildLocalArgs builds the compiler argument list for local compilation
// from preprocessed source.  It replaces the original input file with tmpFile
// and ensures -o is set to outputFile.
func buildLocalArgs(originalArgs []string, tmpFile, outputFile string) []string {
	skipNext := false
	var args []string
	hasOutput := false

	for i, a := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}
		// Skip original input files (non-flag arguments).
		if !strings.HasPrefix(a, "-") {
			// This is an input file — replace with tmpFile later.
			continue
		}
		if a == "-o" {
			// Replace output path.
			if i+1 < len(originalArgs) {
				skipNext = true
			}
			args = append(args, "-o", outputFile)
			hasOutput = true
			continue
		}
		// Skip preprocessing-only flags that should not appear in real compile.
		if a == "-E" || a == "-fdirectives-only" {
			continue
		}
		// Skip -MF/-MT/-MQ and their values.
		if a == "-MF" || a == "-MT" || a == "-MQ" {
			skipNext = true
			continue
		}
		args = append(args, a)
	}

	if !hasOutput {
		args = append(args, "-o", outputFile)
	}

	// The preprocessed input file comes last.
	args = append(args, tmpFile)
	return args
}
