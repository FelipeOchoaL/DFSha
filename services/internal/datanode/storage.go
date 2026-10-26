package datanode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"dfsha/services/internal/common"
)

var (
	ErrBlockNotFound    = errors.New("block not found")
	ErrInvalidBlockID   = errors.New("invalid block id")
	ErrChecksumMismatch = errors.New("checksum mismatch")
	ErrSizeMismatch     = errors.New("size mismatch")
	ErrBlockExists      = errors.New("block already exists with different contents")
	ErrCorruptBlock     = errors.New("corrupt block metadata")
)

var validBlockID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type BlockInfo struct {
	ID             string `json:"id"`
	Size           uint64 `json:"size"`
	ChecksumSHA256 string `json:"checksum_sha256"`
}

type Storage struct {
	dir string
	mu  sync.RWMutex
}

func NewStorage(dir string) (*Storage, error) {
	if dir == "" {
		return nil, fmt.Errorf("data dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Storage{dir: dir}, nil
}

func (s *Storage) Save(id string, r io.Reader, expectedSize uint64, expectedChecksum string) (BlockInfo, error) {
	if err := validateBlockID(id); err != nil {
		return BlockInfo{}, err
	}
	if err := validateChecksum(expectedChecksum); err != nil {
		return BlockInfo{}, err
	}
	if expectedSize >= math.MaxInt64 {
		return BlockInfo{}, fmt.Errorf("%w: block is too large", ErrSizeMismatch)
	}

	blockTmp, err := os.CreateTemp(s.dir, id+"-*.blk.tmp")
	if err != nil {
		return BlockInfo{}, fmt.Errorf("create temp block: %w", err)
	}
	blockTmpPath := blockTmp.Name()
	cleanupBlock := true
	defer func() {
		if cleanupBlock {
			_ = os.Remove(blockTmpPath)
		}
	}()

	size, checksum, err := copyAndChecksum(blockTmp, io.LimitReader(r, int64(expectedSize)+1))
	if err == nil {
		err = blockTmp.Sync()
	}
	closeErr := blockTmp.Close()
	if err != nil {
		return BlockInfo{}, err
	}
	if closeErr != nil {
		return BlockInfo{}, fmt.Errorf("close temp block: %w", closeErr)
	}

	if uint64(size) != expectedSize {
		return BlockInfo{}, fmt.Errorf("%w: expected %d bytes, got %d", ErrSizeMismatch, expectedSize, size)
	}
	if checksum != expectedChecksum {
		return BlockInfo{}, fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expectedChecksum, checksum)
	}

	info := BlockInfo{
		ID:             id,
		Size:           uint64(size),
		ChecksumSHA256: checksum,
	}

	metaTmp, err := os.CreateTemp(s.dir, id+"-*.meta.tmp")
	if err != nil {
		return BlockInfo{}, fmt.Errorf("create temp metadata: %w", err)
	}
	metaTmpPath := metaTmp.Name()
	cleanupMeta := true
	defer func() {
		if cleanupMeta {
			_ = os.Remove(metaTmpPath)
		}
	}()

	if err := json.NewEncoder(metaTmp).Encode(info); err != nil {
		_ = metaTmp.Close()
		return BlockInfo{}, fmt.Errorf("write metadata: %w", err)
	}
	if err := metaTmp.Sync(); err != nil {
		_ = metaTmp.Close()
		return BlockInfo{}, fmt.Errorf("sync metadata: %w", err)
	}
	if err := metaTmp.Close(); err != nil {
		return BlockInfo{}, fmt.Errorf("close temp metadata: %w", err)
	}

	// Publish metadata last; readers never see an unfinished block. Existing
	// blocks are immutable, so a failed retry cannot destroy a previous write.
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.readMeta(id); err == nil {
		if existing != info {
			return BlockInfo{}, ErrBlockExists
		}
		file, err := os.Open(s.blockPath(id))
		if err != nil {
			return BlockInfo{}, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
		}
		defer file.Close()
		if err := verifyFile(file, existing); err != nil {
			return BlockInfo{}, err
		}
		return existing, nil
	} else if !errors.Is(err, ErrBlockNotFound) {
		return BlockInfo{}, err
	}
	if err := os.Rename(blockTmpPath, s.blockPath(id)); err != nil {
		return BlockInfo{}, fmt.Errorf("commit block: %w", err)
	}
	cleanupBlock = false

	if err := os.Rename(metaTmpPath, s.metaPath(id)); err != nil {
		_ = os.Remove(s.blockPath(id))
		return BlockInfo{}, fmt.Errorf("commit metadata: %w", err)
	}
	cleanupMeta = false

	return info, nil
}

func (s *Storage) Open(id string) (*os.File, BlockInfo, error) {
	if err := validateBlockID(id); err != nil {
		return nil, BlockInfo{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	info, err := s.readMeta(id)
	if err != nil {
		return nil, BlockInfo{}, err
	}

	file, err := os.Open(s.blockPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, BlockInfo{}, ErrBlockNotFound
	}
	if err != nil {
		return nil, BlockInfo{}, fmt.Errorf("open block: %w", err)
	}

	return file, info, nil
}

func (s *Storage) Verify(id string) (BlockInfo, error) {
	file, info, err := s.OpenVerified(id)
	if err != nil {
		return BlockInfo{}, err
	}
	defer file.Close()

	return info, nil
}

// OpenVerified verifies and rewinds the same handle that the caller will read.
func (s *Storage) OpenVerified(id string) (*os.File, BlockInfo, error) {
	file, info, err := s.Open(id)
	if err != nil {
		return nil, BlockInfo{}, err
	}
	if err := verifyFile(file, info); err != nil {
		_ = file.Close()
		return nil, BlockInfo{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, BlockInfo{}, err
	}
	return file, info, nil
}

func verifyFile(file *os.File, info BlockInfo) error {
	checksum, err := common.SHA256HexReader(file)
	if err != nil {
		return fmt.Errorf("verify block: %w", err)
	}
	if checksum != info.ChecksumSHA256 {
		return fmt.Errorf("%w: block %s", ErrChecksumMismatch, info.ID)
	}
	if stat, err := file.Stat(); err != nil {
		return fmt.Errorf("stat block: %w", err)
	} else if uint64(stat.Size()) != info.Size {
		return fmt.Errorf("%w: block %s", ErrSizeMismatch, info.ID)
	}
	return nil
}

func (s *Storage) Delete(id string) (bool, error) {
	if err := validateBlockID(id); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := false
	if err := os.Remove(s.blockPath(id)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("delete block: %w", err)
		}
	} else {
		deleted = true
	}

	if err := os.Remove(s.metaPath(id)); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return deleted, fmt.Errorf("delete metadata: %w", err)
		}
	} else {
		deleted = true
	}

	return deleted, nil
}

func (s *Storage) List() ([]BlockInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	matches, err := filepath.Glob(filepath.Join(s.dir, "*.meta"))
	if err != nil {
		return nil, fmt.Errorf("list metadata: %w", err)
	}

	blocks := make([]BlockInfo, 0, len(matches))
	for _, metaPath := range matches {
		file, err := os.Open(metaPath)
		if err != nil {
			return nil, fmt.Errorf("open metadata: %w", err)
		}

		var info BlockInfo
		decodeErr := json.NewDecoder(file).Decode(&info)
		closeErr := file.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode metadata %s: %w", metaPath, decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close metadata %s: %w", metaPath, closeErr)
		}

		if err := validateBlockID(info.ID); err != nil {
			return nil, fmt.Errorf("metadata %s: %w", metaPath, err)
		}
		if filepath.Base(metaPath) != info.ID+".meta" || validateChecksum(info.ChecksumSHA256) != nil || info.Size >= math.MaxInt64 {
			return nil, fmt.Errorf("%w: %s", ErrCorruptBlock, metaPath)
		}
		if _, err := os.Stat(s.blockPath(info.ID)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat block %s: %w", info.ID, err)
		}

		blocks = append(blocks, info)
	}

	return blocks, nil
}

