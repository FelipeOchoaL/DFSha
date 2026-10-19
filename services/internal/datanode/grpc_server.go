package datanode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"

	datanodev1 "dfsha/services/gen/datanode/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultReadChunkSize = 64 * 1024

type GRPCServer struct {
	datanodev1.UnimplementedDataNodeServer

	nodeID   string
	storage  *Storage
	chunkLen int
}

func NewGRPCServer(nodeID string, storage *Storage) *GRPCServer {
	return &GRPCServer{
		nodeID:   nodeID,
		storage:  storage,
		chunkLen: defaultReadChunkSize,
	}
}

func (s *GRPCServer) WriteBlock(stream datanodev1.DataNode_WriteBlockServer) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "first WriteBlock message must be a header")
	}
	if err != nil {
		return err
	}

	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "first WriteBlock message must be a header")
	}
	if header.GetBlockId() == "" {
		return status.Error(codes.InvalidArgument, "block_id is required")
	}
	if header.GetChecksumSha256() == "" {
		return status.Error(codes.InvalidArgument, "checksum_sha256 is required")
	}
	if err := validateBlockID(header.GetBlockId()); err != nil {
		return storageStatus(err)
	}
	if err := validateChecksum(header.GetChecksumSha256()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if header.GetSize() >= math.MaxInt64 {
		return status.Error(codes.InvalidArgument, "block is too large")
	}
	if err := validatePipeline(s.nodeID, header.GetPipeline()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	var next *pipelineWriter
	if len(header.GetPipeline()) > 0 {
		next, err = openPipeline(stream.Context(), header)
		if err != nil {
			return status.Error(codes.Unavailable, err.Error())
		}
		defer next.Close()
	}
	temp, err := os.CreateTemp(s.storage.dir, ".incoming-*.tmp")
	if err != nil {
		return status.Errorf(codes.Internal, "create incoming block: %v", err)
	}
	defer func() {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
	}()
	hash := sha256.New()
	writer := io.MultiWriter(temp, hash)
	var received uint64

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		payload, ok := req.GetPayload().(*datanodev1.WriteBlockRequest_Chunk)
		if !ok {
			return status.Error(codes.InvalidArgument, "messages after the header must be chunks")
		}
		chunk := payload.Chunk
		if uint64(len(chunk)) > header.GetSize()-received {
			return status.Error(codes.InvalidArgument, "received data exceeds declared block size")
		}
		if _, err := writer.Write(chunk); err != nil {
			return status.Errorf(codes.Internal, "write incoming chunk: %v", err)
		}
		received += uint64(len(chunk))
		if next != nil {
			if err := next.Send(req); err != nil {
				return status.Error(codes.Unavailable, err.Error())
			}
		}
	}

	if received != header.GetSize() || hex.EncodeToString(hash.Sum(nil)) != header.GetChecksumSha256() {
		return status.Error(codes.InvalidArgument, "block size or checksum does not match the header")
	}
	replicaIDs := []string{s.nodeID}
	if next != nil {
		ids, err := next.Finish()
		if err != nil {
			return status.Error(codes.Unavailable, err.Error())
		}
		replicaIDs = append(replicaIDs, ids...)
	}
	// Publish locally only after every downstream ACK. Failed replication leaves
	// only temporary data and never deletes an existing copy of the block.
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return status.Errorf(codes.Internal, "rewind incoming block: %v", err)
	}
	info, err := s.storage.Save(
		header.GetBlockId(),
		temp,
		header.GetSize(),
		header.GetChecksumSha256(),
	)
	if err != nil {
		if errors.Is(err, ErrChecksumMismatch) || errors.Is(err, ErrSizeMismatch) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return storageStatus(err)
	}

	return stream.SendAndClose(&datanodev1.WriteBlockResponse{
		BlockId:        info.ID,
		ChecksumSha256: info.ChecksumSHA256,
		BytesWritten:   info.Size,
		ReplicaIds:     replicaIDs,
	})
}

func (s *GRPCServer) ReadBlock(req *datanodev1.ReadBlockRequest, stream datanodev1.DataNode_ReadBlockServer) error {
	if req.GetBlockId() == "" {
		return status.Error(codes.InvalidArgument, "block_id is required")
	}

	file, info, err := s.storage.OpenVerified(req.GetBlockId())
	if err != nil {
		return storageStatus(err)
	}
	defer file.Close()

	if req.GetOffset() > info.Size {
		return status.Error(codes.InvalidArgument, "offset is beyond block size")
	}

	if _, err := file.Seek(int64(req.GetOffset()), io.SeekStart); err != nil {
		return status.Errorf(codes.Internal, "seek block: %v", err)
	}

	toRead := info.Size - req.GetOffset()
	if req.GetLength() > 0 && req.GetLength() < toRead {
		toRead = req.GetLength()
	}

	if err := stream.Send(&datanodev1.ReadBlockResponse{
		Payload: &datanodev1.ReadBlockResponse_Header{
			Header: &datanodev1.ReadBlockHeader{
				BlockId:        info.ID,
				Size:           info.Size,
				ChecksumSha256: info.ChecksumSHA256,
			},
		},
	}); err != nil {
		return err
	}

	buf := make([]byte, s.chunkLen)
	limited := io.LimitReader(file, int64(toRead))
	var sent uint64
	for {
		n, err := limited.Read(buf)
		if n > 0 {
			sent += uint64(n)
			chunk := append([]byte(nil), buf[:n]...)
			if sendErr := stream.Send(&datanodev1.ReadBlockResponse{
				Payload: &datanodev1.ReadBlockResponse_Chunk{Chunk: chunk},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "read block: %v", err)
		}
	}

	if sent != toRead {
		return status.Error(codes.DataLoss, "block truncated while reading")
	}
	return nil
}

func (s *GRPCServer) DeleteBlock(_ context.Context, req *datanodev1.DeleteBlockRequest) (*datanodev1.DeleteBlockResponse, error) {
	if req.GetBlockId() == "" {
		return nil, status.Error(codes.InvalidArgument, "block_id is required")
	}

	deleted, err := s.storage.Delete(req.GetBlockId())
	if err != nil {
		return nil, storageStatus(err)
	}

	return &datanodev1.DeleteBlockResponse{Deleted: deleted}, nil
}

func storageStatus(err error) error {
	switch {
	case errors.Is(err, ErrBlockNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrInvalidBlockID):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrChecksumMismatch):
		return status.Error(codes.DataLoss, err.Error())
	case errors.Is(err, ErrSizeMismatch):
		return status.Error(codes.DataLoss, err.Error())
	case errors.Is(err, ErrCorruptBlock):
		return status.Error(codes.DataLoss, err.Error())
	case errors.Is(err, ErrBlockExists):
		return status.Error(codes.AlreadyExists, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
