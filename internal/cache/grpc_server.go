package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	pb "yadcc-go/api/gen/yadcc/v1"
	"yadcc-go/internal/metrics"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCServer implements pb.CacheServiceServer backed by a Store.
type GRPCServer struct {
	pb.UnimplementedCacheServiceServer

	// GRPCAddr is the address to listen on (e.g. "0.0.0.0:8338").
	GRPCAddr string
	// Store is the backing cache store.  Defaults to a new MemoryStore.
	Store Store
}

// ListenAndServe starts the gRPC cache server and blocks until it stops.
func (s *GRPCServer) ListenAndServe() error {
	store := s.Store
	if store == nil {
		store = NewMemoryStore()
	}

	lis, err := net.Listen("tcp", s.GRPCAddr)
	if err != nil {
		return fmt.Errorf("cache grpc: listen %s: %w", s.GRPCAddr, err)
	}

	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(256<<20),
		grpc.MaxSendMsgSize(256<<20),
	)
	bm := NewBloomManager(store)
	impl := &grpcCacheService{store: store, bloom: bm}
	pb.RegisterCacheServiceServer(gs, impl)

	slog.Info("cache: gRPC server listening", "addr", s.GRPCAddr)
	return gs.Serve(lis)
}

// grpcCacheService is the concrete gRPC handler.
type grpcCacheService struct {
	pb.UnimplementedCacheServiceServer
	store Store
	bloom *BloomManager
}

// TryGetEntry retrieves a cache entry by key.
func (g *grpcCacheService) TryGetEntry(_ context.Context, req *pb.TryGetEntryRequest) (*pb.TryGetEntryResponse, error) {
	value, err := g.store.Get(req.Key)
	if errors.Is(err, ErrNotFound) {
		metrics.CacheGetTotal.WithLabelValues("miss").Inc()
		return &pb.TryGetEntryResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cache get: %v", err)
	}
	metrics.CacheGetTotal.WithLabelValues("hit").Inc()
	return &pb.TryGetEntryResponse{Value: value}, nil
}

// PutEntry stores a value under key.
func (g *grpcCacheService) PutEntry(_ context.Context, req *pb.PutEntryRequest) (*pb.PutEntryResponse, error) {
	if err := g.store.Put(req.Key, req.Value); err != nil {
		return nil, status.Errorf(codes.Internal, "cache put: %v", err)
	}
	g.bloom.NotifyPut(req.Key)
	metrics.CachePutTotal.Inc()
	st := g.store.Stats()
	metrics.CacheStoreBytes.Set(float64(st.Bytes))
	metrics.CacheStoreEntries.Set(float64(st.Entries))
	return &pb.PutEntryResponse{}, nil
}

// FetchBloomFilter returns a (possibly incremental) bloom filter of all current
// keys so callers can cheaply check for likely cache hits before TryGetEntry.
// The filter bytes are zstd-compressed.
func (g *grpcCacheService) FetchBloomFilter(_ context.Context, req *pb.FetchBloomFilterRequest) (*pb.FetchBloomFilterResponse, error) {
	incremental, newKeys, filterBytes, numHashes := g.bloom.FetchResponse(req.SecondsSinceLastFetch)
	return &pb.FetchBloomFilterResponse{
		Incremental:        incremental,
		NewlyPopulatedKeys: newKeys,
		BloomFilter:        filterBytes,
		NumHashes:          numHashes,
	}, nil
}
