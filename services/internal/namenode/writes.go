// Escritura de archivos (write-once): allocate, commit y finalize.
//
// Flujo por archivo (ver documentation/diagrams/01-put-escritura.puml):
//
// para cada bloque i: allocate -> WriteBlock (gRPC, no pasa por aquí) -> commit
// finalize
//
// El estado de cada subida en curso vive en uploadTable. Cada operación se
// aplica con los apply* de persist.go y se registra en el edit log (US-70), así
// que una subida a medias sobrevive a un reinicio del NameNode.
package namenode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PipelineNode es un DataNode del pipeline de un bloque, tal como lo ve el
// cliente (host y port son los que el DataNode anunció con ADVERTISE_ADDR).
type PipelineNode struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port uint32 `json:"port"`
	Rack string `json:"rack"`
}

// BlockPlacer decide en qué DataNodes va cada bloque. Lo implementa la
// colocación del NameNode (placement.go, US-24 y US-50); aquí solo se define lo
// que allocate necesita, para que ambas partes se integren sin acoplarse.
type BlockPlacer interface {
	// Place devuelve exactamente replicas DataNodes vivos y distintos, en el
	// orden del pipeline (el cliente escribe en el primero). path y blockIndex
	// permiten repartir los bloques consecutivos de un archivo entre nodos
	// distintos. Si no hay suficientes DataNodes vivos devuelve un error.
	Place(path string, blockIndex, replicas int) ([]PipelineNode, error)
}

// committedBlock es un bloque ya confirmado por el cliente.
type committedBlock struct {
	id       string
	size     int64
	checksum string
}

// pendingBlock es el bloque asignado (allocate) que aún espera su commit.
type pendingBlock struct {
	id    string
	index int
	size  int64
}

// upload es el estado de una subida en curso de un archivo.
type upload struct {
	id            string
	owner         string
	committed     []committedBlock // bloques 0..n-1 confirmados, en orden
	committedSize int64            // suma de los tamaños confirmados (= próximo offset)
	pending       *pendingBlock    // bloque n asignado y sin confirmar, si hay
	attempts      map[int]int      // blockIndex -> intentos de allocate (para ids únicos)
}

// uploadTable guarda las subidas en curso por ruta. Su mutex serializa TODAS las
// mutaciones de metadatos (no solo las subidas): ver persist.go. Orden de
// locks: uploadTable.mu -> Namespace.mu -> BlockMap.mu.
type uploadTable struct {
	mu     sync.Mutex
	byPath map[string]*upload
}

func newUploadTable() uploadTable {
	return uploadTable{byPath: make(map[string]*upload)}
}

// newUploadID genera un identificador aleatorio de subida (UUID v4).
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// nextBlockID arma el id del próximo bloque de un índice: blk_<8 primeros del
// uploadId>_<índice>. Los reintentos del mismo índice (attempt > 0) añaden _r<n>
// para que un bloque a medias del intento anterior nunca se confunda con el nuevo.
func nextBlockID(uploadID string, index, attempt int) string {
	short := uploadID
	if len(short) > 8 {
		short = short[:8]
	}
	if attempt == 0 {
		return fmt.Sprintf("blk_%s_%d", short, index)
	}
	return fmt.Sprintf("blk_%s_%d_r%d", short, index, attempt)
}

// lookupUpload devuelve la subida de path validando uploadId y dueño. Si falla
// ya respondió al cliente y devuelve false. Debe llamarse con up.mu tomado.
func (s *RESTServer) lookupUpload(w http.ResponseWriter, path, uploadID, user string) (*upload, bool) {
	u, ok := s.up.byPath[path]
	if !ok {
		// Sin subida registrada: o el archivo no existe o ya está completo.
		e, err := s.ns.Stat(path)
		switch {
		case err != nil:
			writeNamespaceError(w, err)
		case e.Type == TypeDir:
			writeError(w, http.StatusBadRequest, codeIsADirectory, "es un directorio: "+path)
		case e.State == StateComplete:
			writeError(w, http.StatusConflict, codeAlreadyExists, "el archivo ya está completo: "+path)
		default:
			// UNDER_CONSTRUCTION sin subida: no debería ocurrir salvo con metadatos anteriores a la persistencia.
			writeError(w, http.StatusConflict, codeFileBeingWritten,
				"el archivo está en construcción y no tiene una subida activa; elimínelo con rm y vuelva a intentar: "+path)
		}
		return nil, false
	}
	if u.id != uploadID {
		writeError(w, http.StatusConflict, codeFileBeingWritten,
			"otra subida está escribiendo este archivo: "+path)
		return nil, false
	}
	if u.owner != user {
		writeError(w, http.StatusForbidden, codeForbidden, "la subida pertenece a otro usuario: "+path)
		return nil, false
	}
	return u, true
}

