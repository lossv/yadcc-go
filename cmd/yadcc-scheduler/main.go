package main

import (
	"flag"
	"log/slog"
	"os"

	"yadcc-go/internal/scheduler"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8336", "scheduler listen address")
	flag.Parse()

	slog.Info("starting yadcc-scheduler", "addr", *addr)
	if err := (&scheduler.Server{Addr: *addr}).ListenAndServe(); err != nil {
		slog.Error("scheduler stopped", "error", err)
		os.Exit(1)
	}
}
