package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mayurvarma14/go-proxy/internal/admin"
	"github.com/mayurvarma14/go-proxy/internal/config"
	"github.com/mayurvarma14/go-proxy/internal/runtime/supervisor"
)

func main() {
	cfgPath := flag.String("config", "./config/dev.yaml", "path to config file (YAML or JSON)")
	adminAddr := flag.String("admin", "127.0.0.1:9901", "admin server listen address")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load error: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("loaded %d listener(s)\n", len(cfg.Listeners))

	sup := supervisor.New(ctx, *cfgPath)
	if err := sup.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start error: %v\n", err)
		os.Exit(2)
	}

	go func() {
		if err := admin.Start(ctx, *adminAddr, sup.CurrentConfig, sup.Reload, sup.DrainStart, stop, func() interface{} { return sup.EndpointsDebug() }); err != nil {
			fmt.Fprintf(os.Stderr, "admin server error: %v\n", err)
		}
	}()

	<-ctx.Done()
	fmt.Println("shutting down")
}