// --- POST /files/allocate ---

type allocateRequest struct {
	Path       string `json:"path"`
	BlockIndex int    `json:"blockIndex"`
	Size       int64  `json:"size"`
	UploadID   string `json:"uploadId"`
}

type allocateResponse struct {
	UploadID   string         `json:"uploadId"`
	BlockID    string         `json:"blockId"`
	BlockIndex int            `json:"blockIndex"`
	Offset     int64          `json:"offset"`
	Size       int64          `json:"size"`
	Pipeline   []PipelineNode `json:"pipeline"`
}

// handleAllocate asigna un bloque. El primero (blockIndex 0, sin uploadId) crea
// el archivo en UNDER_CONSTRUCTION y abre la subida. Repetir allocate para el
// bloque pendiente lo descarta y asigna uno nuevo (reintento del cliente).
func (s *RESTServer) handleAllocate(w http.ResponseWriter, r *http.Request, user string) {
	var req allocateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	switch {
	case req.Path == "":
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "falta el campo obligatorio: path")
		return
	case req.BlockIndex < 0:
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "blockIndex no puede ser negativo")
		return
	case req.Size < 0 || (req.Size == 0 && (req.BlockIndex != 0 || req.UploadID != "")):
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"size debe ser mayor que 0 (size 0 solo se admite en el primer allocate de un archivo vacío)")
		return
	case req.UploadID == "" && req.BlockIndex != 0:
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"uploadId es obligatorio salvo en el primer bloque (blockIndex 0)")
		return
	}
	norm, err := NormalizePath(req.Path)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}

	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	// Validar sin modificar nada: el estado solo cambia al final, cuando ya se
	// sabe que la operación es válida y se puede registrar.
	var (
		u       *upload // subida existente (nil si es la primera petición)
		isNew   = req.UploadID == ""
		uploadI string
	)
	if isNew {
		if err := s.ns.CanCreateFile(norm); err != nil {
			s.writeStartUploadError(w, norm, err)
			return
		}
		if uploadI, err = newUploadID(); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "error interno del servidor")
			return
		}
	} else {
		var ok bool
		if u, ok = s.lookupUpload(w, norm, req.UploadID, user); !ok {
			return
		}
		// Solo se puede pedir el siguiente bloque o repetir el pendiente.
		if req.BlockIndex != len(u.committed) {
			writeError(w, http.StatusConflict, codeBlockOutOfOrder, fmt.Sprintf(
				"se esperaba el bloque %d y se pidió el %d", len(u.committed), req.BlockIndex))
			return
		}
		uploadI = u.id
	}

	entry := EditEntry{
		Op: OpAllocate, Time: s.stamp(), Path: norm, Owner: user,
		UploadID: uploadI, NewUpload: isNew, BlockIndex: req.BlockIndex, Size: req.Size,
	}
	resp := allocateResponse{UploadID: uploadI, BlockIndex: req.BlockIndex, Size: req.Size, Pipeline: []PipelineNode{}}
	if u != nil {
		resp.Offset = u.committedSize
	}

	// Archivo vacío (size 0): solo se abre la subida; no hay bloque que asignar.
	// El cliente llama a finalize a continuación.
	if req.Size > 0 {
		pipeline, err := s.placer.Place(norm, req.BlockIndex, s.ReplicationFactor)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, codeInsufficientDN,
				"no hay suficientes DataNodes vivos: "+err.Error())
			return
		}
		attempt := 0
		if u != nil {
			attempt = u.attempts[req.BlockIndex]
		}
		entry.BlockID = nextBlockID(uploadI, req.BlockIndex, attempt)
		resp.BlockID = entry.BlockID
		resp.Pipeline = pipeline
	}

	if err := s.applyAllocate(entry); err != nil {
		writeNamespaceError(w, err)
		return
	}
	if !s.record(w, &entry) {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeStartUploadError traduce el fallo al validar la creación del archivo.
// Que el path ya exista es 409 FILE_BEING_WRITTEN si es un archivo en
// construcción (otro put en curso) y 409 ALREADY_EXISTS en cualquier otro caso.
func (s *RESTServer) writeStartUploadError(w http.ResponseWriter, path string, err error) {
	if errors.Is(err, ErrAlreadyExists) {
		if e, serr := s.ns.Stat(path); serr == nil && e.Type == TypeFile && e.State == StateUnderConstruction {
			writeError(w, http.StatusConflict, codeFileBeingWritten,
				"otra subida está escribiendo este archivo: "+path)
			return
		}
	}
	writeNamespaceError(w, err)
}

// --- POST /files/commit ---

type commitRequest struct {
	Path           string `json:"path"`
	UploadID       string `json:"uploadId"`
	BlockID        string `json:"blockId"`
	Size           int64  `json:"size"`
	ChecksumSha256 string `json:"checksumSha256"`
}

type commitResponse struct {
	BlockID  string `json:"blockId"`
	Replicas int    `json:"replicas"`
}

// handleCommit confirma un bloque con su tamaño y checksum.
//
// Regla de consistencia estricta (RNF3): solo se acepta cuando los DataNodes ya
// confirmaron (BlockReceived) ReplicationFactor réplicas con el mismo checksum
// que envía el cliente. Como el aviso de los DataNodes puede llegar un instante
// después del ACK del pipeline, se espera hasta CommitWait antes de responder
// 409 BLOCK_NOT_REPLICATED. La espera se hace SIN el lock de subidas para no
// bloquear otras escrituras; por eso el estado se valida antes y después.
func (s *RESTServer) handleCommit(w http.ResponseWriter, r *http.Request, user string) {
	var req commitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	switch {
	case req.Path == "" || req.UploadID == "" || req.BlockID == "":
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "faltan campos obligatorios: path, uploadId, blockId")
		return
	case req.Size <= 0:
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "size debe ser mayor que 0")
		return
	case !isSHA256Hex(req.ChecksumSha256):
		writeError(w, http.StatusBadRequest, codeInvalidRequest,
			"checksumSha256 debe ser un SHA-256 en hexadecimal minúscula (64 caracteres)")
		return
	}
	norm, err := NormalizePath(req.Path)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}

	// 1) Validar la petición antes de esperar.
	s.up.mu.Lock()
	_, applied, ok := s.checkCommit(w, req, norm, user)
	s.up.mu.Unlock()
	if !ok || applied {
		return
	}

	// 2) Esperar las réplicas, sin el lock de subidas.
	if n, ok := s.waitForReplicas(r.Context(), req.BlockID, req.ChecksumSha256, s.ReplicationFactor); !ok {
		writeError(w, http.StatusConflict, codeBlockNotReplicated, fmt.Sprintf(
			"el bloque %s tiene %d de %d réplicas confirmadas con el mismo checksum",
			req.BlockID, n, s.ReplicationFactor))
		return
	}

	// 3) Aplicar y registrar el commit. Se revalida porque el estado pudo cambiar
	// mientras se esperaba (otro commit idéntico, o un allocate que reintentó el bloque).
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	_, applied, ok = s.checkCommit(w, req, norm, user)
	if !ok || applied {
		return
	}
	entry := EditEntry{
		Op: OpCommit, Time: s.stamp(), Path: norm, UploadID: req.UploadID,
		BlockID: req.BlockID, Size: req.Size, Checksum: req.ChecksumSha256,
	}
	if err := s.applyCommit(entry); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "error interno del servidor")
		return
	}
	if !s.record(w, &entry) {
		return
	}
	writeJSON(w, http.StatusOK, commitResponse{
		BlockID:  req.BlockID,
		Replicas: s.blocks.CountReplicas(req.BlockID, req.ChecksumSha256),
	})
}

