// API REST para clientes: /auth/login, /fs/ls, /fs/mkdir, /fs/rm,
// /files/allocate, /files/commit, /files/finalize, /files/locations.
// El contrato exacto está en documentation/api-rest.md.
//
// Estado actual: /fs/ls, /fs/mkdir, /fs/rmdir (US-25) y /files/allocate,
// /files/commit, /files/finalize (US-26, ver writes.go) y /files/locations
// (US-27, ver reads.go).
package namenode

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// maxBodyBytes limita el tamaño del body JSON que aceptamos en las peticiones.
const maxBodyBytes = 1 << 20 // 1 MiB

// Códigos de error estables de la API (ver sección 1.2 de api-rest.md).
const (
	codeInvalidRequest     = "INVALID_REQUEST"
	codeInvalidPath        = "INVALID_PATH"
	codeIsADirectory       = "IS_A_DIRECTORY"
	codeUnauthorized       = "UNAUTHORIZED"
	codeNotFound           = "NOT_FOUND"
	codeParentNotFound     = "PARENT_NOT_FOUND"
	codeAlreadyExists      = "ALREADY_EXISTS"
	codeNotADirectory      = "NOT_A_DIRECTORY"
	codeDirNotEmpty        = "DIRECTORY_NOT_EMPTY"
	codeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	codeInternal           = "INTERNAL_ERROR"
	codeForbidden          = "FORBIDDEN"
	codeFileBeingWritten   = "FILE_BEING_WRITTEN"
	codeBlockOutOfOrder    = "BLOCK_OUT_OF_ORDER"
	codeIncompleteFile     = "INCOMPLETE_FILE"
	codeInsufficientDN     = "INSUFFICIENT_DATANODES"
	codeBlockNotReplicated = "BLOCK_NOT_REPLICATED"
)

// anonymousUser es el usuario que se asume mientras no exista autenticación.
const anonymousUser = "anonymous"

// RESTServer expone el namespace por HTTP/JSON.
type RESTServer struct {
	ns            *Namespace
	blocks        *BlockMap
	placer        BlockPlacer
	up            uploadTable // subidas en curso (ver writes.go)
	log           *EditLog    // edit log persistente; nil si la persistencia no esta activada (persist.go)
	snapshotEvery int         // cada cuantas operaciones se guarda un snapshot

	// ReplicationFactor es el número de DataNodes que allocate devuelve en el
	// pipeline de cada bloque (REPLICATION_FACTOR). Por defecto 3.
	ReplicationFactor int

	// CommitWait es el tiempo máximo que commit espera a que los DataNodes confirmen
	// (BlockReceived) las réplicas de un bloque antes de responder 409
	// BLOCK_NOT_REPLICATED. Absorbe la carrera entre el ACK del pipeline al cliente
	// y el aviso de los DataNodes al NameNode. Por defecto 3 s.
	CommitWait time.Duration

	// CommitPollInterval es cada cuánto commit revisa si ya llegaron las réplicas
	// mientras espera. Por defecto 50 ms.
	CommitPollInterval time.Duration

	// Nodes informa qué DataNodes están vivos y cuál es su dirección. Lo usa
	// /files/locations; debe asignarse antes de servir peticiones (lo implementa el
	// registro de DataNodes, US-22).
	Nodes NodeDirectory

	// Deletes recibe las órdenes de borrado de bloques que genera /fs/rm (lo
	// implementa la cola de comandos del registro de DataNodes, US-22). Si es nil,
	// rm no encola nada y los bloques los limpia el block report (US-58).
	Deletes BlockDeleter

	// Authenticate identifica al usuario de una petición. Si devuelve error la
	// petición se rechaza con 401 UNAUTHORIZED.
	//
	// ATENCIÓN: por defecto NO valida nada y todas las peticiones son del usuario
	// "anonymous". Hasta que exista el login con JWT (US-80, David) esta API no
	// tiene autenticación ni control de acceso: no debe exponerse fuera de un
	// entorno de desarrollo. US-80 reemplaza esta función por la que valida el token.
	Authenticate func(r *http.Request) (user string, err error)
}

// NewRESTServer crea el servidor REST. blocks es el mapa de bloques y placer
// decide en qué DataNodes va cada bloque (lo implementa placement.go, US-24).
func NewRESTServer(ns *Namespace, blocks *BlockMap, placer BlockPlacer) *RESTServer {
	return &RESTServer{
		ns:                 ns,
		blocks:             blocks,
		placer:             placer,
		up:                 newUploadTable(),
		ReplicationFactor:  3,
		CommitWait:         3 * time.Second,
		CommitPollInterval: 50 * time.Millisecond,
		Authenticate:       func(*http.Request) (string, error) { return anonymousUser, nil },
	}
}

// Handler devuelve el http.Handler con todas las rutas registradas.
func (s *RESTServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/fs/ls", s.route(http.MethodGet, s.handleLs))
	mux.HandleFunc("/fs/mkdir", s.route(http.MethodPost, s.handleMkdir))
	mux.HandleFunc("/fs/rmdir", s.route(http.MethodDelete, s.handleRmdir))
	mux.HandleFunc("/files/allocate", s.route(http.MethodPost, s.handleAllocate))
	mux.HandleFunc("/files/commit", s.route(http.MethodPost, s.handleCommit))
	mux.HandleFunc("/files/finalize", s.route(http.MethodPost, s.handleFinalize))
	mux.HandleFunc("/files/locations", s.route(http.MethodGet, s.handleLocations))
	mux.HandleFunc("/fs/rm", s.route(http.MethodDelete, s.handleRm))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "ruta desconocida: "+r.URL.Path)
	})
	return mux
}

