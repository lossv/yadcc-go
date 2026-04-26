package main

import (
	"flag"
	"log/slog"
	"os"

	"yadcc-go/internal/daemon"
)

func main() {
	localAddr := flag.String("local_addr", "127.0.0.1:8334",
		"HTTP listen address for wrapper-facing API (loopback)")
	servantAddr := flag.String("servant_addr", "0.0.0.0:8335",
		"gRPC listen address for remote compilation tasks")
	schedulerURI := flag.String("scheduler_uri", "",
		"scheduler gRPC address, e.g. 10.0.0.1:8336 (empty = no distributed compilation)")
	cacheAddr := flag.String("cache_addr", "",
		"yadcc-cache gRPC address (empty = in-process L1 memory cache only)")
	token := flag.String("token", "yadcc",
		"authentication token for scheduler and cache")
	servantPriority := flag.String("servant_priority", "user",
		"CPU allocation for remote tasks: user (~40% CPUs) or dedicated (~95% CPUs)")
	workerID := flag.String("worker_id", "",
		"unique worker identifier (default: hostname:servant_port)")

	flag.Parse()

	var prio daemon.ServantPriority
	switch *servantPriority {
	case "dedicated":
		prio = daemon.ServantPriorityDedicated
	default:
		prio = daemon.ServantPriorityUser
	}

	srv := &daemon.Server{
		LocalAddr:       *localAddr,
		ServantAddr:     *servantAddr,
		SchedulerAddr:   *schedulerURI,
		CacheAddr:       *cacheAddr,
		Token:           *token,
		ServantPriority: prio,
		WorkerID:        *workerID,
	}

	slog.Info("starting yadcc-daemon",
		"local_addr", *localAddr,
		"servant_addr", *servantAddr,
		"scheduler_uri", *schedulerURI,
		"servant_priority", *servantPriority,
	)

	if err := srv.ListenAndServe(); err != nil {
		slog.Error("daemon stopped", "error", err)
		os.Exit(1)
	}
}
