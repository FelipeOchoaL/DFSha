// Transporte gRPC de la alta disponibilidad (proto/namenode.proto, servicio NameNodeHA).
//
// Une la lógica de ha.go y failover.go con el contrato del proto:
//   - HAServer atiende ReplicateEditLog y Ping en el NameNode que los recibe.
//   - GRPCPeer es el cliente hacia el otro NameNode: implementa StandbyClient
//     (replicar el edit log) y ActiveProbe (Ping).
//
// Las entradas del edit log viajan como EditLogEntry. La hora va en milisegundos
// (timestamp_unix_ms); por eso stamp() ya trunca a milisegundos y el estado del
// standby es idéntico al del activo.
package namenode

import (
	"context"
	"errors"
	"fmt"
	"time"

	namenodev1 "dfsha/services/gen/namenode/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// replicateBatch es el máximo de entradas por mensaje: mantiene cada RPC muy por
// debajo del límite de 4 MiB de gRPC incluso al poner al día a un standby atrasado.
const replicateBatch = 500

// --- conversión EditEntry <-> proto ---

// entryToProto convierte una entrada del edit log al mensaje del proto.
func entryToProto(e EditEntry) (*namenodev1.EditLogEntry, error) {
	p := &namenodev1.EditLogEntry{
		Txid:            uint64(e.TxID),
		TimestampUnixMs: e.Time.UnixMilli(),
	}
	switch e.Op {
	case OpMkdir:
		p.Op = &namenodev1.EditLogEntry_Mkdir{Mkdir: &namenodev1.MkdirOp{
			Path: e.Path, Owner: e.Owner, Group: e.Group, Mode: e.Mode, Parents: e.Parents,
		}}
	case OpRmdir:
		p.Op = &namenodev1.EditLogEntry_Rmdir{Rmdir: &namenodev1.RmdirOp{Path: e.Path}}
	case OpDelete:
		p.Op = &namenodev1.EditLogEntry_Delete{Delete: &namenodev1.DeleteOp{Path: e.Path}}
	case OpAllocate:
		p.Op = &namenodev1.EditLogEntry_AllocateBlock{AllocateBlock: &namenodev1.AllocateBlockOp{
			Path: e.Path, BlockIndex: uint32(e.BlockIndex), BlockId: e.BlockID,
			UploadId: e.UploadID, NewUpload: e.NewUpload,
			Owner: e.Owner, Group: e.Group, Mode: e.Mode, Size: uint64(e.Size),
		}}
	case OpCommit:
		p.Op = &namenodev1.EditLogEntry_CommitBlock{CommitBlock: &namenodev1.CommitBlockOp{
			Path: e.Path, BlockId: e.BlockID, Size: uint64(e.Size),
			ChecksumSha256: e.Checksum, UploadId: e.UploadID,
		}}
	case OpFinalize:
		p.Op = &namenodev1.EditLogEntry_FinalizeFile{FinalizeFile: &namenodev1.FinalizeFileOp{
			Path: e.Path, TotalSize: uint64(e.Size), UploadId: e.UploadID,
		}}
	case OpChmod:
		p.Op = &namenodev1.EditLogEntry_Chmod{Chmod: &namenodev1.ChmodOp{Path: e.Path, Mode: e.Mode}}
	default:
		return nil, fmt.Errorf("operación desconocida %q en la entrada %d", e.Op, e.TxID)
	}
	return p, nil
}

// entryFromProto convierte un mensaje del proto en una entrada del edit log.
func entryFromProto(p *namenodev1.EditLogEntry) (EditEntry, error) {
	e := EditEntry{
		TxID: int64(p.GetTxid()),
		Time: time.UnixMilli(p.GetTimestampUnixMs()).UTC(),
	}
	switch op := p.GetOp().(type) {
	case *namenodev1.EditLogEntry_Mkdir:
		m := op.Mkdir
		e.Op, e.Path, e.Owner, e.Group, e.Mode, e.Parents = OpMkdir, m.GetPath(), m.GetOwner(), m.GetGroup(), m.GetMode(), m.GetParents()
	case *namenodev1.EditLogEntry_Rmdir:
		e.Op, e.Path = OpRmdir, op.Rmdir.GetPath()
	case *namenodev1.EditLogEntry_Delete:
		e.Op, e.Path = OpDelete, op.Delete.GetPath()
	case *namenodev1.EditLogEntry_AllocateBlock:
		a := op.AllocateBlock
		e.Op, e.Path, e.BlockIndex, e.BlockID = OpAllocate, a.GetPath(), int(a.GetBlockIndex()), a.GetBlockId()
		e.UploadID, e.NewUpload = a.GetUploadId(), a.GetNewUpload()
		e.Owner, e.Group, e.Mode, e.Size = a.GetOwner(), a.GetGroup(), a.GetMode(), int64(a.GetSize())
	case *namenodev1.EditLogEntry_CommitBlock:
		c := op.CommitBlock
		e.Op, e.Path, e.BlockID, e.Size = OpCommit, c.GetPath(), c.GetBlockId(), int64(c.GetSize())
		e.Checksum, e.UploadID = c.GetChecksumSha256(), c.GetUploadId()
	case *namenodev1.EditLogEntry_FinalizeFile:
		f := op.FinalizeFile
		e.Op, e.Path, e.Size, e.UploadID = OpFinalize, f.GetPath(), int64(f.GetTotalSize()), f.GetUploadId()
	case *namenodev1.EditLogEntry_Chmod:
		e.Op, e.Path, e.Mode = OpChmod, op.Chmod.GetPath(), op.Chmod.GetMode()
	default:
		return EditEntry{}, fmt.Errorf("la entrada %d no trae ninguna operación", e.TxID)
	}
	return e, nil
}

// --- servidor (NameNode que recibe) ---

// HAServer implementa el servicio gRPC NameNodeHA sobre un RESTServer.
type HAServer struct {
	namenodev1.UnimplementedNameNodeHAServer
	rest *RESTServer
}

// NewHAServer crea el servicio. Se registra con namenodev1.RegisterNameNodeHAServer.
func NewHAServer(rest *RESTServer) *HAServer { return &HAServer{rest: rest} }

// ReplicateEditLog aplica las entradas que envía el activo (solo si este NameNode
// es standby). El resultado va en status del mensaje, no como error gRPC, para que
// el activo distinga un hueco, un par que ya es activo y una divergencia.
func (h *HAServer) ReplicateEditLog(_ context.Context, req *namenodev1.ReplicateEditLogRequest) (*namenodev1.ReplicateEditLogResponse, error) {
	entries := make([]EditEntry, 0, len(req.GetEntries()))
	for _, p := range req.GetEntries() {
		e, err := entryFromProto(p)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		entries = append(entries, e)
	}
	last, err := h.rest.ReplicateEditLog(entries)
	resp := &namenodev1.ReplicateEditLogResponse{LastAppliedTxid: uint64(last)}
	switch {
	case err == nil:
		resp.Status = namenodev1.ReplicateStatus_REPLICATE_STATUS_OK
	case errors.Is(err, ErrReplicationGap):
		resp.Status = namenodev1.ReplicateStatus_REPLICATE_STATUS_GAP
		resp.Message = err.Error()
	case errors.Is(err, ErrNotStandby):
		resp.Status = namenodev1.ReplicateStatus_REPLICATE_STATUS_NOT_STANDBY
		resp.Message = err.Error()
	case errors.Is(err, ErrLogsDiverged):
		resp.Status = namenodev1.ReplicateStatus_REPLICATE_STATUS_DIVERGED
		resp.Message = err.Error()
	default:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return resp, nil
}

// Ping informa quién es este NameNode, si es el activo y su último txid.
func (h *HAServer) Ping(_ context.Context, _ *namenodev1.PingRequest) (*namenodev1.PingResponse, error) {
	info := h.rest.PingInfo()
	return &namenodev1.PingResponse{NodeId: info.NodeID, IsActive: info.Active, LastTxid: uint64(info.LastTxID)}, nil
}

// --- cliente (hacia el otro NameNode) ---

// GRPCPeer es la conexión gRPC con el otro NameNode. Implementa StandbyClient y
// ActiveProbe. La conexión se establece al primer uso y se reintenta sola, así que
// puede crearse aunque el otro NameNode todavía no esté en marcha.
type GRPCPeer struct {
	conn   *grpc.ClientConn
	client namenodev1.NameNodeHAClient
	self   *RESTServer
}

// DialPeer prepara la conexión con el NameNode de addr. creds son las credenciales
// de transporte (mTLS); si es nil se usa un canal sin cifrar (solo desarrollo).
// self es el NameNode local: se usa para informar su id y su último txid en cada Ping.
func DialPeer(addr string, creds credentials.TransportCredentials, self *RESTServer) (*GRPCPeer, error) {
	if addr == "" {
		return nil, fmt.Errorf("falta la dirección del otro NameNode")
	}
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	// Reconexion rapida: mientras el canal espera para reintentar, las llamadas fallan al
	// instante; con el backoff por defecto (hasta 2 min) un par que acaba de arrancar
	// seguiria pareciendo caido durante demasiado tiempo.
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond,
				Multiplier: 1.3,
				Jitter:     0.2,
				MaxDelay:   500 * time.Millisecond,
			},
			MinConnectTimeout: time.Second,
		}))
	if err != nil {
		return nil, fmt.Errorf("preparar la conexión con el NameNode %s: %w", addr, err)
	}
	return &GRPCPeer{conn: conn, client: namenodev1.NewNameNodeHAClient(conn), self: self}, nil
}

