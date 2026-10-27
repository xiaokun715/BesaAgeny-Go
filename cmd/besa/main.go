// besa 是单二进制入口：CLI、控制面与所有服务角色都由 --role 切换。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/besa-ai/besa-agent/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(app.NewCLI().Run(ctx, os.Args[1:]))
}
