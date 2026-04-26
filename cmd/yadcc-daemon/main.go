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
	mode := flag.String("mode", "local", "daemon mode: local (wrapper-facing daemon) or remote (worker)")

	// local daemon flags
	addr := flag.String("addr", "127.0.0.1:8334", "listen address")
	schedulerAddr := flag.String("scheduler", "http://127.0.0.1:8336", "scheduler address")
	cacheAddr := flag.String("cache", "", "cache service address (empty = in-process memory cache)")
	maxLocal := flag.Int("max-local", 8, "max concurrent local fallback compilations")

	// remote worker flags
	workerID := flag.String("worker-id", "", "worker unique ID (defaults to hostname:port)")
	compilerPath := flag.String("compiler", "", "path to compiler on this machine (remote mode)")
	capacity := flag.Int("capacity", 4, "max concurrent compile tasks (remote mode)")

	flag.Parse()

	switch *mode {
	case "local":
		slog.Info("starting yadcc-daemon (local mode)", "addr", *addr, "scheduler", *schedulerAddr)
		srv := locald.Server{
			Addr:             *addr,
			SchedulerAddr:    *schedulerAddr,
			CacheAddr:        *cacheAddr,
			MaxLocalParallel: *maxLocal,
		}
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("daemon stopped", "error", err)
			os.Exit(1)
		}

	case "remote":
		remoteAddr := *addr
		if remoteAddr == "127.0.0.1:8334" {
			remoteAddr = "0.0.0.0:8335"
		}
		id := *workerID
		if id == "" {
			hostname, _ := os.Hostname()
			id = fmt.Sprintf("%s:%s", hostname, portOf(remoteAddr))
		}
		slog.Info("starting yadcc-daemon (remote worker mode)", "addr", remoteAddr, "id", id, "scheduler", *schedulerAddr)
		srv := remoted.Server{
			Addr:          remoteAddr,
			SchedulerAddr: *schedulerAddr,
			WorkerID:      id,
			CompilerPath:  *compilerPath,
			Capacity:      *capacity,
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
