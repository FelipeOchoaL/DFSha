package datanode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	datanodev1 "dfsha/services/gen/datanode/v1"
	"dfsha/services/internal/common"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type testNode struct {
	storage  *Storage
	client   datanodev1.DataNodeClient
	endpoint *datanodev1.DataNodeEndpoint
}

func startTestNode(t *testing.T, id string, override datanodev1.DataNodeServer) testNode {
	t.Helper()
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	if override == nil {
		override = NewGRPCServer(id, storage)
	}
	datanodev1.RegisterDataNodeServer(server, override)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return testNode{storage: storage, client: datanodev1.NewDataNodeClient(conn), endpoint: &datanodev1.DataNodeEndpoint{Id: id, Host: "127.0.0.1", Port: uint32(listener.Addr().(*net.TCPAddr).Port), Rack: "rack-a"}}
}

func blockHeader(id string, data []byte, pipeline ...*datanodev1.DataNodeEndpoint) *datanodev1.WriteBlockHeader {
	return &datanodev1.WriteBlockHeader{BlockId: id, Size: uint64(len(data)), ChecksumSha256: common.SHA256Hex(data), Pipeline: pipeline}
}

func headerMessage(header *datanodev1.WriteBlockHeader) *datanodev1.WriteBlockRequest {
	return &datanodev1.WriteBlockRequest{Payload: &datanodev1.WriteBlockRequest_Header{Header: header}}
}

func chunkMessage(data []byte) *datanodev1.WriteBlockRequest {
	return &datanodev1.WriteBlockRequest{Payload: &datanodev1.WriteBlockRequest_Chunk{Chunk: data}}
}

func writeRequests(client datanodev1.DataNodeClient, requests ...*datanodev1.WriteBlockRequest) (*datanodev1.WriteBlockResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.WriteBlock(ctx)
	if err != nil {
		return nil, err
	}
	for _, req := range requests {
		if err := stream.Send(req); err != nil {
			if err == io.EOF {
				return stream.CloseAndRecv()
			}
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

func writeData(client datanodev1.DataNodeClient, header *datanodev1.WriteBlockHeader, data []byte) (*datanodev1.WriteBlockResponse, error) {
	requests := []*datanodev1.WriteBlockRequest{headerMessage(header)}
	for len(data) > 0 {
		n := len(data)
		if n > 64*1024 {
			n = 64 * 1024
		}
		requests = append(requests, chunkMessage(data[:n]))
		data = data[n:]
	}
	return writeRequests(client, requests...)
}

func readData(client datanodev1.DataNodeClient, req *datanodev1.ReadBlockRequest) (*datanodev1.ReadBlockHeader, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ReadBlock(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, nil, err
	}
	header := first.GetHeader()
	if header == nil {
		return nil, nil, fmt.Errorf("first read message was not a header")
	}
	var data bytes.Buffer
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return header, data.Bytes(), nil
		}
		if err != nil {
			return nil, nil, err
		}
		chunk, ok := msg.GetPayload().(*datanodev1.ReadBlockResponse_Chunk)
		if !ok || len(chunk.Chunk) > 64*1024 {
			return nil, nil, fmt.Errorf("invalid chunk after read header")
		}
		data.Write(chunk.Chunk)
	}
}

func TestGRPCWriteReadAndRanges(t *testing.T) {
	node := startTestNode(t, "dn-1", nil)
	data := bytes.Repeat([]byte("distributed blocks\n"), 18000)
	header := blockHeader("block-1", data)
	response, err := writeData(node.client, header, data)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetBlockId() != header.BlockId || response.GetBytesWritten() != uint64(len(data)) || response.GetChecksumSha256() != header.ChecksumSha256 || !reflect.DeepEqual(response.GetReplicaIds(), []string{"dn-1"}) {
		t.Fatalf("unexpected ACK: %v", response)
	}
	for _, tc := range []struct {
		name           string
		offset, length uint64
	}{
		{"full", 0, 0}, {"slice", 100, 73}, {"tail", 100, 0}, {"clamped", uint64(len(data) - 10), 100}, {"at-end", uint64(len(data)), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, got, err := readData(node.client, &datanodev1.ReadBlockRequest{BlockId: header.BlockId, Offset: tc.offset, Length: tc.length})
			if err != nil {
				t.Fatal(err)
			}
			end := uint64(len(data))
			if tc.length > 0 && tc.length < end-tc.offset {
				end = tc.offset + tc.length
			}
			if !bytes.Equal(got, data[tc.offset:end]) || head.GetSize() != uint64(len(data)) || head.GetChecksumSha256() != header.ChecksumSha256 {
				t.Fatal("range or header differs from original block")
			}
		})
	}
	_, _, err = readData(node.client, &datanodev1.ReadBlockRequest{BlockId: header.BlockId, Offset: uint64(len(data) + 1)})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid offset: %v", err)
	}
}

