package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/evalops/service-runtime/agenthook"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := agenthook.Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