// checkCommit valida un commit contra el estado de la subida. Debe llamarse con
// up.mu tomado. Si la petición no es válida ya respondió al cliente y devuelve
// ok=false. Si el bloque ya estaba confirmado con los mismos datos (el cliente
// repite un commit cuya respuesta se perdió) responde 200 y devuelve
// applied=true.
func (s *RESTServer) checkCommit(w http.ResponseWriter, req commitRequest, path, user string) (u *upload, applied, ok bool) {
	u, ok = s.lookupUpload(w, path, req.UploadID, user)
	if !ok {
		return nil, false, false
	}
	if u.pending == nil || u.pending.id != req.BlockID {
		for _, c := range u.committed {
			if c.id == req.BlockID && c.size == req.Size && c.checksum == req.ChecksumSha256 {
				writeJSON(w, http.StatusOK, commitResponse{
					BlockID:  c.id,
					Replicas: s.blocks.CountReplicas(c.id, c.checksum),
				})
				return u, true, true
			}
		}
		writeError(w, http.StatusNotFound, codeNotFound, "bloque desconocido o no asignado: "+req.BlockID)
		return nil, false, false
	}
	if req.Size != u.pending.size {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf(
			"size (%d) no coincide con el tamaño asignado (%d)", req.Size, u.pending.size))
		return nil, false, false
	}
	return u, false, true
}