// UsedBytes suma el tamaño de los bloques en disco (para el heartbeat).
func (s *Storage) UsedBytes() uint64 {
	blocks, err := s.List()
	if err != nil {
		return 0
	}
	var n uint64
	for _, b := range blocks {
		n += b.Size
	}
	return n
}

func (s *Storage) blockPath(id string) string {
	return filepath.Join(s.dir, id+".blk")
}

func (s *Storage) metaPath(id string) string {
	return filepath.Join(s.dir, id+".meta")
}

func (s *Storage) readMeta(id string) (BlockInfo, error) {
	file, err := os.Open(s.metaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return BlockInfo{}, ErrBlockNotFound
	}
	if err != nil {
		return BlockInfo{}, fmt.Errorf("open metadata: %w", err)
	}
	defer file.Close()

	var info BlockInfo
	if err := json.NewDecoder(file).Decode(&info); err != nil {
		return BlockInfo{}, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	if info.ID != id || validateChecksum(info.ChecksumSHA256) != nil || info.Size >= math.MaxInt64 {
		return BlockInfo{}, ErrCorruptBlock
	}
	return info, nil
}

func validateBlockID(id string) error {
	if !validBlockID.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidBlockID, id)
	}
	base := strings.ToUpper(strings.SplitN(id, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return fmt.Errorf("%w: reserved filename %q", ErrInvalidBlockID, id)
	}
	return nil
}

func validateChecksum(checksum string) error {
	decoded, err := hex.DecodeString(checksum)
	if err != nil || len(decoded) != sha256.Size || checksum != strings.ToLower(checksum) {
		return fmt.Errorf("%w: expected lowercase SHA-256 hexadecimal", ErrChecksumMismatch)
	}
	return nil
}

func copyAndChecksum(dst io.Writer, src io.Reader) (int64, string, error) {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hash), src)
	if err != nil {
		return 0, "", fmt.Errorf("write block: %w", err)
	}

	return written, hex.EncodeToString(hash.Sum(nil)), nil
}
