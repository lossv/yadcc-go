package main

import (
	"flag"
	"log/slog"
	"os"

	"yadcc-go/internal/cache"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8337", "cache service listen address")
	engine := flag.String("engine", "memory", "cache backend: memory or disk")
	diskDir := flag.String("disk-dir", "tmp/cache", "disk cache directory when -engine=disk")
	flag.Parse()

	store, err := newStore(*engine, *diskDir)
	if err != nil {
		slog.Error("failed to initialize cache store", "error", err)
		os.Exit(1)
	}

	slog.Info("starting yadcc-cache", "addr", *addr)
	if err := (cache.Server{Addr: *addr, Store: store}).ListenAndServe(); err != nil {
		slog.Error("cache stopped", "error", err)
		os.Exit(1)
	}
}

func newStore(engine string, diskDir string) (cache.Store, error) {
	switch engine {
	case "memory":
		return cache.NewMemoryStore(), nil
	case "disk":
		return cache.NewDiskStore(diskDir)
	default:
		return nil, flag.ErrHelp
	}
}