func TestGRPCRejectsInvalidWritesWithoutSaving(t *testing.T) {
	node := startTestNode(t, "dn-1", nil)
	data := []byte("abc")
	badSize := blockHeader("block-size", data)
	badSize.Size++
	tooMuch := blockHeader("block-excess", data)
	tooMuch.Size--
	badHash := blockHeader("block-hash", data)
	badHash.ChecksumSha256 = common.SHA256Hex([]byte("different"))
	badPipeline := blockHeader("block-cycle", data, node.endpoint)
	for _, tc := range []struct {
		name     string
		requests []*datanodev1.WriteBlockRequest
	}{
		{"empty-stream", nil},
		{"chunk-first", []*datanodev1.WriteBlockRequest{chunkMessage(data)}},
		{"missing-payload", []*datanodev1.WriteBlockRequest{{}}},
		{"second-header", []*datanodev1.WriteBlockRequest{headerMessage(blockHeader("block-2", data)), headerMessage(blockHeader("block-2", data))}},
		{"size-short", []*datanodev1.WriteBlockRequest{headerMessage(badSize), chunkMessage(data)}},
		{"size-excess", []*datanodev1.WriteBlockRequest{headerMessage(tooMuch), chunkMessage(data)}},
		{"checksum", []*datanodev1.WriteBlockRequest{headerMessage(badHash), chunkMessage(data)}},
		{"path-traversal", []*datanodev1.WriteBlockRequest{headerMessage(blockHeader("../escape", data))}},
		{"pipeline-cycle", []*datanodev1.WriteBlockRequest{headerMessage(badPipeline)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := writeRequests(node.client, tc.requests...)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
			blocks, err := node.storage.List()
			if err != nil || len(blocks) != 0 {
				t.Fatalf("failed write became visible: %v, %v", blocks, err)
			}
			entries, err := os.ReadDir(node.storage.dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files leaked: %v, %v", entries, err)
			}
		})
	}
}

func TestGRPCEmptyBlockAndDelete(t *testing.T) {
	node := startTestNode(t, "dn-1", nil)
	if _, err := writeRequests(node.client, headerMessage(blockHeader("empty", nil)), chunkMessage(nil)); err != nil {
		t.Fatal(err)
	}
	_, got, err := readData(node.client, &datanodev1.ReadBlockRequest{BlockId: "empty"})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty read: %v %q", err, got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, want := range []bool{true, false} {
		response, err := node.client.DeleteBlock(ctx, &datanodev1.DeleteBlockRequest{BlockId: "empty"})
		if err != nil || response.GetDeleted() != want {
			t.Fatalf("delete: %v %v, want %v", response, err, want)
		}
	}
	_, _, err = readData(node.client, &datanodev1.ReadBlockRequest{BlockId: "empty"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing read: %v", err)
	}
}

