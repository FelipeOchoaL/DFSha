package main

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"
	"dfsha/services/internal/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRunStartsAndStops(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	cfg := common.Config{NodeID: "lifecycle-test", Rack: "rack-test", GRPCPort: port, DataDir: filepath.Join(t.TempDir(), "blocks"), AdvertiseAddr: "localhost"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("DataNode did not shut down")
		}
	})
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, err := grpc.DialContext(dialCtx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = datanodev1.NewDataNodeClient(conn).DeleteBlock(dialCtx, &datanodev1.DeleteBlockRequest{BlockId: "missing"})
	if err != nil {
		t.Fatal(err)
	}
}
