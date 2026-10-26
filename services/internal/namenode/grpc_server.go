// Servidor gRPC del NameNode: implementa el servicio NameNode de namenode.proto
// (Register, Heartbeat, BlockReport, BlockReceived) traduciendo protobuf a
// llamadas del Registry. Historias US-22, US-23, US-58.
//
// Un NameNode standby rechaza todo con Unavailable: los DataNodes deben
// reportar al activo (reintentan con la siguiente dirección de NAMENODE_ADDRS).
package namenode

import (
	"context"
	"errors"

	datanodev1 "dfsha/services/gen/datanode/v1"
	namenodev1 "dfsha/services/gen/namenode/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DataNodeService es el servicio gRPC que atiende a los DataNodes.
type DataNodeService struct {
	namenodev1.UnimplementedNameNodeServer
	registry *Registry
	rest     *RESTServer
}

// NewDataNodeService crea el servicio. rest se usa para saber si este NameNode
// es standby (puede ser nil en pruebas).
func NewDataNodeService(registry *Registry, rest *RESTServer) *DataNodeService {
	return &DataNodeService{registry: registry, rest: rest}
}

func (s *DataNodeService) rejectIfStandby() error {
	if s.rest != nil && s.rest.IsStandby() {
		return status.Error(codes.Unavailable, "este NameNode es standby; reportar al activo")
	}
	return nil
}

func (s *DataNodeService) Register(_ context.Context, req *namenodev1.RegisterRequest) (*namenodev1.RegisterResponse, error) {
	if err := s.rejectIfStandby(); err != nil {
		return nil, err
	}
	ep := req.GetEndpoint()
	if ep == nil {
		return nil, status.Error(codes.InvalidArgument, "endpoint es obligatorio")
	}
	id, err := s.registry.Register(fromEndpoint(ep), req.GetCapacityBytes())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	secs := uint32(s.registry.HeartbeatInterval().Seconds())
	if secs == 0 {
		secs = 1
	}
	return &namenodev1.RegisterResponse{DatanodeId: id, HeartbeatIntervalSeconds: secs}, nil
}

func (s *DataNodeService) Heartbeat(_ context.Context, req *namenodev1.HeartbeatRequest) (*namenodev1.HeartbeatResponse, error) {
	if err := s.rejectIfStandby(); err != nil {
		return nil, err
	}
	if req.GetDatanodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "datanode_id es obligatorio")
	}
	cmds, registered := s.registry.Heartbeat(req.GetDatanodeId(), req.GetCapacityBytes(), req.GetUsedBytes())
	resp := &namenodev1.HeartbeatResponse{Registered: registered}
	for _, c := range cmds {
		if p := toProtoCommand(c); p != nil {
			resp.Commands = append(resp.Commands, p)
		}
	}
	return resp, nil
}

func (s *DataNodeService) BlockReport(_ context.Context, req *namenodev1.BlockReportRequest) (*namenodev1.BlockReportResponse, error) {
	if err := s.rejectIfStandby(); err != nil {
		return nil, err
	}
	entries := make([]BlockReportEntry, 0, len(req.GetBlocks()))
	for _, b := range req.GetBlocks() {
		entries = append(entries, fromBlockInfo(b))
	}
	orphans, err := s.registry.BlockReport(req.GetDatanodeId(), entries)
	if errors.Is(err, ErrUnknownDataNode) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &namenodev1.BlockReportResponse{OrphanBlockIds: orphans}, nil
}

func (s *DataNodeService) BlockReceived(_ context.Context, req *namenodev1.BlockReceivedRequest) (*namenodev1.BlockReceivedResponse, error) {
	if err := s.rejectIfStandby(); err != nil {
		return nil, err
	}
	if req.GetBlock() == nil || req.GetBlock().GetBlockId() == "" {
		return nil, status.Error(codes.InvalidArgument, "block.block_id es obligatorio")
	}
	err := s.registry.BlockReceived(req.GetDatanodeId(), fromBlockInfo(req.GetBlock()))
	switch {
	case err == nil:
		return &namenodev1.BlockReceivedResponse{}, nil
	case errors.Is(err, ErrUnknownDataNode):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrBlockNotFound):
		// Bloque de un archivo ya borrado o de otro NameNode: se limpia en el
		// próximo block report. No es un error del DataNode.
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrChecksumMismatch):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	default:
		return nil, status.Error(codes.Internal, err.Error())
	}
}

// ---------------------------------------------------------------------------
// Conversión protobuf <-> tipos del paquete
// ---------------------------------------------------------------------------

func fromEndpoint(ep *datanodev1.DataNodeEndpoint) PipelineNode {
	return PipelineNode{ID: ep.GetId(), Host: ep.GetHost(), Port: ep.GetPort(), Rack: ep.GetRack()}
}

func toEndpoint(n PipelineNode) *datanodev1.DataNodeEndpoint {
	return &datanodev1.DataNodeEndpoint{Id: n.ID, Host: n.Host, Port: n.Port, Rack: n.Rack}
}

func fromBlockInfo(b *namenodev1.BlockInfo) BlockReportEntry {
	return BlockReportEntry{ID: b.GetBlockId(), Size: int64(b.GetSize()), Checksum: b.GetChecksumSha256()}
}

func toProtoCommand(c Command) *namenodev1.Command {
	switch {
	case c.Replicate != nil:
		targets := make([]*datanodev1.DataNodeEndpoint, 0, len(c.Replicate.Targets))
		for _, t := range c.Replicate.Targets {
			targets = append(targets, toEndpoint(t))
		}
		return &namenodev1.Command{Action: &namenodev1.Command_Replicate{
			Replicate: &namenodev1.ReplicateCommand{BlockId: c.Replicate.BlockID, Targets: targets},
		}}
	case c.Delete != nil:
		return &namenodev1.Command{Action: &namenodev1.Command_Delete{
			Delete: &namenodev1.DeleteCommand{BlockIds: c.Delete.BlockIDs},
		}}
	}
	return nil
}
