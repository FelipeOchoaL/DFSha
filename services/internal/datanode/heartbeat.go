// Cliente hacia el NameNode: Register, Heartbeat cada 3 s, BlockReport,
// BlockReceived, y ejecución de los comandos recibidos (US-14, US-15, US-55, US-57).
//
// Si el NameNode activo no responde o es standby (Unavailable), prueba la
// siguiente dirección de NAMENODE_ADDRS. Tras un failover, registered=false
// obliga a volver a Register + BlockReport.
package datanode

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"
	namenodev1 "dfsha/services/gen/namenode/v1"
	"dfsha/services/internal/common"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	defaultCapacityBytes   = 100 << 30 // 100 GiB si no hay DATANODE_CAPACITY_BYTES
	reconnectWait          = 2 * time.Second
	defaultReportEvery     = 10 * time.Minute
	commandTimeout         = 2 * time.Minute
	blockReceivedTimeout   = 5 * time.Second
)

// BlockEvent es un bloque que acaba de guardarse en disco.
type BlockEvent struct {
	ID       string
	Checksum string
	Size     uint64
}

// Reporter habla con el NameNode en segundo plano.
type Reporter struct {
	nodeID   string
	rack     string
	host     string
	port     uint32
	addrs    []string
	storage  *Storage
	interval time.Duration
	capacity uint64

	mu        sync.Mutex
	conn      *grpc.ClientConn
	client    namenodev1.NameNodeClient
	current   string
	pending   []BlockEvent
	lastReport time.Time
}

// NewReporter crea el cliente. ADVERTISE_ADDR es lo que el NameNode le entrega
// al cliente (en AWS, la IP pública:50051).
func NewReporter(cfg common.Config, storage *Storage) (*Reporter, error) {
	host, port, err := common.AdvertiseHostPort(cfg.AdvertiseAddr, cfg.GRPCPort)
	if err != nil {
		return nil, err
	}
	if len(cfg.NameNodeAddrs) == 0 {
		return nil, errNoNameNode
	}
	interval := 3 * time.Second
	return &Reporter{
		nodeID:   cfg.NodeID,
		rack:     cfg.Rack,
		host:     host,
		port:     port,
		addrs:    append([]string(nil), cfg.NameNodeAddrs...),
		storage:  storage,
		interval: interval,
		capacity: defaultCapacityBytes,
	}, nil
}

var errNoNameNode = errString("NAMENODE_ADDRS is empty")

type errString string

func (e errString) Error() string { return string(e) }

// NotifyBlock encola un BlockReceived. No bloquea WriteBlock.
func (r *Reporter) NotifyBlock(ev BlockEvent) {
	if ev.ID == "" {
		return
	}
	r.mu.Lock()
	r.pending = append(r.pending, ev)
	r.mu.Unlock()
}

// Run registra el DataNode y mantiene el heartbeat hasta que ctx termina.
func (r *Reporter) Run(ctx context.Context) {
	log.Printf("NameNode reporter starting id=%s advertise=%s:%d namenodes=%v",
		r.nodeID, r.host, r.port, r.addrs)
	for ctx.Err() == nil {
		if err := r.ensureClient(ctx); err != nil {
			r.sleep(ctx, reconnectWait)
			continue
		}
		if err := r.registerAndReport(ctx); err != nil {
			log.Printf("register/block report failed (%s): %v", r.current, err)
			r.drop()
			r.sleep(ctx, reconnectWait)
			continue
		}
		if err := r.loop(ctx); err != nil && ctx.Err() == nil {
			log.Printf("heartbeat loop ended (%s): %v", r.current, err)
		}
		r.drop()
		r.sleep(ctx, reconnectWait)
	}
	r.drop()
}

func (r *Reporter) loop(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	if err := r.flushReceived(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.heartbeat(ctx); err != nil {
				return err
			}
			if err := r.flushReceived(ctx); err != nil {
				return err
			}
			if time.Since(r.lastReport) >= defaultReportEvery {
				if err := r.blockReport(ctx); err != nil {
					return err
				}
			}
		}
	}
}

func (r *Reporter) ensureClient(ctx context.Context) error {
	r.mu.Lock()
	if r.client != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	var last error
	for _, addr := range r.addrs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := grpc.DialContext(dialCtx, addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock())
		cancel()
		if err != nil {
			last = err
			continue
		}
		r.mu.Lock()
		r.conn = conn
		r.client = namenodev1.NewNameNodeClient(conn)
		r.current = addr
		r.mu.Unlock()
		log.Printf("connected to NameNode %s", addr)
		return nil
	}
	if last == nil {
		last = errNoNameNode
	}
	return last
}

func (r *Reporter) drop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn, r.client, r.current = nil, nil, ""
}

func (r *Reporter) cli() namenodev1.NameNodeClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

