// Registro de DataNodes vivos: Register, Heartbeat, BlockReport, marcar DEAD
// tras heartbeats perdidos y encolar comandos (re-replicar, borrar).
//
// Historias: US-22 (registro y liveness), US-23 (BlockReceived), US-54/55
// (DeleteCommand), US-56 (re-replicación), US-58 (block report).
//
// Implementa las interfaces que el resto del NameNode ya espera:
//   - BlockPlacer (Place)              -> writes.go, allocate
//   - NodeDirectory (LiveNode)         -> reads.go, locations
//   - BlockDeleter (QueueDelete)       -> rm.go
//   - Reregistrar (RequestReregistration) -> failover.go, promoción
//
// No depende del código gRPC generado: grpc_server.go traduce los mensajes
// protobuf a llamadas de este archivo.
package namenode

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// NodeState es el estado de liveness de un DataNode.
type NodeState int

const (
	NodeAlive NodeState = iota
	NodeDead
)

func (s NodeState) String() string {
	if s == NodeAlive {
		return "ALIVE"
	}
	return "DEAD"
}

// Errores del registro.
var (
	ErrUnknownDataNode    = errors.New("datanode desconocido: debe registrarse")
	ErrNotEnoughDataNodes = errors.New("no hay suficientes datanodes vivos")
	ErrNoReplicationTarget = errors.New("no hay destino disponible para la réplica")
)

// Command es una orden pendiente para un DataNode. Se entrega en la respuesta
// del siguiente heartbeat (el NameNode nunca abre conexiones hacia los DataNodes).
type Command struct {
	// Replicate: copiar BlockID a Targets usando WriteBlock con Targets como pipeline.
	Replicate *ReplicateCommand
	// Delete: borrar los bloques indicados.
	Delete *DeleteCommand
}

type ReplicateCommand struct {
	BlockID string
	Targets []PipelineNode
}

type DeleteCommand struct {
	BlockIDs []string
}

// BlockReportEntry es un bloque tal como lo reporta un DataNode.
type BlockReportEntry struct {
	ID       string
	Size     int64
	Checksum string
}

// dataNode es el registro interno de un DataNode.
type dataNode struct {
	ep            PipelineNode
	capacity      uint64
	used          uint64
	lastHeartbeat time.Time
	state         NodeState
	reregister    bool      // próximo heartbeat responde registered=false
	pending       []Command // comandos a entregar en el próximo heartbeat
	// replicating: bloques cuya re-replicación ya se ordenó a este nodo (origen),
	// para no encolar la misma orden dos veces.
	replicating map[string]time.Time
}

// RegistryOptions configura el registro.
type RegistryOptions struct {
	// HeartbeatInterval es lo que se pide a los DataNodes (RegisterResponse). Por defecto 3 s.
	HeartbeatInterval time.Duration
	// DeadAfter: sin heartbeat durante este tiempo el nodo pasa a DEAD. Por defecto 3 intervalos.
	DeadAfter time.Duration
	// ReplicationFactor para re-replicar. Por defecto 3.
	ReplicationFactor int
	// Now permite inyectar el reloj en pruebas.
	Now func() time.Time
}

