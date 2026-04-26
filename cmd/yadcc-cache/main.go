package main

import (
	"flag"
	"log/slog"
	"os"

	"yadcc-go/internal/cache"
)

func main() {
	httpAddr := flag.String("addr", "0.0.0.0:8339", "cache service HTTP listen address")
	grpcAddr := flag.String("grpc-addr", "0.0.0.0:8338", "cache service gRPC listen address")
	engine := flag.String("engine", "memory", "cache backend: memory or disk")
	diskDir := flag.String("disk-dir", "tmp/cache", "disk cache directory when -engine=disk")
	diskMaxGB := flag.Int64("disk-max-gb", 10,
		"maximum disk cache size in GiB when -engine=disk (0 = unlimited)")
	flag.Parse()

	store, err := newStore(*engine, *diskDir, *diskMaxGB)
	if err != nil {
		slog.Error("failed to initialize cache store", "error", err)
		os.Exit(1)
	}

	// Start the gRPC server in the background.
	go func() {
		slog.Info("starting yadcc-cache gRPC", "addr", *grpcAddr)
		if err := (&cache.GRPCServer{GRPCAddr: *grpcAddr, Store: store}).ListenAndServe(); err != nil {
			slog.Error("cache gRPC stopped", "error", err)
			os.Exit(1)
		}
	}()

	// HTTP server runs in the foreground.
	slog.Info("starting yadcc-cache HTTP", "addr", *httpAddr)
	if err := (cache.Server{Addr: *httpAddr, Store: store}).ListenAndServe(); err != nil {
		slog.Error("cache HTTP stopped", "error", err)
		os.Exit(1)
	}
}

func newStore(engine, diskDir string, diskMaxGB int64) (cache.Store, error) {
	switch engine {
	case "memory":
		return cache.NewMemoryStore(), nil
	case "disk":
		maxBytes := diskMaxGB << 30 // GiB → bytes; 0 = unlimited
		return cache.NewDiskStoreWithLimit(diskDir, maxBytes)
	default:
		return nil, flag.ErrHelp
	}
}
