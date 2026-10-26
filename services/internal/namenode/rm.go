// Borrado de archivos: DELETE /fs/rm.
package namenode

import (
	"net/http"
	"sort"
)

// BlockDeleter encola en un DataNode la orden de borrar bloques. Lo implementa
// la cola de comandos del registro de DataNodes (datanodes.go, US-22): la orden
// viaja en la respuesta del siguiente heartbeat como DeleteCommand. Debe ser
// seguro para uso concurrente y no bloquear.
type BlockDeleter interface {
	QueueDelete(nodeID string, blockIDs []string)
}

// handleRm: DELETE /fs/rm?path=<ruta>. Elimina un archivo del namespace y del
// mapa de bloques y ordena a cada DataNode que tenía bloques suyos que los
// borre. También sirve para limpiar un archivo UNDER_CONSTRUCTION (una subida
// fallida); en ese caso se cierra la subida en curso.
//
// El borrado físico es asíncrono: si un DataNode está caído o la orden se
// pierde, el bloque queda huérfano y lo elimina el block report (US-58).
func (s *RESTServer) handleRm(w http.ResponseWriter, r *http.Request, user string) {
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

	if err := s.ns.Authorize(norm, principalOf(r), AccessReq{Parent: AccessWrite | AccessExec}); err != nil {
		writeNamespaceError(w, err)
		return
	}
	// Una subida en curso solo puede cancelarla quien la abrió.
	if u, ok := s.up.byPath[norm]; ok && u.owner != user {
		writeError(w, http.StatusForbidden, codeForbidden, "la subida pertenece a otro usuario: "+norm)
		return
	}
	entry := EditEntry{Op: OpDelete, Time: s.stamp(), Path: norm}
	removed, err := s.applyDelete(entry)
	if err != nil {
		writeNamespaceError(w, err)
		return
	}
	if !s.record(w, &entry) {
		return
	}

	if s.Deletes != nil {
		// Un solo aviso por DataNode con todos sus bloques del archivo.
		perNode := make(map[string][]string)
		for _, b := range removed {
			for _, nodeID := range b.Replicas {
				perNode[nodeID] = append(perNode[nodeID], b.ID)
			}
		}
		nodes := make([]string, 0, len(perNode))
		for nodeID := range perNode {
			nodes = append(nodes, nodeID)
		}
		sort.Strings(nodes)
		for _, nodeID := range nodes {
			s.Deletes.QueueDelete(nodeID, perNode[nodeID])
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