func (r *Reporter) registerAndReport(ctx context.Context) error {
	client := r.cli()
	if client == nil {
		return errNoNameNode
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.Register(ctx, &namenodev1.RegisterRequest{
		Endpoint: &datanodev1.DataNodeEndpoint{
			Id: r.nodeID, Host: r.host, Port: r.port, Rack: r.rack,
		},
		CapacityBytes: r.capacity,
	})
	if err != nil {
		return err
	}
	if secs := resp.GetHeartbeatIntervalSeconds(); secs > 0 {
		r.interval = time.Duration(secs) * time.Second
	}
	if resp.GetDatanodeId() != "" && resp.GetDatanodeId() != r.nodeID {
		log.Printf("NameNode assigned id %s (announced %s)", resp.GetDatanodeId(), r.nodeID)
	}
	log.Printf("registered with NameNode %s as %s (heartbeat %s)", r.current, r.nodeID, r.interval)
	return r.blockReport(ctx)
}

func (r *Reporter) heartbeat(ctx context.Context) error {
	client := r.cli()
	if client == nil {
		return errNoNameNode
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := client.Heartbeat(ctx, &namenodev1.HeartbeatRequest{
		DatanodeId:    r.nodeID,
		CapacityBytes: r.capacity,
		UsedBytes:     r.storage.UsedBytes(),
	})
	if err != nil {
		return err
	}
	if !resp.GetRegistered() {
		return errString("NameNode asked to re-register")
	}
	for _, cmd := range resp.GetCommands() {
		r.execCommand(cmd)
	}
	return nil
}

func (r *Reporter) blockReport(ctx context.Context) error {
	client := r.cli()
	if client == nil {
		return errNoNameNode
	}
	blocks, err := r.storage.List()
	if err != nil {
		return err
	}
	info := make([]*namenodev1.BlockInfo, 0, len(blocks))
	for _, b := range blocks {
		info = append(info, &namenodev1.BlockInfo{
			BlockId: b.ID, Size: b.Size, ChecksumSha256: b.ChecksumSHA256,
		})
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := client.BlockReport(ctx, &namenodev1.BlockReportRequest{
		DatanodeId: r.nodeID, Blocks: info,
	})
	if err != nil {
		return err
	}
	for _, id := range resp.GetOrphanBlockIds() {
		if _, delErr := r.storage.Delete(id); delErr != nil {
			log.Printf("delete orphan %s: %v", id, delErr)
		} else {
			log.Printf("deleted orphan block %s", id)
		}
	}
	r.lastReport = time.Now()
	return nil
}

func (r *Reporter) flushReceived(ctx context.Context) error {
	r.mu.Lock()
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	client := r.cli()
	if client == nil {
		r.mu.Lock()
		r.pending = append(pending, r.pending...)
		r.mu.Unlock()
		return errNoNameNode
	}
	var failed []BlockEvent
	for _, ev := range pending {
		if ctx.Err() != nil {
			failed = append(failed, ev)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, blockReceivedTimeout)
		_, err := client.BlockReceived(cctx, &namenodev1.BlockReceivedRequest{
			DatanodeId: r.nodeID,
			Block: &namenodev1.BlockInfo{
				BlockId: ev.ID, Size: ev.Size, ChecksumSha256: ev.Checksum,
			},
		})
		cancel()
		if err != nil {
			if status.Code(err) == codes.NotFound {
				continue // bloque huérfano
			}
			log.Printf("BlockReceived %s: %v", ev.ID, err)
			failed = append(failed, ev)
		}
	}
	if len(failed) > 0 {
		r.mu.Lock()
		r.pending = append(failed, r.pending...)
		r.mu.Unlock()
	}
	return nil
}

func (r *Reporter) execCommand(cmd *namenodev1.Command) {
	if cmd == nil {
		return
	}
	switch x := cmd.GetAction().(type) {
	case *namenodev1.Command_Delete:
		for _, id := range x.Delete.GetBlockIds() {
			if _, err := r.storage.Delete(id); err != nil {
				log.Printf("DeleteCommand %s: %v", id, err)
			}
		}
	case *namenodev1.Command_Replicate:
		go r.replicate(x.Replicate)
	}
}

func (r *Reporter) replicate(cmd *namenodev1.ReplicateCommand) {
	if cmd == nil || cmd.GetBlockId() == "" || len(cmd.GetTargets()) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := replicateTo(ctx, r.storage, cmd.GetBlockId(), cmd.GetTargets()); err != nil {
		log.Printf("ReplicateCommand %s: %v", cmd.GetBlockId(), err)
	} else {
		log.Printf("replicated %s to %d targets", cmd.GetBlockId(), len(cmd.GetTargets()))
	}
}

func replicateTo(ctx context.Context, storage *Storage, blockID string, targets []*datanodev1.DataNodeEndpoint) error {
	f, info, err := storage.Open(blockID)
	if err != nil {
		return err
	}
	defer f.Close()

	header := &datanodev1.WriteBlockHeader{
		BlockId:        info.ID,
		Size:           info.Size,
		ChecksumSha256: info.ChecksumSHA256,
		Pipeline:       targets,
	}
	pw, err := openPipeline(ctx, header)
	if err != nil {
		return err
	}
	defer pw.Close()

	buf := make([]byte, defaultReadChunkSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if sendErr := pw.Send(&datanodev1.WriteBlockRequest{
				Payload: &datanodev1.WriteBlockRequest_Chunk{Chunk: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				return sendErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	_, err = pw.Finish()
	return err
}

func (r *Reporter) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
