package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

// Dial 建立内部服务连接。传输在集群内网，使用明文；跨公网需加 mTLS。
func Dial(addr string) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, fmt.Errorf("服务地址为空")
	}
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(32<<20), grpc.MaxCallSendMsgSize(32<<20)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// NewServer 构造带 panic 恢复与 trace 传播的 gRPC 服务端。
func NewServer(logger *slog.Logger) *grpc.Server {
	recovery := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("gRPC handler panic", "method", info.FullMethod, "panic", rec, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		if id := ids.Trace(ctx); id == "" {
			ctx = ids.WithTrace(ctx, ids.New("trace"))
		}
		return handler(ctx, req)
	}
	streamRecovery := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("gRPC stream panic", "method", info.FullMethod, "panic", rec, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
	return grpc.NewServer(
		grpc.MaxRecvMsgSize(32<<20), grpc.MaxSendMsgSize(32<<20),
		grpc.ChainUnaryInterceptor(recovery),
		grpc.ChainStreamInterceptor(streamRecovery),
	)
}

// Serve 监听并阻塞直到 ctx 取消。
func Serve(ctx context.Context, logger *slog.Logger, addr string, server *grpc.Server, register func(*grpc.Server)) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", addr, err)
	}
	register(server)
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(lis) }()
	logger.Info("gRPC 服务已启动", "addr", addr)
	select {
	case <-ctx.Done():
		done := make(chan struct{})
		go func() { server.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
		return nil
	case err := <-errCh:
		return err
	}
}
