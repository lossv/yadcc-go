package compiler

import (
	"path/filepath"
	"strings"
)

type Args struct {
	options []Option
	files   []string
}

type Option struct {
	Key    string
	Values []string
}

var oneValueOptions = map[string]struct{}{
	"-B":                           {},
	"-CLASSPATH":                   {},
	"-D":                           {},
	"-I":                           {},
	"-L":                           {},
	"-MF":                          {},
	"-MQ":                          {},
	"-MT":                          {},
	"-U":                           {},
	"-Xclang":                      {},
	"-Xlinker":                     {},
	"-Xpreprocessor":               {},
	"-allowable_client":            {},
	"-arch":                        {},
	"-arch_only":                   {},
	"-arcmt-migrate-report-output": {},
	"-bundle_loader":               {},
	"-c-isystem":                   {},
	"-cxx-isystem":                 {},
	"-dependency-dot":              {},
	"-dependency-file":             {},
	"-dyld-prefix":                 {},
	"-dylib_file":                  {},
	"-encoding":                    {},
	"-exported_symbols_list":       {},
	"-filelist":                    {},
	"-force_load":                  {},
	"-framework":                   {},
	"-gcc-toolchain":               {},
	"-image_base":                  {},
	"-idirafter":                   {},
	"-iframework":                  {},
	"-iframeworkwithsysroot":       {},
	"-imacros":                     {},
	"-include":                     {},
	"-include-pch":                 {},
	"-init":                        {},
	"-install_name":                {},
	"-iquote":                      {},
	"-isystem":                     {},
	"-isystem-after":               {},
	"-isysroot":                    {},
	"-ivfsoverlay":                 {},
	"-iwithsysroot":                {},
	"-lazy_framework":              {},
	"-lazy_library":                {},
	"-l":                           {},
	"-meabi":                       {},
	"-mhwdiv":                      {},
	"-mllvm":                       {},
	"-module-dependency-dir":       {},
	"-mthread-model":               {},
	"-multiply_defined":            {},
	"-multiply_defined_unused":     {},
	"-o":                           {},
	"-rpath":                       {},
	"-resource-dir":                {},
	"-seg_addr_table":              {},
	"-seg_addr_table_filename":     {},
	"-segs_read_only_addr":         {},
	"-segs_read_write_addr":        {},
	"-serialize-diagnostics":       {},
	"-std":                         {},
	"-stdlib":                      {},
	"-target":                      {},
	"-umbrella":                    {},
	"-unexported_symbols_list":     {},
	"-weak_library":                {},
	"-weak_reference_mismatches":   {},
	"-x":                           {},
	"--CLASSPATH":                  {},
	"--assert":                     {},
	"--bootclasspath":              {},
	"--classpath":                  {},
	"--encoding":                   {},
	"--extdirs":                    {},
	"--force-link":                 {},
	"--include-directory":          {},
	"--include-directory-after":    {},
	"--include-prefix":             {},
	"--include-with-prefix":        {},
	"--include-with-prefix-after":  {},
	"--library-directory":          {},
	"--output-class-directory":     {},
	"--param":                      {},
	"--prefix":                     {},
	"--resource":                   {},
	"--rtlib":                      {},
	"--serialize-diagnostics":      {},
	"--stdlib":                     {},
	"--system-header-prefix":       {},
	"--sysroot":                    {},
}

var joinedValuePrefixes = []string{
	"-D",
	"-I",
	"-L",
	"-MF",
	"-MQ",
	"-MT",
	"-U",
	"-B",
	"-l",
	"-o",
	"-std=",
	"-stdlib=",
	"--sysroot=",
	"-isysroot=",
	"-target=",
	"-x",
}

func Parse(argv []string) Args {
	var out Args
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if _, ok := oneValueOptions[arg]; ok {
			opt := Option{Key: arg}
			if i+1 < len(argv) {
				opt.Values = append(opt.Values, argv[i+1])
				i++
			}
			out.options = append(out.options, opt)
			continue
		}
		if opt, ok := splitJoinedOption(arg); ok {
			out.options = append(out.options, opt)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			out.options = append(out.options, Option{Key: arg})
			continue
		}
		out.files = append(out.files, arg)
	}
	return out
}

