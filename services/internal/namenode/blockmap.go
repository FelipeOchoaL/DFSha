// Mapa archivo -> bloques y bloque -> DataNodes que lo tienen (con checksum).
// Detecta bloques under-replicated.
package namenode

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Errores del mapa de bloques. Se devuelven envueltos con el id del bloque.
var (
	ErrBlockNotFound     = errors.New("el bloque no existe")
	ErrBlockExists       = errors.New("el bloque ya existe")
	ErrChecksumMismatch  = errors.New("el checksum no coincide")
	ErrBlockSizeMismatch = errors.New("el tamaño no coincide")
)

// BlockInfo es una copia de los datos de un bloque. Se devuelve en lugar del
// registro interno para que nadie lo modifique fuera del lock del BlockMap.
type BlockInfo struct {
	ID   string
	Size int64
	// Checksum es el SHA-256 confirmado por el cliente en commit. Vacío mientras
	// el bloque no se ha confirmado.
	Checksum string
	// Replicas son los ids de los DataNodes que tienen el bloque, ordenados.
	Replicas []string
}

// Committed indica si el cliente ya confirmó el bloque (commit).
func (b BlockInfo) Committed() bool { return b.Checksum != "" }

// blockRecord es el registro interno de un bloque.
type blockRecord struct {
	size     int64
	checksum string
	// replicas: datanodeId -> checksum que ese nodo reportó para su copia. Se
	// guarda por réplica para poder exigir "3 de 3 con el mismo checksum" (US-52).
	replicas map[string]string
}

// BlockMap relaciona archivos con bloques y bloques con DataNodes. Es seguro
// para uso concurrente. Las ubicaciones de las réplicas NO se persisten: se
// reconstruyen con los block reports (ver US-70).
type BlockMap struct {
	mu     sync.RWMutex
	files  map[string][]string            // path -> blockIds en orden
	blocks map[string]*blockRecord        // blockId -> registro
	byNode map[string]map[string]struct{} // datanodeId -> blockIds (índice inverso)
}

// NewBlockMap crea un mapa de bloques vacío.
func NewBlockMap() *BlockMap {
	return &BlockMap{
		files:  make(map[string][]string),
		blocks: make(map[string]*blockRecord),
		byNode: make(map[string]map[string]struct{}),
	}
}

// AddFileBlock agrega un bloque al final de la lista de un archivo (allocate).
// El bloque nace sin checksum ni réplicas; size es el tamaño que pidió el cliente.
func (m *BlockMap) AddFileBlock(path, blockID string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.blocks[blockID]; exists {
		return fmt.Errorf("%w: %s", ErrBlockExists, blockID)
	}
	m.blocks[blockID] = &blockRecord{size: size, replicas: make(map[string]string)}
	m.files[path] = append(m.files[path], blockID)
	return nil
}

// CommitBlock registra el tamaño y checksum finales de un bloque (commit).
func (m *BlockMap) CommitBlock(blockID string, size int64, checksum string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.blocks[blockID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrBlockNotFound, blockID)
	}
	rec.size = size
	rec.checksum = checksum
	return nil
}

// FileBlocks devuelve los ids de los bloques de un archivo, en orden. Devuelve
// nil si el archivo no tiene bloques.
func (m *BlockMap) FileBlocks(path string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := m.files[path]
	if len(ids) == 0 {
		return nil
	}
	return append([]string(nil), ids...)
}

// Block devuelve los datos de un bloque, o false si no existe.
func (m *BlockMap) Block(blockID string) (BlockInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, ok := m.blocks[blockID]
	if !ok {
		return BlockInfo{}, false
	}
	return infoOf(blockID, rec), true
}

// AddReplica registra que un DataNode tiene una copia del bloque (BlockReceived
// o block report). checksum es el que reporta el DataNode.
//
// Falla con ErrBlockNotFound si el bloque no pertenece a ningún archivo (el
// block report lo trata como huérfano) y con ErrChecksumMismatch si el bloque
// ya fue confirmado con otro checksum; en ese caso la réplica NO se registra.
func (m *BlockMap) AddReplica(blockID, nodeID, checksum string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.blocks[blockID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrBlockNotFound, blockID)
	}
	if rec.checksum != "" && checksum != rec.checksum {
		return fmt.Errorf("%w: bloque %s en %s", ErrChecksumMismatch, blockID, nodeID)
	}
	rec.replicas[nodeID] = checksum
	set, ok := m.byNode[nodeID]
	if !ok {
		set = make(map[string]struct{})
		m.byNode[nodeID] = set
	}
	set[blockID] = struct{}{}
	return nil
}

// RemoveReplica quita la réplica de un bloque en un DataNode. No falla si no existía.
func (m *BlockMap) RemoveReplica(blockID, nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rec, ok := m.blocks[blockID]; ok {
		delete(rec.replicas, nodeID)
	}
	if set, ok := m.byNode[nodeID]; ok {
		delete(set, blockID)
		if len(set) == 0 {
			delete(m.byNode, nodeID)
		}
	}
}

// RemoveNode quita todas las réplicas de un DataNode (cuando se marca DEAD).
// Devuelve los ids de los bloques afectados, ordenados, para que quien llama
// pueda calcular la re-replicación.
func (m *BlockMap) RemoveNode(nodeID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	set := m.byNode[nodeID]
	affected := make([]string, 0, len(set))
	for blockID := range set {
		if rec, ok := m.blocks[blockID]; ok {
			delete(rec.replicas, nodeID)
		}
		affected = append(affected, blockID)
	}
	delete(m.byNode, nodeID)
	sort.Strings(affected)
	return affected
}

