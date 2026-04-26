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

func TestParseJoinedOptions(t *testing.T) {
	args := Parse([]string{"-Iinclude", "-DDEBUG=1", "-std=c++17", "-xc++", "-c", "hello.cc", "-ohello.o"})

	if got := args.OutputFile(); got != "hello.o" {
		t.Fatalf("OutputFile() = %q, want hello.o", got)
	}
	if got, ok := args.Get("-I"); !ok || len(got) != 1 || got[0] != "include" {
		t.Fatalf("-I parse = %v, %v; want include", got, ok)
	}
	if got, ok := args.Get("-D"); !ok || len(got) != 1 || got[0] != "DEBUG=1" {
		t.Fatalf("-D parse = %v, %v; want DEBUG=1", got, ok)
	}
	if got, ok := args.Get("-std"); !ok || len(got) != 1 || got[0] != "c++17" {
		t.Fatalf("-std parse = %v, %v; want c++17", got, ok)
	}
	if got, ok := args.Language(); !ok || got != "c++" {
		t.Fatalf("Language() = %q, %v, want c++, true", got, ok)
	}
}

func TestParseRejectsCoverageAndPreprocessOnly(t *testing.T) {
	for _, argv := range [][]string{
		{"-c", "--coverage", "hello.c"},
		{"-c", "-fprofile-arcs", "hello.c"},
		{"-c", "-ftest-coverage", "hello.c"},
		{"-E", "hello.c"},
		{"-S", "hello.c"},
		{"-fsyntax-only", "hello.c"},
		{"-c", "-pipe", "hello.c"},
	} {
		if Parse(argv).IsDistributable() {
			t.Fatalf("Parse(%v).IsDistributable() = true, want false", argv)
		}
	}
}

func TestBuildPreprocessArgsKeepsDependencyFlags(t *testing.T) {
	args := Parse([]string{"-c", "-MD", "-MF", "hello.d", "-MTtarget", "-Iinclude", "hello.c", "-o", "hello.o"})
	got := buildPreprocessArgs(args, "c", "hello.c")

	for _, want := range []string{"-MD", "-MF", "hello.d", "-MT", "target", "-I", "include", "-E", "-o", "-"} {
		if !contains(got, want) {
			t.Fatalf("buildPreprocessArgs missing %q in %v", want, got)
		}
	}
	for _, bad := range []string{"-c", "hello.o"} {
		if contains(got, bad) {
			t.Fatalf("buildPreprocessArgs kept %q in %v", bad, got)
		}
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