func splitJoinedOption(arg string) (Option, bool) {
	for _, prefix := range joinedValuePrefixes {
		if !strings.HasPrefix(arg, prefix) || len(arg) == len(prefix) {
			continue
		}
		switch prefix {
		case "-std=", "-stdlib=", "--sysroot=", "-isysroot=", "-target=":
			return Option{Key: strings.TrimSuffix(prefix, "="), Values: []string{strings.TrimPrefix(arg, prefix)}}, true
		case "-x":
			value := strings.TrimPrefix(arg, prefix)
			if strings.HasPrefix(value, "=") {
				return Option{}, false
			}
			return Option{Key: prefix, Values: []string{value}}, true
		default:
			return Option{Key: prefix, Values: []string{strings.TrimPrefix(arg, prefix)}}, true
		}
	}
	return Option{}, false
}

func (a Args) Files() []string {
	return append([]string(nil), a.files...)
}

// SourceFile returns the single source file argument, or "" if not exactly one.
func (a Args) SourceFile() string {
	if len(a.files) == 1 {
		return a.files[0]
	}
	return ""
}

func (a Args) Has(key string) bool {
	_, ok := a.Get(key)
	return ok
}

func (a Args) Get(key string) ([]string, bool) {
	for _, opt := range a.options {
		if opt.Key == key {
			return append([]string(nil), opt.Values...), true
		}
	}
	return nil, false
}

func (a Args) HasPrefix(prefix string) bool {
	for _, opt := range a.options {
		if strings.HasPrefix(opt.Key, prefix) {
			return true
		}
	}
	return false
}

func (a Args) OutputFile() string {
	if values, ok := a.Get("-o"); ok && len(values) > 0 {
		return values[0]
	}
	if len(a.files) != 1 {
		return "a.o"
	}
	base := filepath.Base(a.files[0])
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base + ".o"
}

func (a Args) Language() (string, bool) {
	if values, ok := a.Get("-x"); ok && len(values) > 0 {
		return values[0], true
	}
	if len(a.files) != 1 {
		return "", false
	}
	name := a.files[0]
	switch {
	case strings.HasSuffix(name, ".c"):
		return "c", true
	case strings.HasSuffix(name, ".cc"), strings.HasSuffix(name, ".cpp"), strings.HasSuffix(name, ".cxx"), strings.HasSuffix(name, ".C"):
		return "c++", true
	default:
		return "", false
	}
}

// TargetTriple returns the value of -target if present.
func (a Args) TargetTriple() string {
	if values, ok := a.Get("-target"); ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

// Sysroot returns the sysroot path from -isysroot or --sysroot.
func (a Args) Sysroot() string {
	if values, ok := a.Get("-isysroot"); ok && len(values) > 0 {
		return values[0]
	}
	if values, ok := a.Get("--sysroot"); ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

// Stdlib returns the -stdlib value (e.g. "libc++", "libstdc++").
func (a Args) Stdlib() string {
	for _, opt := range a.options {
		if strings.HasPrefix(opt.Key, "-stdlib=") {
			return strings.TrimPrefix(opt.Key, "-stdlib=")
		}
		if opt.Key == "-stdlib" && len(opt.Values) > 0 {
			return opt.Values[0]
		}
	}
	return ""
}

func (a Args) IsDistributable() bool {
	if !a.Has("-c") {
		return false
	}
	if a.Has("-E") || a.Has("-S") || a.Has("-fsyntax-only") || a.Has("-M") || a.Has("-MM") {
		return false
	}
	if a.Has("-save-temps") || a.HasPrefix("-save-temps=") || a.Has("-pipe") {
		return false
	}
	if a.Has("--coverage") || a.Has("-coverage") ||
		a.Has("-fprofile-arcs") || a.Has("-ftest-coverage") ||
		a.HasPrefix("-fprofile-generate") || a.HasPrefix("-fprofile-use") ||
		a.HasPrefix("-fprofile-instr-generate") || a.HasPrefix("-fcoverage-") {
		return false
	}
	if a.Has("-Xclang") || a.Has("-Xpreprocessor") || a.Has("-Xassembler") || a.Has("-Xlinker") {
		return false
	}
	if a.Has("-save-stats") || a.HasPrefix("-save-stats=") ||
		a.Has("-ftime-trace") || a.HasPrefix("-ftime-trace=") {
		return false
	}
	if values, ok := a.Get("-x"); ok {
		if len(values) == 0 || (values[0] != "c" && values[0] != "c++") {
			return false
		}
	}
	if a.Has("-") {
		return false
	}
	if len(a.files) != 1 {
		return false
	}
	name := a.files[0]
	if strings.HasSuffix(name, ".s") || strings.HasSuffix(name, ".S") {
		return false
	}
	if _, ok := a.Language(); !ok {
		return false
	}
	return true
}
