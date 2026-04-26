package cache

import "testing"

func TestBuildKeyStable(t *testing.T) {
	input := KeyInput{
		CompilerDigest:           "compiler",
		CompilerKind:             "gcc",
		CompilerVersion:          "13.2.0",
		HostOS:                   "linux",
		HostArch:                 "amd64",
		TargetTriple:             "x86_64-linux-gnu",
		ObjectFormat:             "elf",
		NormalizedArguments:      []string{"-O2", "-c", "-fPIC"},
		PreprocessedSourceDigest: "source",
		OutputKind:               "object",
	}

	first := BuildKey(input)
	second := BuildKey(input)
	if first != second {
		t.Fatalf("BuildKey() is unstable: %q != %q", first, second)
	}
}

func TestBuildKeySeparatesPlatforms(t *testing.T) {
	input := KeyInput{
		CompilerDigest:           "compiler",
		NormalizedArguments:      []string{"-O2"},
		PreprocessedSourceDigest: "source",
		OutputKind:               "object",
		HostOS:                   "linux",
		HostArch:                 "amd64",
	}
	linux := BuildKey(input)
	input.HostOS = "darwin"
	darwin := BuildKey(input)

	if linux == darwin {
		t.Fatal("BuildKey() did not separate different host OS values")
	}
}
