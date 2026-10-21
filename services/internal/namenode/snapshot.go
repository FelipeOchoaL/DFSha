// Snapshot del estado de los metadatos: namespace, mapa de bloques (sin
// ubicaciones de réplicas) y subidas en curso. Se escribe de forma atómica
// (archivo temporal + rename) para que una caída nunca deje un snapshot a medias.
package namenode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	snapshotFileName = "snapshot.json"
	snapshotVersion  = 2 // 2 añade grupo y permisos; se siguen leyendo los de version 1
)

type snapCommitted struct {
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
}

type snapPending struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	Size  int64  `json:"size"`
}

type snapUpload struct {
	ID        string          `json:"id"`
	Owner     string          `json:"owner"`
	Committed []snapCommitted `json:"committed,omitempty"`
	Pending   *snapPending    `json:"pending,omitempty"`
	Attempts  map[int]int     `json:"attempts,omitempty"`
}

// snapshotFile es el contenido de snapshot.json.
type snapshotFile struct {
	Version  int                    `json:"version"`
	LastTxID int64                  `json:"lastTxid"`
	Time     time.Time              `json:"time"`
	Root     *snapInode             `json:"root"`
	Files    map[string][]string    `json:"files"`
	Blocks   map[string]snapBlock   `json:"blocks"`
	Uploads  map[string]*snapUpload `json:"uploads"`
}

// writeSnapshot guarda el snapshot en dir de forma atómica y durable.
func writeSnapshot(dir string, snap *snapshotFile) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, snapshotFileName)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	// Mejor esfuerzo: sincronizar el directorio para que el rename sea durable.
	// En Windows abrir un directorio para sincronizarlo no está soportado.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readSnapshot lee el snapshot de dir. Devuelve nil, nil si no existe.
func readSnapshot(dir string) (*snapshotFile, error) {
	path := filepath.Join(dir, snapshotFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("snapshot %s corrupto: %w", path, err)
	}
	if snap.Version != 1 && snap.Version != snapshotVersion {
		return nil, fmt.Errorf("snapshot %s: versión %d no soportada", path, snap.Version)
	}
	return &snap, nil
}
