package datanode

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dfsha/services/internal/common"
)

func TestStorageSaveOpenListDelete(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewStorage() error = %v", err)
	}

	data := []byte("block contents")
	checksum := common.SHA256Hex(data)
	info, err := storage.Save("block-1", strings.NewReader(string(data)), uint64(len(data)), checksum)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if info.Size != uint64(len(data)) || info.ChecksumSHA256 != checksum {
		t.Fatalf("Save() info = %+v", info)
	}

	file, meta, err := storage.Open("block-1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	got, readErr := io.ReadAll(file)
	_ = file.Close()
	if readErr != nil || string(got) != string(data) {
		t.Fatalf("Open() contents = %q, error = %v", got, readErr)
	}
	if meta.ID != "block-1" {
		t.Fatalf("Open() metadata ID = %q", meta.ID)
	}

	blocks, err := storage.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(blocks) != 1 || blocks[0].ID != "block-1" {
		t.Fatalf("List() = %+v", blocks)
	}

	deleted, err := storage.Delete("block-1")
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !deleted {
		t.Fatal("Delete() deleted = false, want true")
	}
	if _, _, err := storage.Open("block-1"); !errors.Is(err, ErrBlockNotFound) {
		t.Fatalf("Open() after delete error = %v, want ErrBlockNotFound", err)
	}
}

func TestStorageRetryDoesNotOverwriteBlock(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	save := func(body string) error {
		_, err := storage.Save("immutable", strings.NewReader(body), uint64(len(body)), common.SHA256Hex([]byte(body)))
		return err
	}
	if err := save("original"); err != nil {
		t.Fatal(err)
	}
	if err := save("original"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if err := save("replacement"); !errors.Is(err, ErrBlockExists) {
		t.Fatalf("overwrite: %v", err)
	}
	reopened, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	file, info, err := reopened.OpenVerified("immutable")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "original" || info.ChecksumSHA256 != common.SHA256Hex(data) {
		t.Fatalf("persisted contents differ: %q %v", data, err)
	}
}

func TestStorageRejectsUnsafeIDs(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../outside", "a/b", `a\b`, "CON", "nul.txt", "COM1", strings.Repeat("x", 129)} {
		t.Run(id, func(t *testing.T) {
			if _, err := storage.Save(id, strings.NewReader("abc"), 3, common.SHA256Hex([]byte("abc"))); !errors.Is(err, ErrInvalidBlockID) {
				t.Fatalf("Save(%q): %v", id, err)
			}
			if _, _, err := storage.Open(id); !errors.Is(err, ErrInvalidBlockID) {
				t.Fatalf("Open(%q): %v", id, err)
			}
			if _, err := storage.Delete(id); !errors.Is(err, ErrInvalidBlockID) {
				t.Fatalf("Delete(%q): %v", id, err)
			}
		})
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestStorageInterruptedWriteLeavesNoFiles(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Save("interrupted", brokenReader{}, 3, common.SHA256Hex([]byte("abc"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("interrupted Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial files leaked: %v %v", entries, err)
	}
}

func TestStorageRejectsChecksumAndSizeMismatch(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewStorage() error = %v", err)
	}

	if _, err := storage.Save("bad-checksum", strings.NewReader("abc"), 3, "wrong"); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Save() error = %v, want ErrChecksumMismatch", err)
	}

	checksum := common.SHA256Hex([]byte("abc"))
	if _, err := storage.Save("bad-size", strings.NewReader("abc"), 4, checksum); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("Save() error = %v, want ErrSizeMismatch", err)
	}

	blocks, err := storage.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("List() = %+v, want no saved blocks", blocks)
	}
}

func TestStorageListIgnoresTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("NewStorage() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "half-written.blk.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	blocks, err := storage.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("List() = %+v, want no blocks", blocks)
	}
}

func TestStorageConcurrentDifferentBlocks(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewStorage() error = %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []string{"block-a", "block-b", "block-c"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			body := []byte(id + "-data")
			_, saveErr := storage.Save(id, strings.NewReader(string(body)), uint64(len(body)), common.SHA256Hex(body))
			if saveErr != nil {
				t.Errorf("Save(%q) error = %v", id, saveErr)
			}
		}(id)
	}
	wg.Wait()

	blocks, err := storage.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("List() returned %d blocks, want 3: %+v", len(blocks), blocks)
	}
}
