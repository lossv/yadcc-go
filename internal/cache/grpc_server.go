package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	pb "yadcc-go/api/gen/yadcc/v1"

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
	// Inject store into the server at serve time.
	impl := &grpcCacheService{store: store}
	pb.RegisterCacheServiceServer(gs, impl)

	slog.Info("cache: gRPC server listening", "addr", s.GRPCAddr)
	return gs.Serve(lis)
}

// grpcCacheService is the concrete gRPC handler (separate from GRPCServer so
// the Store reference is always set when RPC methods are called).
type grpcCacheService struct {
	pb.UnimplementedCacheServiceServer
	store Store
}

// TryGetEntry retrieves a cache entry by key.
func (g *grpcCacheService) TryGetEntry(_ context.Context, req *pb.TryGetEntryRequest) (*pb.TryGetEntryResponse, error) {
	value, err := g.store.Get(req.Key)
	if errors.Is(err, ErrNotFound) {
		// Return empty response — caller detects miss by len(value)==0.
		return &pb.TryGetEntryResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cache get: %v", err)
	}
	return &pb.TryGetEntryResponse{Value: value}, nil
}

// PutEntry stores a value under key.
func (g *grpcCacheService) PutEntry(_ context.Context, req *pb.PutEntryRequest) (*pb.PutEntryResponse, error) {
	if err := g.store.Put(req.Key, req.Value); err != nil {
		return nil, status.Errorf(codes.Internal, "cache put: %v", err)
	}
	return &pb.PutEntryResponse{}, nil
}

// FetchBloomFilter returns a bloom filter of all current keys so callers can
// cheaply check for likely cache hits before making a TryGetEntry RPC.
func (g *grpcCacheService) FetchBloomFilter(_ context.Context, req *pb.FetchBloomFilterRequest) (*pb.FetchBloomFilterResponse, error) {
	keys, err := g.store.Keys()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cache keys: %v", err)
	}

	// Incremental updates (delta since last fetch) are not yet implemented;
	// always return the full filter.  Callers should treat incremental=false
	// as "replace your local copy".
	filter := NewBloomFilterForKeys(keys)
	return &pb.FetchBloomFilterResponse{
		Incremental: false,
		BloomFilter: filter.Bytes(),
		NumHashes:   filter.NumHashes(),
	}, nil
}