// Close cierra la conexión.
func (p *GRPCPeer) Close() error { return p.conn.Close() }

// ReplicateEditLog envía las entradas al standby, en lotes. Devuelve el último txid
// que el standby tiene aplicado. Los resultados distintos de OK se devuelven como
// errores que cumplen errors.Is con ErrReplicationGap, ErrNotStandby o ErrLogsDiverged.
func (p *GRPCPeer) ReplicateEditLog(ctx context.Context, entries []EditEntry) (int64, error) {
	if len(entries) == 0 {
		return p.send(ctx, nil)
	}
	var last int64
	for start := 0; start < len(entries); start += replicateBatch {
		end := start + replicateBatch
		if end > len(entries) {
			end = len(entries)
		}
		batch := make([]*namenodev1.EditLogEntry, 0, end-start)
		for _, e := range entries[start:end] {
			pe, err := entryToProto(e)
			if err != nil {
				return 0, err
			}
			batch = append(batch, pe)
		}
		var err error
		if last, err = p.send(ctx, batch); err != nil {
			return last, err
		}
	}
	return last, nil
}

func (p *GRPCPeer) send(ctx context.Context, batch []*namenodev1.EditLogEntry) (int64, error) {
	resp, err := p.client.ReplicateEditLog(ctx, &namenodev1.ReplicateEditLogRequest{Entries: batch})
	if err != nil {
		return 0, fmt.Errorf("ReplicateEditLog: %w", err)
	}
	last := int64(resp.GetLastAppliedTxid())
	switch resp.GetStatus() {
	case namenodev1.ReplicateStatus_REPLICATE_STATUS_OK:
		return last, nil
	case namenodev1.ReplicateStatus_REPLICATE_STATUS_GAP:
		return last, fmt.Errorf("%w: %s", ErrReplicationGap, resp.GetMessage())
	case namenodev1.ReplicateStatus_REPLICATE_STATUS_NOT_STANDBY:
		return last, fmt.Errorf("%w: %s", ErrNotStandby, resp.GetMessage())
	case namenodev1.ReplicateStatus_REPLICATE_STATUS_DIVERGED:
		return last, fmt.Errorf("%w: %s", ErrLogsDiverged, resp.GetMessage())
	default:
		return last, fmt.Errorf("respuesta desconocida del standby: %v", resp.GetStatus())
	}
}

// Ping consulta al otro NameNode.
func (p *GRPCPeer) Ping(ctx context.Context) (PingInfo, error) {
	mine := p.self.PingInfo()
	resp, err := p.client.Ping(ctx, &namenodev1.PingRequest{NodeId: mine.NodeID, LastTxid: uint64(mine.LastTxID)})
	if err != nil {
		return PingInfo{}, fmt.Errorf("Ping: %w", err)
	}
	return PingInfo{NodeID: resp.GetNodeId(), Active: resp.GetIsActive(), LastTxID: int64(resp.GetLastTxid())}, nil
}