func (o RegistryOptions) withDefaults() RegistryOptions {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 3 * time.Second
	}
	if o.DeadAfter <= 0 {
		o.DeadAfter = 3 * o.HeartbeatInterval
	}
	if o.ReplicationFactor <= 0 {
		o.ReplicationFactor = 3
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Registry es el registro de DataNodes. Seguro para uso concurrente.
type Registry struct {
	mu     sync.Mutex
	nodes  map[string]*dataNode
	blocks *BlockMap
	opts   RegistryOptions
	// lastPrimary: path -> id del primario del último bloque asignado a ese
	// archivo, para que bloques consecutivos no empiecen en el mismo nodo.
	lastPrimary map[string]string
}

// NewRegistry crea el registro. blocks es el mapa de bloques del NameNode.
func NewRegistry(blocks *BlockMap, opts RegistryOptions) *Registry {
	return &Registry{
		nodes:       make(map[string]*dataNode),
		blocks:      blocks,
		opts:        opts.withDefaults(),
		lastPrimary: make(map[string]string),
	}
}

// HeartbeatInterval es lo que se informa al DataNode al registrarse.
func (r *Registry) HeartbeatInterval() time.Duration { return r.opts.HeartbeatInterval }

// ---------------------------------------------------------------------------
// Register / Heartbeat / BlockReport / BlockReceived
// ---------------------------------------------------------------------------

// Register da de alta (o vuelve a dar de alta) un DataNode. Devuelve el id con
// el que el NameNode lo conoce (el que anunció el propio nodo).
func (r *Registry) Register(ep PipelineNode, capacity uint64) (string, error) {
	if ep.ID == "" || ep.Host == "" || ep.Port == 0 {
		return "", fmt.Errorf("endpoint incompleto: id, host y port son obligatorios")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opts.Now()
	n, ok := r.nodes[ep.ID]
	if !ok {
		n = &dataNode{replicating: make(map[string]time.Time)}
		r.nodes[ep.ID] = n
		log.Printf("DATANODE %s registrado (%s:%d rack %s)", ep.ID, ep.Host, ep.Port, ep.Rack)
	} else if n.state == NodeDead {
		log.Printf("DATANODE %s vuelve a estar vivo", ep.ID)
	}
	n.ep = ep
	n.capacity = capacity
	n.lastHeartbeat = now
	n.state = NodeAlive
	n.reregister = false
	return ep.ID, nil
}

// Heartbeat registra que el nodo sigue vivo y devuelve los comandos pendientes
// y si el NameNode lo reconoce. Un nodo desconocido (o al que se le pidió
// re-registrarse) recibe registered=false y ningún comando.
func (r *Registry) Heartbeat(id string, capacity, used uint64) (cmds []Command, registered bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok || n.reregister {
		return nil, false
	}
	n.capacity, n.used = capacity, used
	n.lastHeartbeat = r.opts.Now()
	if n.state == NodeDead {
		n.state = NodeAlive
		log.Printf("DATANODE %s vuelve a estar vivo", id)
	}
	cmds = n.pending
	n.pending = nil
	return cmds, true
}

// BlockReport reconcilia el mapa con la lista completa de bloques de un nodo.
// Devuelve los bloques huérfanos (que no pertenecen a ningún archivo) para que
// el DataNode los borre.
func (r *Registry) BlockReport(id string, blocks []BlockReportEntry) (orphans []string, err error) {
	r.mu.Lock()
	n, ok := r.nodes[id]
	if ok {
		n.lastHeartbeat = r.opts.Now()
	}
	r.mu.Unlock()
	if !ok {
		return nil, ErrUnknownDataNode
	}
	for _, b := range blocks {
		if addErr := r.blocks.AddReplica(b.ID, id, b.Checksum); addErr != nil {
			if errors.Is(addErr, ErrBlockNotFound) {
				orphans = append(orphans, b.ID)
			} else {
				log.Printf("AVISO: block report de %s, bloque %s: %v", id, b.ID, addErr)
			}
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}

// BlockReceived registra que un DataNode acaba de guardar un bloque.
func (r *Registry) BlockReceived(id string, b BlockReportEntry) error {
	r.mu.Lock()
	n, ok := r.nodes[id]
	if ok {
		n.lastHeartbeat = r.opts.Now()
		delete(n.replicating, b.ID)
	}
	r.mu.Unlock()
	if !ok {
		return ErrUnknownDataNode
	}
	if err := r.blocks.AddReplica(b.ID, id, b.Checksum); err != nil {
		// Un bloque desconocido puede ser una réplica tardía de un archivo ya
		// borrado: se limpia con el próximo block report.
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Liveness
// ---------------------------------------------------------------------------

// Sweep marca DEAD a los nodos sin heartbeat reciente, quita sus réplicas del
// mapa y encola la re-replicación de los bloques afectados. Se llama
// periódicamente (RunSweeper) o desde pruebas.
func (r *Registry) Sweep() {
	r.mu.Lock()
	now := r.opts.Now()
	var died []string
	for id, n := range r.nodes {
		if n.state == NodeAlive && now.Sub(n.lastHeartbeat) > r.opts.DeadAfter {
			n.state = NodeDead
			n.pending = nil
			died = append(died, id)
		}
	}
	r.mu.Unlock()

	for _, id := range died {
		affected := r.blocks.RemoveNode(id)
		log.Printf("DATANODE %s marcado DEAD (%d bloques afectados)", id, len(affected))
	}
	if len(died) > 0 {
		r.ScheduleReplication()
	}
}

// RunSweeper ejecuta Sweep cada intervalo hasta que done se cierre.
func (r *Registry) RunSweeper(done <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = r.opts.HeartbeatInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			r.Sweep()
		}
	}
}

// Alive devuelve los endpoints de los nodos vivos, ordenados por id.
func (r *Registry) Alive() []PipelineNode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aliveLocked()
}

func (r *Registry) aliveLocked() []PipelineNode {
	var out []PipelineNode
	for _, n := range r.nodes {
		if n.state == NodeAlive {
			out = append(out, n.ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LiveNode implementa NodeDirectory (reads.go).
func (r *Registry) LiveNode(id string) (PipelineNode, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok || n.state != NodeAlive {
		return PipelineNode{}, false
	}
	return n.ep, true
}

// RequestReregistration implementa Reregistrar (failover.go): el próximo
// heartbeat de cada nodo responde registered=false para que se registre de
// nuevo y envíe un block report completo al NameNode recién promovido.
func (r *Registry) RequestReregistration() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.nodes {
		n.reregister = true
	}
}

// ---------------------------------------------------------------------------
// Comandos
// ---------------------------------------------------------------------------

// QueueDelete implementa BlockDeleter (rm.go).
func (r *Registry) QueueDelete(nodeID string, blockIDs []string) {
	if len(blockIDs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return
	}
	n.pending = append(n.pending, Command{Delete: &DeleteCommand{BlockIDs: append([]string(nil), blockIDs...)}})
}

// ScheduleReplication revisa los bloques con menos réplicas que el factor y, para
// cada uno, ordena a un nodo que lo tenga copiarlo a un nodo que no (US-56).
func (r *Registry) ScheduleReplication() {
	for _, blockID := range r.blocks.UnderReplicated(r.opts.ReplicationFactor) {
		info, ok := r.blocks.Block(blockID)
		if !ok {
			continue
		}
		holders := r.blocks.ReplicasWithChecksum(blockID, info.Checksum)
		if len(holders) == 0 {
			log.Printf("AVISO: bloque %s sin réplicas válidas conocidas; no se puede re-replicar", blockID)
			continue
		}
		missing := r.opts.ReplicationFactor - len(holders)
		if missing <= 0 {
			continue
		}
		r.mu.Lock()
		src := r.pickSourceLocked(holders, blockID)
		if src == nil {
			r.mu.Unlock()
			continue
		}
		targets := r.chooseTargetsLocked(holders, missing, info.Size)
		if len(targets) == 0 {
			r.mu.Unlock()
			continue
		}
		src.pending = append(src.pending, Command{Replicate: &ReplicateCommand{BlockID: blockID, Targets: targets}})
		src.replicating[blockID] = r.opts.Now()
		ids := make([]string, len(targets))
		for i, t := range targets {
			ids[i] = t.ID
		}
		log.Printf("RE-REPLICACION: %s copia %s -> %v", src.ep.ID, blockID, ids)
		r.mu.Unlock()
	}
}

// pickSourceLocked elige un nodo vivo que tenga el bloque y al que no se le haya
// ordenado ya replicarlo hace poco.
func (r *Registry) pickSourceLocked(holders []string, blockID string) *dataNode {
	now := r.opts.Now()
	for _, id := range holders {
		n, ok := r.nodes[id]
		if !ok || n.state != NodeAlive {
			continue
		}
		if t, pending := n.replicating[blockID]; pending && now.Sub(t) < 2*r.opts.DeadAfter {
			return nil // ya hay una orden en curso para este bloque
		}
		return n
	}
	return nil
}

// chooseTargetsLocked elige hasta n nodos vivos que no tengan el bloque,
// prefiriendo racks no cubiertos y luego más espacio libre.
func (r *Registry) chooseTargetsLocked(holders []string, n int, size int64) []PipelineNode {
	exclude := make(map[string]bool, len(holders))
	racks := make(map[string]int)
	for _, id := range holders {
		exclude[id] = true
		if h, ok := r.nodes[id]; ok {
			racks[h.ep.Rack]++
		}
	}
	var cands []*dataNode
	for id, node := range r.nodes {
		if node.state == NodeAlive && !exclude[id] && node.free() >= uint64(size) {
			cands = append(cands, node)
		}
	}
	var out []PipelineNode
	for len(out) < n && len(cands) > 0 {
		sort.Slice(cands, func(i, j int) bool {
			ri, rj := racks[cands[i].ep.Rack], racks[cands[j].ep.Rack]
			if ri != rj {
				return ri < rj
			}
			if cands[i].free() != cands[j].free() {
				return cands[i].free() > cands[j].free()
			}
			return cands[i].ep.ID < cands[j].ep.ID
		})
		pick := cands[0]
		cands = cands[1:]
		racks[pick.ep.Rack]++
		out = append(out, pick.ep)
	}
	return out
}

func (d *dataNode) free() uint64 {
	if d.capacity == 0 {
		return 1 << 62 // capacidad desconocida: no limita
	}
	if d.used >= d.capacity {
		return 0
	}
	return d.capacity - d.used
}

// Snapshot describe un DataNode para diagnóstico (logs, API de estado).
type NodeStatus struct {
	PipelineNode
	State         string
	CapacityBytes uint64
	UsedBytes     uint64
	LastHeartbeat time.Time
	Pending       int
}

// Status devuelve el estado de todos los nodos, ordenado por id.
func (r *Registry) Status() []NodeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NodeStatus, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, NodeStatus{
			PipelineNode: n.ep, State: n.state.String(), CapacityBytes: n.capacity,
			UsedBytes: n.used, LastHeartbeat: n.lastHeartbeat, Pending: len(n.pending),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
