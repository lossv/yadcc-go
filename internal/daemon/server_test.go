package daemon

import (
	"testing"
)

// ---------- isCacheable ----------

func TestIsCacheable_noMacros(t *testing.T) {
	if !isCacheable([]string{"-O2", "-c"}, []byte("#include <stdio.h>\nint main(){}")) {
		t.Fatal("expected cacheable when no timestamp macros present")
	}
}

func TestIsCacheable_timeMacroInSource(t *testing.T) {
	src := []byte(`const char *ts = __TIME__;`)
	if isCacheable([]string{"-O2"}, src) {
		t.Fatal("expected NOT cacheable when __TIME__ appears in preprocessed source")
	}
}

func TestIsCacheable_dateMacroInSource(t *testing.T) {
	src := []byte(`const char *d = __DATE__;`)
	if isCacheable([]string{"-O2"}, src) {
		t.Fatal("expected NOT cacheable when __DATE__ appears in preprocessed source")
	}
}

func TestIsCacheable_timestampMacroInSource(t *testing.T) {
	src := []byte(`const char *ts = __TIMESTAMP__;`)
	if isCacheable([]string{"-O2"}, src) {
		t.Fatal("expected NOT cacheable when __TIMESTAMP__ appears in preprocessed source")
	}
}

func TestIsCacheable_allOverriddenByDFlags(t *testing.T) {
	args := []string{
		"-D__TIME__=\"00:00:00\"",
		"-D__DATE__=\"Jan  1 1970\"",
		"-D__TIMESTAMP__=\"\"",
		"-O2",
	}
	// Even if the source still mentions the macro names, the -D overrides make
	// the output deterministic.
	src := []byte(`const char *ts = __TIME__;`)
	if !isCacheable(args, src) {
		t.Fatal("expected cacheable when all three macros are overridden via -D")
	}
}

func TestIsCacheable_partialOverride(t *testing.T) {
	// Only __TIME__ and __DATE__ overridden — __TIMESTAMP__ is not.
	args := []string{
		"-D__TIME__=\"00:00:00\"",
		"-D__DATE__=\"Jan  1 1970\"",
	}
	src := []byte(`const char *ts = __TIMESTAMP__;`)
	if isCacheable(args, src) {
		t.Fatal("expected NOT cacheable when __TIMESTAMP__ is not overridden and appears in source")
	}
}

// ---------- normalizeArgs ----------

func TestNormalizeArgs_stripsOutputAndDeps(t *testing.T) {
	args := []string{"-O2", "-o", "foo.o", "-MF", "foo.d", "-MD", "-c"}
	got := normalizeArgs(args)
	for _, a := range got {
		switch a {
		case "-o", "foo.o", "-MF", "foo.d", "-MD":
			t.Errorf("unexpected arg in normalized output: %q", a)
		}
	}
	// -O2 and -c should survive
	found := map[string]bool{}
	for _, a := range got {
		found[a] = true
	}
	if !found["-O2"] {
		t.Error("-O2 should survive normalizeArgs")
	}
	if !found["-c"] {
		t.Error("-c should survive normalizeArgs")
	}
}

func TestNormalizeArgs_empty(t *testing.T) {
	if got := normalizeArgs(nil); len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

// ---------- buildLocalArgs ----------

func TestBuildLocalArgs_injectsXFlag(t *testing.T) {
	args := buildLocalArgs([]string{"-O2", "-c", "-o", "out.o", "src.c"}, "c", "/tmp/src.i", "/tmp/out.o")
	// Must start with -x cpp-output
	if len(args) < 2 || args[0] != "-x" || args[1] != "cpp-output" {
		t.Errorf("expected -x cpp-output at start, got %v", args[:min(2, len(args))])
	}
}

func TestBuildLocalArgs_cppLang(t *testing.T) {
	args := buildLocalArgs([]string{"-std=c++17", "-O2", "-c"}, "c++", "/tmp/src.ii", "/tmp/out.o")
	if len(args) < 2 || args[0] != "-x" || args[1] != "c++-cpp-output" {
		t.Errorf("expected -x c++-cpp-output at start, got %v", args[:min(2, len(args))])
	}
}

func TestBuildLocalArgs_outputReplaced(t *testing.T) {
	args := buildLocalArgs([]string{"-c", "-o", "wrong.o"}, "c", "/tmp/s.i", "/tmp/correct.o")
	for i, a := range args {
		if a == "-o" && i+1 < len(args) && args[i+1] != "/tmp/correct.o" {
			t.Errorf("output not replaced: got %q", args[i+1])
		}
	}
}

// ---------- buildCompileArgs ----------

func TestBuildCompileArgs_injectsXFlag(t *testing.T) {
	args := buildCompileArgs([]string{"-O2", "-c", "-o", "out.o", "src.c"}, "/tmp/src.i", "/tmp/out.o")
	if len(args) < 2 || args[0] != "-x" {
		t.Errorf("expected -x flag at start, got %v", args)
	}
}

func TestBuildCompileArgs_stripsEFlag(t *testing.T) {
	args := buildCompileArgs([]string{"-E", "-O2", "-c"}, "/tmp/src.i", "/tmp/out.o")
	for _, a := range args {
		if a == "-E" {
			t.Error("-E should be stripped by buildCompileArgs")
		}
	}
}

// ---------- preprocessedLangFlag ----------

func TestPreprocessedLangFlag(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"c++", "c++-cpp-output"},
		{"c++-cpp-output", "c++-cpp-output"},
		{"c++header", "c++-cpp-output"},
		{"c", "cpp-output"},
		{"", "cpp-output"},
		{"objc", "cpp-output"},
	}
	for _, tc := range cases {
		if got := preprocessedLangFlag(tc.in); got != tc.want {
			t.Errorf("preprocessedLangFlag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
