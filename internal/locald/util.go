package locald

import (
	"os"
	"strings"
)

// createTempFile creates a temp file with the given pattern and writes content to it.
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

// preprocessedLanguageFlag returns the gcc/clang -x value for a preprocessed
// source file.  gcc/clang requires an explicit -x when compiling a .i/.ii
// file to indicate whether it is C or C++, because the preprocessed form
// is "cpp-output" / "c++-cpp-output", not "c" / "c++".
//
// Using the wrong value (or omitting -x) causes the compiler to apply
// another macro-expansion pass, which breaks on already-preprocessed input.
func preprocessedLanguageFlag(lang string) string {
	if strings.Contains(lang, "++") {
		return "c++-cpp-output"
	}
	return "cpp-output"
}

// buildLocalArgs builds the compiler argument list for compiling a
// preprocessed source file locally.
//
// Key transformations:
//   - Strip the original input file(s) — replaced by tmpFile.
//   - Replace -o <path> with -o outputFile.
//   - Drop preprocessing-only flags (-E, -fdirectives-only).
//   - Drop dependency-file flags (-MF/-MT/-MQ and their values; -MD/-MMD/-MP/-MG).
//   - Drop or replace any existing -x with the correct preprocessed-language value.
//   - Prepend -x <cpp-output|c++-cpp-output> so the compiler knows the input
//     is already preprocessed.
//   - Append tmpFile as the input.
func buildLocalArgs(originalArgs []string, lang, tmpFile, outputFile string) []string {
	skipNext := false
	hasOutput := false
	var args []string

	for i, a := range originalArgs {
		if skipNext {
			skipNext = false
			continue
		}
		// Non-flag tokens are input files — skip them (replaced by tmpFile).
		if !strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "-o":
			if i+1 < len(originalArgs) {
				skipNext = true
			}
			args = append(args, "-o", outputFile)
			hasOutput = true
		case "-E", "-fdirectives-only":
			// preprocessing-only flags — drop
		case "-MD", "-MMD", "-MP", "-MG":
			// dependency generation flags — drop
		case "-MF", "-MT", "-MQ":
			// dependency flags with a value — drop flag and value
			skipNext = true
		case "-x":
			// drop original -x; we will prepend the correct one below
			if i+1 < len(originalArgs) {
				skipNext = true
			}
		default:
			args = append(args, a)
		}
	}

	if !hasOutput {
		args = append(args, "-o", outputFile)
	}

	// Prepend -x <preprocessed-lang> so gcc/clang treats the input as
	// already preprocessed and does not run another macro-expansion pass.
	result := make([]string, 0, len(args)+3)
	result = append(result, "-x", preprocessedLanguageFlag(lang))
	result = append(result, args...)
	result = append(result, tmpFile)
	return result
}
