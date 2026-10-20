// Lectura de archivos: GET /files/locations.
package namenode

import (
	"net/http"
)

// codeFileUnderConstruction: se intentó leer un archivo que aún no está COMPLETE.
const codeFileUnderConstruction = "FILE_UNDER_CONSTRUCTION"

// NodeDirectory permite consultar el estado de los DataNodes. Lo implementa el
// registro de DataNodes del NameNode (datanodes.go, US-22); aquí solo se define
// lo que locations necesita, para que ambas partes se integren sin acoplarse.
type NodeDirectory interface {
	// LiveNode devuelve el endpoint anunciado de un DataNode si está vivo. Si el
	// nodo no existe o está DEAD devuelve false.
	LiveNode(id string) (PipelineNode, bool)
}

type blockLocationJSON struct {
	BlockID        string         `json:"blockId"`
	BlockIndex     int            `json:"blockIndex"`
	Offset         int64          `json:"offset"`
	Size           int64          `json:"size"`
	ChecksumSha256 string         `json:"checksumSha256"`
	Locations      []PipelineNode `json:"locations"`
}

type locationsResponse struct {
	Path   string              `json:"path"`
	Size   int64               `json:"size"`
	Blocks []blockLocationJSON `json:"blocks"`
}

// handleLocations: GET /files/locations?path=<ruta>. Devuelve, en orden, cada
// bloque del archivo con los DataNodes vivos que tienen una copia válida.
//
// Las ubicaciones de cada bloque rotan según su índice (el bloque i empieza por
// la réplica i mod n) para que las lecturas en paralelo del cliente se repartan
// entre los nodos en lugar de ir todas al primero.
func (s *RESTServer) handleLocations(w http.ResponseWriter, r *http.Request, _ string) {
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
	if s.Nodes == nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "error interno del servidor")
		return
	}
	entry, err := s.ns.Stat(norm)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	if entry.Type == TypeDir {
		writeError(w, http.StatusBadRequest, codeIsADirectory, "es un directorio: "+norm)
		return
	}
	if entry.State != StateComplete {
		writeError(w, http.StatusConflict, codeFileUnderConstruction,
			"el archivo aún se está escribiendo: "+norm)
		return
	}

	ids := s.blocks.FileBlocks(norm)
	resp := locationsResponse{Path: norm, Size: entry.Size, Blocks: make([]blockLocationJSON, 0, len(ids))}
	var offset int64
	for i, id := range ids {
		info, ok := s.blocks.Block(id)
		if !ok {
			continue // el archivo y el mapa de bloques no deberían divergir; se omite
		}
		resp.Blocks = append(resp.Blocks, blockLocationJSON{
			BlockID:        id,
			BlockIndex:     i,
			Offset:         offset,
			Size:           info.Size,
			ChecksumSha256: info.Checksum,
			Locations:      s.liveLocations(id, info, i),
		})
		offset += info.Size
	}
	writeJSON(w, http.StatusOK, resp)
}

// liveLocations devuelve los DataNodes vivos que tienen el bloque con el
// checksum confirmado, rotados según el índice del bloque. Nunca devuelve nil.
func (s *RESTServer) liveLocations(blockID string, info BlockInfo, index int) []PipelineNode {
	live := make([]PipelineNode, 0, len(info.Replicas))
	for _, nodeID := range s.blocks.ReplicasWithChecksum(blockID, info.Checksum) {
		if n, ok := s.Nodes.LiveNode(nodeID); ok {
			live = append(live, n)
		}
	}
	if len(live) > 1 {
		start := index % len(live)
		live = append(live[start:], live[:start]...)
	}
	return live
}
