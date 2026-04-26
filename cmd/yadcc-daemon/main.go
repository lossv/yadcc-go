package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"yadcc-go/internal/locald"
	"yadcc-go/internal/remoted"
)

func main() {
	mode := flag.String("mode", "local", "daemon mode: local (wrapper-facing) or remote (worker)")

	// shared
	addr := flag.String("addr", "", "listen address (default depends on mode)")
	schedulerAddr := flag.String("scheduler", "127.0.0.1:8336", "scheduler gRPC address")

	// local daemon flags
	cacheAddr := flag.String("cache", "", "cache service address (empty = in-process memory)")
	maxLocal := flag.Int("max-local", 8, "max concurrent local fallback compilations")

	// remote worker flags
	workerID := flag.String("worker-id", "", "worker unique ID (defaults to hostname:port)")
	compilerPath := flag.String("compiler", "", "compiler binary path on this machine (remote mode)")
	capacity := flag.Int("capacity", 4, "max concurrent compile tasks (remote mode)")

	flag.Parse()

	switch *mode {
	case "local":
		listenAddr := *addr
		if listenAddr == "" {
			listenAddr = "127.0.0.1:8334"
		}
		slog.Info("starting yadcc-daemon (local mode)", "addr", listenAddr, "scheduler", *schedulerAddr)
		srv := locald.Server{
			Addr:             listenAddr,
			SchedulerAddr:    *schedulerAddr,
			CacheAddr:        *cacheAddr,
			MaxLocalParallel: *maxLocal,
		}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("daemon stopped", "error", err)
			os.Exit(1)
		}

	case "remote":
		listenAddr := *addr
		if listenAddr == "" {
			listenAddr = "0.0.0.0:8335"
		}
		id := *workerID
		if id == "" {
			hostname, _ := os.Hostname()
			id = fmt.Sprintf("%s:%s", hostname, portOf(listenAddr))
		}
		slog.Info("starting yadcc-daemon (remote worker mode)",
			"addr", listenAddr, "id", id, "scheduler", *schedulerAddr)
		srv := remoted.Server{
			GRPCAddr:      listenAddr,
			SchedulerAddr: *schedulerAddr,
			WorkerID:      id,
			CompilerPath:  *compilerPath,
			Capacity:      uint32(*capacity),
		}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("worker stopped", "error", err)
			os.Exit(1)
		}

	default:
		slog.Error("unknown mode", "mode", *mode)
		os.Exit(1)
	}
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
