package main

import (
	"flag"
	"log/slog"
	"os"

	"yadcc-go/internal/scheduler"
)

func main() {
	grpcAddr := flag.String("addr", "0.0.0.0:8336", "scheduler gRPC listen address")
	httpAddr := flag.String("http-addr", "0.0.0.0:8337", "scheduler HTTP debug listen address (empty to disable)")
	flag.Parse()

	slog.Info("starting yadcc-scheduler", "grpc", *grpcAddr, "http", *httpAddr)
	srv := &scheduler.Server{
		GRPCAddr: *grpcAddr,
		HTTPAddr: *httpAddr,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("scheduler stopped", "error", err)
		os.Exit(1)
	}
}
