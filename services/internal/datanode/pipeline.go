package datanode

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type pipelineWriter struct {
	conn   *grpc.ClientConn
	stream datanodev1.DataNode_WriteBlockClient
	cancel context.CancelFunc
	header *datanodev1.WriteBlockHeader
	nextID string
}

func openPipeline(ctx context.Context, header *datanodev1.WriteBlockHeader) (*pipelineWriter, error) {
	next := header.GetPipeline()[0]
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := grpc.DialContext(ctx, endpointAddress(next), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to node %s: %w", next.GetId(), err)
	}
	p := &pipelineWriter{conn: conn, cancel: cancel, header: header, nextID: next.GetId()}
	p.stream, err = datanodev1.NewDataNodeClient(conn).WriteBlock(ctx)
	if err == nil {
		err = p.stream.Send(&datanodev1.WriteBlockRequest{
			Payload: &datanodev1.WriteBlockRequest_Header{Header: &datanodev1.WriteBlockHeader{
				BlockId: header.GetBlockId(), Size: header.GetSize(),
				ChecksumSha256: header.GetChecksumSha256(), Pipeline: header.GetPipeline()[1:],
			}},
		})
	}
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("open WriteBlock to node %s: %w", next.GetId(), err)
	}
	return p, nil
}

func (p *pipelineWriter) Send(req *datanodev1.WriteBlockRequest) error {
	if err := p.stream.Send(req); err != nil {
		return fmt.Errorf("stream to node %s: %w", p.nextID, err)
	}
	return nil
}

func (p *pipelineWriter) Finish() ([]string, error) {
	response, err := p.stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("ACK from node %s: %w", p.nextID, err)
	}
	if response.GetBlockId() != p.header.GetBlockId() || response.GetBytesWritten() != p.header.GetSize() ||
		response.GetChecksumSha256() != p.header.GetChecksumSha256() || len(response.GetReplicaIds()) != len(p.header.GetPipeline()) {
		return nil, fmt.Errorf("invalid ACK from node %s", p.nextID)
	}
	for i, endpoint := range p.header.GetPipeline() {
		if response.GetReplicaIds()[i] != endpoint.GetId() {
			return nil, fmt.Errorf("incorrect replica order in ACK from node %s", p.nextID)
		}
	}
	return response.GetReplicaIds(), nil
}

func (p *pipelineWriter) Close() {
	p.cancel()
	_ = p.conn.Close()
}

func validatePipeline(nodeID string, pipeline []*datanodev1.DataNodeEndpoint) error {
	seen := map[string]bool{nodeID: true}
	for _, endpoint := range pipeline {
		if endpoint.GetId() == "" || endpointAddress(endpoint) == "" {
			return fmt.Errorf("pipeline endpoint must include id, host and a valid port")
		}
		if seen[endpoint.GetId()] {
			return fmt.Errorf("duplicate pipeline node %s", endpoint.GetId())
		}
		seen[endpoint.GetId()] = true
	}
	return nil
}

func endpointAddress(endpoint *datanodev1.DataNodeEndpoint) string {
	if endpoint.GetHost() == "" || endpoint.GetPort() == 0 || endpoint.GetPort() > 65535 {
		return ""
	}
	return net.JoinHostPort(endpoint.GetHost(), strconv.FormatUint(uint64(endpoint.GetPort()), 10))
}
