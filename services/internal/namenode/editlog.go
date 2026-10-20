// Edit log append-only + snapshot en disco para reconstruir los metadatos al reiniciar.
//
// Cada mutación de metadatos (mkdir, rmdir, delete, allocate, commit, finalize)
// se escribe como una línea JSON (EditEntry) con un txid creciente y se fuerza a
// disco (fsync) antes de responder al cliente. Periódicamente se guarda un
// snapshot del estado completo (snapshot.go) y el log se trunca. Al arrancar se
// carga el snapshot y se reproduce el log (persist.go).
//
// Las ubicaciones de las réplicas NO se registran: se reconstruyen con los
// block reports de los DataNodes.
package namenode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const editLogFileName = "edits.log"

// EditOp es el tipo de operación registrada.
type EditOp string

const (
	OpMkdir    EditOp = "mkdir"
	OpRmdir    EditOp = "rmdir"
	OpDelete   EditOp = "delete" // rm de un archivo, con sus bloques
	OpAllocate EditOp = "allocate"
	OpCommit   EditOp = "commit"
	OpFinalize EditOp = "finalize"
)

// EditEntry es una operación de metadatos. Es el registro que se escribe en el
// edit log y el que el NameNode activo enviará al standby (US-71). Se guardan
// todos los datos que hacen falta para reproducirla de forma determinista,
// incluida la hora, de modo que el estado recuperado sea idéntico.
type EditEntry struct {
	TxID int64     `json:"txid"`
	Op   EditOp    `json:"op"`
	Time time.Time `json:"time"`
	Path string    `json:"path"`

	Owner   string `json:"owner,omitempty"`   // mkdir, allocate (primer bloque)
	Parents bool   `json:"parents,omitempty"` // mkdir

	UploadID   string `json:"uploadId,omitempty"`   // allocate, commit, finalize
	NewUpload  bool   `json:"newUpload,omitempty"`  // allocate: crea el archivo y abre la subida
	BlockID    string `json:"blockId,omitempty"`    // allocate (vacío en archivos vacíos), commit
	BlockIndex int    `json:"blockIndex,omitempty"` // allocate
	Size       int64  `json:"size,omitempty"`       // allocate, commit, finalize
	Checksum   string `json:"checksum,omitempty"`   // commit
}

// EditLog es el archivo de entradas de edición. Es seguro para uso concurrente.
type EditLog struct {
	mu              sync.Mutex
	dir             string
	f               *os.File
	next            int64 // txid de la próxima entrada
	sinceCheckpoint int   // entradas escritas desde el último snapshot
	failed          error // primer error de escritura; el log queda inutilizable

	// OnAppend, si no es nil, se llama con cada entrada ya guardada en disco.
	// Es el punto de enganche para replicarla al NameNode standby (US-71): si
	// devuelve error, Append lo devuelve y el cliente no recibe confirmación.
	OnAppend func(EditEntry) error
}

// OpenEditLog abre (o crea) el edit log de dir y devuelve las entradas que ya
// contiene, en orden. Si la última línea quedó a medias por una caída durante
// la escritura, se descarta y el archivo se repara. Cualquier otra corrupción
// (línea inválida en medio o txid no consecutivos) es un error.
func OpenEditLog(dir string) (*EditLog, []EditEntry, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("crear directorio de metadatos %s: %w", dir, err)
	}
	path := filepath.Join(dir, editLogFileName)
	// Sin O_APPEND: en Windows no permite Truncate. Se escribe siempre al final.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("abrir %s: %w", path, err)
	}
	entries, valid, err := readEditEntries(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("leer %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if info.Size() > valid {
		log.Printf("edit log: descartando %d bytes incompletos al final de %s", info.Size()-valid, path)
		if err := f.Truncate(valid); err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("reparar %s: %w", path, err)
		}
		_ = f.Sync()
	}
	if _, err := f.Seek(valid, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	l := &EditLog{dir: dir, f: f, next: 1}
	if n := len(entries); n > 0 {
		l.next = entries[n-1].TxID + 1
	}
	return l, entries, nil
}

// readEditEntries lee todas las entradas válidas. Devuelve también el número de
// bytes que ocupan (el resto del archivo es una escritura incompleta).
func readEditEntries(f *os.File) ([]EditEntry, int64, error) {
	r := bufio.NewReader(f)
	var entries []EditEntry
	var valid int64
	for lineNo := 1; ; lineNo++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				// Vacío = fin limpio; con datos = última línea sin '\n' (escritura incompleta).
				return entries, valid, nil
			}
			return nil, 0, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			valid += int64(len(line))
			continue
		}
		var e EditEntry
		if err := json.Unmarshal(trimmed, &e); err != nil || e.TxID <= 0 || e.Op == "" {
			if _, perr := r.Peek(1); perr == io.EOF {
				return entries, valid, nil // última línea corrupta: escritura incompleta
			}
			if err == nil {
				err = fmt.Errorf("entrada sin txid u operación")
			}
			return nil, 0, fmt.Errorf("línea %d corrupta: %w", lineNo, err)
		}
		if n := len(entries); n > 0 && e.TxID != entries[n-1].TxID+1 {
			return nil, 0, fmt.Errorf("línea %d: txid %d no sigue a %d", lineNo, e.TxID, entries[n-1].TxID)
		}
		entries = append(entries, e)
		valid += int64(len(line))
	}
}

// Append asigna el siguiente txid a e, la escribe al final del log y la fuerza a
// disco. Si la escritura falla, el log queda inutilizable (todos los Append
// siguientes fallan): el NameNode no debe seguir aceptando cambios que no puede
// registrar.
func (l *EditLog) Append(e *EditEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.failed != nil {
		return fmt.Errorf("el edit log no se puede usar: %w", l.failed)
	}
	e.TxID = l.next
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := l.f.Write(data); err != nil {
		l.failed = err
		return fmt.Errorf("escribir en el edit log: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		l.failed = err
		return fmt.Errorf("sincronizar el edit log: %w", err)
	}
	l.next++
	l.sinceCheckpoint++
	if l.OnAppend != nil {
		if err := l.OnAppend(*e); err != nil {
			return fmt.Errorf("replicar la entrada %d: %w", e.TxID, err)
		}
	}
	return nil
}

// LastTxID devuelve el txid de la última entrada escrita (0 si no hay ninguna).
func (l *EditLog) LastTxID() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// SinceCheckpoint devuelve cuántas entradas hay desde el último snapshot.
func (l *EditLog) SinceCheckpoint() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sinceCheckpoint
}

// resume deja el log listo para continuar tras la recuperación: la última
// entrada aplicada tiene el txid last y hay since entradas posteriores al snapshot.
func (l *EditLog) resume(last int64, since int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next = last + 1
	l.sinceCheckpoint = since
}

// Reset vacía el archivo después de guardar un snapshot. El contador de txid
// continúa: el siguiente txid no se reinicia.
func (l *EditLog) Reset() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.failed != nil {
		return fmt.Errorf("el edit log no se puede usar: %w", l.failed)
	}
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("truncar el edit log: %w", err)
	}
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		l.failed = err
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.sinceCheckpoint = 0
	return nil
}

// Close cierra el archivo.
func (l *EditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