// waitForReplicas espera hasta CommitWait a que el bloque tenga al menos
// required réplicas con el checksum dado. Devuelve cuántas hay al terminar y si
// se alcanzó el mínimo. Termina antes si el cliente cancela la petición.
func (s *RESTServer) waitForReplicas(ctx context.Context, blockID, checksum string, required int) (int, bool) {
	poll := s.CommitPollInterval
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	deadline := time.Now().Add(s.CommitWait)
	for {
		n := s.blocks.CountReplicas(blockID, checksum)
		if n >= required {
			return n, true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return n, false
		}
		wait := poll
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return n, false
		case <-time.After(wait):
		}
	}
}

// --- POST /files/finalize ---

type finalizeRequest struct {
	Path     string `json:"path"`
	UploadID string `json:"uploadId"`
}

type finalizeResponse struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Blocks int    `json:"blocks"`
	State  string `json:"state"`
}

// handleFinalize cierra el archivo: pasa a COMPLETE con su tamaño total. Un
// archivo sin bloques (vacío) es válido.
func (s *RESTServer) handleFinalize(w http.ResponseWriter, r *http.Request, user string) {
	var req finalizeRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Path == "" || req.UploadID == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "faltan campos obligatorios: path, uploadId")
		return
	}
	norm, err := NormalizePath(req.Path)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}

	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	u, ok := s.lookupUpload(w, norm, req.UploadID, user)
	if !ok {
		return
	}
	if u.pending != nil {
		writeError(w, http.StatusConflict, codeIncompleteFile, fmt.Sprintf(
			"el bloque %d fue asignado pero no confirmado", u.pending.index))
		return
	}
	// Defensa: todos los bloques del archivo deben seguir confirmados en el mapa.
	for _, c := range u.committed {
		if info, ok := s.blocks.Block(c.id); !ok || !info.Committed() {
			writeError(w, http.StatusConflict, codeIncompleteFile, "el bloque "+c.id+" no está confirmado")
			return
		}
	}

	size, blocks := u.committedSize, len(u.committed)
	entry := EditEntry{Op: OpFinalize, Time: s.stamp(), Path: norm, UploadID: req.UploadID, Size: size}
	if err := s.applyFinalize(entry); err != nil {
		writeNamespaceError(w, err)
		return
	}
	if !s.record(w, &entry) {
		return
	}
	writeJSON(w, http.StatusCreated, finalizeResponse{
		Path:   norm,
		Size:   size,
		Blocks: blocks,
		State:  string(StateComplete),
	})
}

// isSHA256Hex indica si s es un SHA-256 en hexadecimal minúscula.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