func TestGRPCDetectsCorruption(t *testing.T) {
	for _, kind := range []string{"data", "metadata", "size"} {
		t.Run(kind, func(t *testing.T) {
			node := startTestNode(t, "dn-1", nil)
			data := []byte("intact")
			if _, err := writeData(node.client, blockHeader("block-1", data), data); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "data":
				err = os.WriteFile(node.storage.blockPath("block-1"), []byte("broken"), 0600)
			case "metadata":
				err = os.WriteFile(node.storage.metaPath("block-1"), []byte("{"), 0600)
			case "size":
				err = os.WriteFile(node.storage.metaPath("block-1"), []byte(fmt.Sprintf(`{"id":"block-1","size":999,"checksum_sha256":"%s"}`, common.SHA256Hex(data))), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = readData(node.client, &datanodev1.ReadBlockRequest{BlockId: "block-1"})
			if status.Code(err) != codes.DataLoss {
				t.Fatalf("corruption was not DataLoss: %v", err)
			}
		})
	}
}

func TestPipelineThreeNodes(t *testing.T) {
	a := startTestNode(t, "dn-1", nil)
	b := startTestNode(t, "dn-2", nil)
	c := startTestNode(t, "dn-3", nil)
	data := bytes.Repeat([]byte("pipeline-data\n"), 20000)
	response, err := writeData(a.client, blockHeader("replicated", data, b.endpoint, c.endpoint), data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.GetReplicaIds(), []string{"dn-1", "dn-2", "dn-3"}) {
		t.Fatalf("replica order: %v", response.GetReplicaIds())
	}
	for _, node := range []testNode{a, b, c} {
		_, got, err := readData(node.client, &datanodev1.ReadBlockRequest{BlockId: "replicated"})
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("node %s differs: %v", node.endpoint.Id, err)
		}
		info, err := node.storage.Verify("replicated")
		if err != nil || info.ChecksumSHA256 != common.SHA256Hex(data) {
			t.Fatalf("disk checksum differs: %+v %v", info, err)
		}
	}
}

type failingDataNode struct {
	datanodev1.UnimplementedDataNodeServer
}

func (failingDataNode) WriteBlock(datanodev1.DataNode_WriteBlockServer) error {
	return status.Error(codes.Unavailable, "dn-3 offline")
}

func TestPipelineFailurePreservesPreviousBlock(t *testing.T) {
	a := startTestNode(t, "dn-1", nil)
	b := startTestNode(t, "dn-2", nil)
	c := startTestNode(t, "dn-3", failingDataNode{})
	data := []byte("previously acknowledged block")
	if _, err := writeData(a.client, blockHeader("existing", data), data); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"existing", "new-block"} {
		_, err := writeData(a.client, blockHeader(id, data, b.endpoint, c.endpoint), data)
		if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "dn-") {
			t.Fatalf("expected failing-node error: %v", err)
		}
		blocks, err := b.storage.List()
		if err != nil || len(blocks) != 0 {
			t.Fatalf("intermediate node published a failed write: %v %v", blocks, err)
		}
	}
	blocks, err := a.storage.List()
	if err != nil || len(blocks) != 1 || blocks[0].ID != "existing" {
		t.Fatalf("failed retry changed published blocks: %v %v", blocks, err)
	}
	_, got, err := readData(a.client, &datanodev1.ReadBlockRequest{BlockId: "existing"})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("failed pipeline destroyed previous data: %v", err)
	}
}

func TestGRPCConcurrentWrites(t *testing.T) {
	node := startTestNode(t, "dn-1", nil)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%d", i)
			data := bytes.Repeat([]byte(id), 1000)
			_, err := writeData(node.client, blockHeader(id, data), data)
			if err == nil {
				_, got, readErr := readData(node.client, &datanodev1.ReadBlockRequest{BlockId: id})
				err = readErr
				if err == nil && !bytes.Equal(got, data) {
					err = fmt.Errorf("data differs for %s", id)
				}
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	blocks, err := node.storage.List()
	if err != nil || len(blocks) != 12 {
		t.Fatalf("concurrent blocks: %d %v", len(blocks), err)
	}
}
