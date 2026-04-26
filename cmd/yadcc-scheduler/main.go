package main

import (
	"flag"
	"log/slog"
	"os"
	"strings"

	"yadcc-go/internal/scheduler"
)

func main() {
	grpcAddr := flag.String("addr", "0.0.0.0:8336", "scheduler gRPC listen address")
	httpAddr := flag.String("http-addr", "0.0.0.0:8337", "scheduler HTTP debug listen address (empty to disable)")
	userTokens := flag.String("user_tokens", "",
		"comma-separated list of accepted user tokens (empty = accept all)")
	servantTokens := flag.String("servant_tokens", "",
		"comma-separated list of accepted servant tokens (empty = accept all)")
	flag.Parse()

	slog.Info("starting yadcc-scheduler", "grpc", *grpcAddr, "http", *httpAddr)
	srv := &scheduler.Server{
		GRPCAddr:      *grpcAddr,
		HTTPAddr:      *httpAddr,
		UserTokens:    splitTokens(*userTokens),
		ServantTokens: splitTokens(*servantTokens),
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("scheduler stopped", "error", err)
		os.Exit(1)
	}
}

func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
