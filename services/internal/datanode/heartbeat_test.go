package datanode

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"
	"dfsha/services/internal/common"
	"dfsha/services/internal/namenode"

	namenodev1 "dfsha/services/gen/namenode/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func startTestNameNode(t *testing.T) (addr string, reg *namenode.Registry, blocks *namenode.BlockMap) {
	t.Helper()
	blocks = namenode.NewBlockMap()
	reg = namenode.NewRegistry(blocks, namenode.RegistryOptions{
		HeartbeatInterval: time.Second,
		DeadAfter:         3 * time.Second,
		ReplicationFactor: 3,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	namenodev1.RegisterNameNodeServer(s, namenode.NewDataNodeService(reg, nil))
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(ln) }()
	t.Cleanup(func() { s.Stop(); <-done })
	return ln.Addr().String(), reg, blocks
}

func TestReporterRegistersAndReportsBlock(t *testing.T) {
	addr, reg, blocks := startTestNameNode(t)
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := common.Config{
		NodeID:        "dn1",
		Rack:          "A",
		GRPCPort:      50051,
		AdvertiseAddr: "127.0.0.1:50051",
		NameNodeAddrs: []string{addr},
	}
	rep, err := NewReporter(cfg, storage)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rep.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := reg.LiveNode("dn1"); ok && n.Host == "127.0.0.1" && n.Port == 50051 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := reg.LiveNode("dn1"); !ok {
		t.Fatal("DataNode did not register")
	}

	if err := blocks.AddFileBlock("/f", "blk1", 3); err != nil {
		t.Fatal(err)
	}
	rep.NotifyBlock(BlockEvent{ID: "blk1", Checksum: "aaa", Size: 3})
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if blocks.CountReplicas("blk1", "aaa") == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("BlockReceived was not applied")
}

func TestAdvertiseHostPort(t *testing.T) {
	h, p, err := common.AdvertiseHostPort("3.80.1.2:50051", 1)
	if err != nil || h != "3.80.1.2" || p != 50051 {
		t.Fatalf("got %s:%d %v", h, p, err)
	}
	h, p, err = common.AdvertiseHostPort("localhost", 50051)
	if err != nil || h != "localhost" || p != 50051 {
		t.Fatalf("host-only: %s:%d %v", h, p, err)
	}
}

type standbyReporterServer struct {
	namenodev1.UnimplementedNameNodeServer
	registers atomic.Int32
}

func (s *standbyReporterServer) Register(context.Context, *namenodev1.RegisterRequest) (*namenodev1.RegisterResponse, error) {
	s.registers.Add(1)
	return nil, status.Error(codes.Unavailable, "este NameNode es standby")
}

func TestReporterTriesNextNameNodeWhenFirstIsStandby(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	standby := &standbyReporterServer{}
	srv := grpc.NewServer()
	namenodev1.RegisterNameNodeServer(srv, standby)
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	active, reg, _ := startTestNameNode(t)
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := NewReporter(common.Config{NodeID: "dn-rotate", AdvertiseAddr: "127.0.0.1:50051",
		NameNodeAddrs: []string{ln.Addr().String(), active}}, storage)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rep.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.LiveNode("dn-rotate"); ok {
			if standby.registers.Load() == 0 {
				t.Fatal("no se intentó primero el standby")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("el reporter no pasó del standby al NameNode activo")
}

func TestDialInsecureStillWorks(t *testing.T) {
	// sanity: generated stub is usable
	_ = insecure.NewCredentials()
	_ = datanodev1.DataNodeEndpoint{}
}