// handlerFunc es un handler que ya recibe al usuario autenticado.
type handlerFunc func(w http.ResponseWriter, r *http.Request, user string)

// route valida el método HTTP, autentica la petición y llama al handler. Los
// errores salen en el formato común {"error", "code"}.
func (s *RESTServer) route(method string, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"método no permitido: use "+method)
			return
		}
		user, err := s.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, err.Error())
			return
		}
		h(w, r, user)
	}
}

// --- /fs/ls ---

type entryJSON struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Owner      string `json:"owner"`
	ModifiedAt string `json:"modifiedAt"`
	State      string `json:"state,omitempty"` // solo archivos
}

type lsResponse struct {
	Path    string      `json:"path"`
	Entries []entryJSON `json:"entries"`
}

// handleLs: GET /fs/ls?path=<ruta>. Sin path lista "/".
func (s *RESTServer) handleLs(w http.ResponseWriter, r *http.Request, _ string) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "/"
	}
	norm, err := NormalizePath(p)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	entries, err := s.ns.List(norm)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	resp := lsResponse{Path: norm, Entries: make([]entryJSON, 0, len(entries))}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, entryJSON{
			Name:       e.Name,
			Type:       string(e.Type),
			Size:       e.Size,
			Owner:      e.Owner,
			ModifiedAt: e.ModTime.UTC().Format(time.RFC3339),
			State:      string(e.State),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- /fs/mkdir ---

type mkdirRequest struct {
	Path    string `json:"path"`
	Parents bool   `json:"parents"`
}

type pathResponse struct {
	Path string `json:"path"`
}

// handleMkdir: POST /fs/mkdir {"path": "...", "parents": false}.
// 201 si se creó; 200 si parents=true y el directorio ya existía.
func (s *RESTServer) handleMkdir(w http.ResponseWriter, r *http.Request, user string) {
	var req mkdirRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "falta el campo obligatorio: path")
		return
	}
	norm, err := NormalizePath(req.Path)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}

	// Las mutaciones se serializan con up.mu: ver persist.go.
	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	existed := false
	if req.Parents {
		if e, err := s.ns.Stat(norm); err == nil && e.Type == TypeDir {
			existed = true
		}
	}
	entry := EditEntry{Op: OpMkdir, Time: s.stamp(), Path: norm, Owner: user, Parents: req.Parents}
	if err := s.applyMkdir(entry); err != nil {
		writeNamespaceError(w, err)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK // mkdir -p de un directorio existente: no cambia nada, no se registra
	} else if !s.record(w, &entry) {
		return
	}
	writeJSON(w, status, pathResponse{Path: norm})
}

// --- /fs/rmdir ---

// handleRmdir: DELETE /fs/rmdir?path=<ruta>. Solo directorios vacíos.
func (s *RESTServer) handleRmdir(w http.ResponseWriter, r *http.Request, _ string) {
	p := r.URL.Query().Get("path")
	if p == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "falta el parámetro obligatorio: path")
		return
	}
	norm, err := NormalizePath(p)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}

	s.up.mu.Lock()
	defer s.up.mu.Unlock()

	entry := EditEntry{Op: OpRmdir, Time: s.stamp(), Path: norm}
	if err := s.applyRmdir(entry); err != nil {
		writeNamespaceError(w, err)
		return
	}
	if !s.record(w, &entry) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- utilidades de respuesta ---

type errorJSON struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorJSON{Error: msg, Code: code})
}

// writeNamespaceError traduce los errores del namespace a HTTP.
func writeNamespaceError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, codeInternal
	switch {
	case errors.Is(err, ErrInvalidPath):
		status, code = http.StatusBadRequest, codeInvalidPath
	case errors.Is(err, ErrIsDirectory):
		status, code = http.StatusBadRequest, codeIsADirectory
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, codeNotFound
	case errors.Is(err, ErrParentNotFound):
		status, code = http.StatusNotFound, codeParentNotFound
	case errors.Is(err, ErrAlreadyExists):
		status, code = http.StatusConflict, codeAlreadyExists
	case errors.Is(err, ErrNotDirectory):
		status, code = http.StatusConflict, codeNotADirectory
	case errors.Is(err, ErrNotEmpty):
		status, code = http.StatusConflict, codeDirNotEmpty
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		msg = "error interno del servidor" // no filtrar detalles al cliente
	}
	writeError(w, status, code, msg)
}

// decodeBody lee el body JSON en v. Si es inválido responde 400 y devuelve false.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(v); err != nil {
		msg := "el body no es un JSON válido"
		if errors.Is(err, io.EOF) {
			msg = "falta el body JSON"
		}
		writeError(w, http.StatusBadRequest, codeInvalidRequest, msg)
		return false
	}
	return true
}
