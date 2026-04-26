package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
)

const CurrentFormatVersion = 1

type KeyInput struct {
	FormatVersion            int
	CompilerDigest           string
	CompilerKind             string
	CompilerVersion          string
	HostOS                   string
	HostArch                 string
	TargetTriple             string
	ObjectFormat             string
	SysrootDigest            string
	StdlibDigest             string
	ABI                      string
	NormalizedArguments      []string
	PreprocessedSourceDigest string
	OutputKind               string
}

func BuildKey(input KeyInput) string {
	if input.FormatVersion == 0 {
		input.FormatVersion = CurrentFormatVersion
	}
	if input.HostOS == "" {
		input.HostOS = runtime.GOOS
	}
	if input.HostArch == "" {
		input.HostArch = runtime.GOARCH
	}

	var b strings.Builder
	writeField(&b, "format", fmt.Sprint(input.FormatVersion))
	writeField(&b, "compiler_digest", input.CompilerDigest)
	writeField(&b, "compiler_kind", input.CompilerKind)
	writeField(&b, "compiler_version", input.CompilerVersion)
	writeField(&b, "host_os", input.HostOS)
	writeField(&b, "host_arch", input.HostArch)
	writeField(&b, "target_triple", input.TargetTriple)
	writeField(&b, "object_format", input.ObjectFormat)
	writeField(&b, "sysroot_digest", input.SysrootDigest)
	writeField(&b, "stdlib_digest", input.StdlibDigest)
	writeField(&b, "abi", input.ABI)
	writeField(&b, "arguments", strings.Join(input.NormalizedArguments, "\x00"))
	writeField(&b, "source_digest", input.PreprocessedSourceDigest)
	writeField(&b, "output_kind", input.OutputKind)

	sum := sha256.Sum256([]byte(b.String()))
	return "v" + fmt.Sprint(input.FormatVersion) + "-" + hex.EncodeToString(sum[:])
}

func writeField(b *strings.Builder, key string, value string) {
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(value)
	b.WriteByte('\n')
}
