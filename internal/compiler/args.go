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
	"-o":         {},
	"-x":         {},
	"-D":         {},
	"-U":         {},
	"-I":         {},
	"-L":         {},
	"-l":         {},
	"-MF":        {},
	"-MT":        {},
	"-MQ":        {},
	"-include":   {},
	"-isystem":   {},
	"-iquote":    {},
	"-idirafter": {},
	"-isysroot":  {},
	"--sysroot":  {},
	"-target":    {},
	"-std":       {},
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
		if strings.HasPrefix(arg, "-") {
			out.options = append(out.options, Option{Key: arg})
			continue
		}
		out.files = append(out.files, arg)
	}
	return out
}

func (a Args) Files() []string {
	return append([]string(nil), a.files...)
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

func (a Args) IsDistributable() bool {
	if !a.Has("-c") {
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
