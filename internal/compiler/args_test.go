package compiler

import "testing"

func TestParseDistributableCompile(t *testing.T) {
	args := Parse([]string{"-I", "include", "-DDEBUG=1", "-c", "hello.cc", "-o", "hello.o"})

	if !args.IsDistributable() {
		t.Fatal("IsDistributable() = false, want true")
	}
	if got := args.OutputFile(); got != "hello.o" {
		t.Fatalf("OutputFile() = %q, want hello.o", got)
	}
	if got, ok := args.Language(); !ok || got != "c++" {
		t.Fatalf("Language() = %q, %v, want c++, true", got, ok)
	}
}

func TestParseRejectsLink(t *testing.T) {
	args := Parse([]string{"hello.o", "-o", "hello"})

	if args.IsDistributable() {
		t.Fatal("IsDistributable() = true, want false")
	}
}

func TestParseRejectsStdin(t *testing.T) {
	args := Parse([]string{"-x", "c++", "-c", "-"})

	if args.IsDistributable() {
		t.Fatal("IsDistributable() = true, want false")
	}
}

func TestOutputFileDefault(t *testing.T) {
	args := Parse([]string{"-c", "path/to/hello.c"})

	if got := args.OutputFile(); got != "hello.o" {
		t.Fatalf("OutputFile() = %q, want hello.o", got)
	}
}
