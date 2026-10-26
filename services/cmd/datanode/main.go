package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"
	"dfsha/services/internal/common"
	"dfsha/services/internal/datanode"

	"google.golang.org/grpc"
)

func main() {
	cfg, err := common.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg common.Config) error {
	storage, err := datanode.NewStorage(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open data dir: %w", err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("listen gRPC: %w", err)
	}
	defer listener.Close()

	server := grpc.NewServer()
	svc := datanode.NewGRPCServer(cfg.NodeID, storage)
	if len(cfg.NameNodeAddrs) > 0 && cfg.AdvertiseAddr != "" {
		reporter, err := datanode.NewReporter(cfg, storage)
		if err != nil {
			return fmt.Errorf("nameNode reporter: %w", err)
		}
		svc.SetNotifier(reporter.NotifyBlock)
		go reporter.Run(ctx)
	} else {
		log.Printf("ADVERTENCIA: sin NAMENODE_ADDRS o ADVERTISE_ADDR no hay heartbeat; este DataNode no se registrará")
	}
	datanodev1.RegisterDataNodeServer(server, svc)

	log.Printf(
		"DataNode starting id=%s rack=%s grpc_port=%d advertise_addr=%s data_dir=%s namenodes=%v",
		cfg.NodeID,
		cfg.Rack,
		cfg.GRPCPort,
		cfg.AdvertiseAddr,
		cfg.DataDir,
		cfg.NameNodeAddrs,
	)

	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil {
			errs <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Println("DataNode shutting down")
	case err := <-errs:
		return fmt.Errorf("serve gRPC: %w", err)
	}

	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		server.Stop()
		<-done
	}
	return nil
}