// CountReplicas cuenta las réplicas de un bloque cuyo checksum es el indicado.
// Es la base de la regla de commit estricto (US-52).
func (m *BlockMap) CountReplicas(blockID, checksum string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, ok := m.blocks[blockID]
	if !ok {
		return 0
	}
	n := 0
	for _, c := range rec.replicas {
		if c == checksum {
			n++
		}
	}
	return n
}

// UnderReplicated devuelve, ordenados, los bloques confirmados que tienen menos
// de factor réplicas. Los bloques aún sin confirmar se ignoran: durante un put
// todavía no tienen todas sus copias y no deben disparar re-replicación.
func (m *BlockMap) UnderReplicated(factor int) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ids []string
	for id, rec := range m.blocks {
		if rec.checksum != "" && len(rec.replicas) < factor {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// RemoveFile quita un archivo y todos sus bloques del mapa. Devuelve los bloques
// eliminados con las réplicas que tenían, para que rm pueda encolar un
// DeleteCommand a cada DataNode (US-54). Si el archivo no tiene bloques
// devuelve nil.
func (m *BlockMap) RemoveFile(path string) []BlockInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := m.files[path]
	delete(m.files, path)

	removed := make([]BlockInfo, 0, len(ids))
	for _, id := range ids {
		rec, ok := m.blocks[id]
		if !ok {
			continue
		}
		removed = append(removed, infoOf(id, rec))
		for nodeID := range rec.replicas {
			if set, ok := m.byNode[nodeID]; ok {
				delete(set, id)
				if len(set) == 0 {
					delete(m.byNode, nodeID)
				}
			}
		}
		delete(m.blocks, id)
	}
	if len(removed) == 0 {
		return nil
	}
	return removed
}

func infoOf(id string, rec *blockRecord) BlockInfo {
	replicas := make([]string, 0, len(rec.replicas))
	for nodeID := range rec.replicas {
		replicas = append(replicas, nodeID)
	}
	sort.Strings(replicas)
	return BlockInfo{ID: id, Size: rec.size, Checksum: rec.checksum, Replicas: replicas}
}

// RemoveBlock quita un solo bloque de un archivo y del mapa (por ejemplo un
// bloque asignado que el cliente reintenta con otro pipeline). Las copias que
// hubiera en los DataNodes quedan huérfanas y se limpian con el block report.
// No falla si el bloque no existe.
func (m *BlockMap) RemoveBlock(path, blockID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ids, ok := m.files[path]; ok {
		kept := ids[:0:0]
		for _, id := range ids {
			if id != blockID {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(m.files, path)
		} else {
			m.files[path] = kept
		}
	}
	rec, ok := m.blocks[blockID]
	if !ok {
		return
	}
	for nodeID := range rec.replicas {
		if set, ok := m.byNode[nodeID]; ok {
			delete(set, blockID)
			if len(set) == 0 {
				delete(m.byNode, nodeID)
			}
		}
	}
	delete(m.blocks, blockID)
}

// ReplicasWithChecksum devuelve, ordenados, los ids de los DataNodes cuya copia
// del bloque tiene el checksum indicado. Excluye copias con otro checksum, que
// no deben servirse a los clientes.
func (m *BlockMap) ReplicasWithChecksum(blockID, checksum string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, ok := m.blocks[blockID]
	if !ok {
		return nil
	}
	nodes := make([]string, 0, len(rec.replicas))
	for nodeID, c := range rec.replicas {
		if c == checksum {
			nodes = append(nodes, nodeID)
		}
	}
	sort.Strings(nodes)
	return nodes
}

// --- soporte para persistencia (snapshot, US-70) ---

// snapBlock es la forma serializable de un bloque. Las réplicas NO se guardan:
// se reconstruyen con los block reports de los DataNodes.
type snapBlock struct {
	Size     int64  `json:"size"`
	Checksum string `json:"checksum,omitempty"`
}

// export devuelve copias de la lista de bloques por archivo y de los datos de
// cada bloque, sin las ubicaciones de las réplicas.
func (m *BlockMap) export() (map[string][]string, map[string]snapBlock) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	files := make(map[string][]string, len(m.files))
	for path, ids := range m.files {
		files[path] = append([]string(nil), ids...)
	}
	blocks := make(map[string]snapBlock, len(m.blocks))
	for id, rec := range m.blocks {
		blocks[id] = snapBlock{Size: rec.size, Checksum: rec.checksum}
	}
	return files, blocks
}

// restore reemplaza todo el contenido del mapa. Los bloques quedan sin réplicas.
func (m *BlockMap) restore(files map[string][]string, blocks map[string]snapBlock) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.files = make(map[string][]string, len(files))
	for path, ids := range files {
		m.files[path] = append([]string(nil), ids...)
	}
	m.blocks = make(map[string]*blockRecord, len(blocks))
	for id, b := range blocks {
		m.blocks[id] = &blockRecord{size: b.Size, checksum: b.Checksum, replicas: make(map[string]string)}
	}
	m.byNode = make(map[string]map[string]struct{})
}
