// Política de colocación: elige el pipeline de 3 DataNodes por bloque
// (nodos distintos, al menos 2 racks, repartir bloques del mismo archivo).
//
// Historias: US-24 (colocación básica) y US-50 (conciencia de rack).
//
// Algoritmo de Place (para el informe, sección "Algoritmos de particionamiento
// y distribución"):
//
//  1. Candidatos: DataNodes vivos con espacio para el bloque. Si hay menos que
//     el factor de réplica => ErrNotEnoughDataNodes (allocate responde 503).
//  2. Primario (primer nodo del pipeline, el que recibe del cliente): el
//     candidato con más espacio libre, excluyendo el primario del bloque
//     anterior del mismo archivo. Así bloques consecutivos nunca empiezan en el
//     mismo nodo y cada archivo queda esparcido por el clúster (RNF4).
//  3. Resto de réplicas, una a una: entre los que faltan se elige el que
//     minimiza (réplicas ya elegidas en su rack, -espacio libre, id). Con dos o
//     más racks esto fuerza a cubrir un rack distinto antes de repetir uno
//     (RNF2: sobrevivir a la caída de un rack). Con un solo rack degrada a
//     "los de más espacio libre".
package namenode

import (
	"fmt"
	"sort"
)

// Place implementa BlockPlacer (writes.go).
func (r *Registry) Place(path string, blockIndex, replicas int) ([]PipelineNode, error) {
	if replicas <= 0 {
		return nil, fmt.Errorf("factor de replicación inválido: %d", replicas)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var cands []*dataNode
	for _, n := range r.nodes {
		if n.state == NodeAlive {
			cands = append(cands, n)
		}
	}
	if len(cands) < replicas {
		return nil, fmt.Errorf("%w: %d vivos, se necesitan %d", ErrNotEnoughDataNodes, len(cands), replicas)
	}

	// 2. Primario.
	avoid := ""
	if blockIndex > 0 {
		avoid = r.lastPrimary[path]
	}
	sort.Slice(cands, func(i, j int) bool {
		ai, aj := cands[i].ep.ID == avoid, cands[j].ep.ID == avoid
		if ai != aj {
			return !ai // el que hay que evitar va al final
		}
		if cands[i].free() != cands[j].free() {
			return cands[i].free() > cands[j].free()
		}
		return cands[i].ep.ID < cands[j].ep.ID
	})
	primary := cands[0]
	rest := cands[1:]
	pipeline := []PipelineNode{primary.ep}
	racks := map[string]int{primary.ep.Rack: 1}

	// 3. Resto de réplicas.
	for len(pipeline) < replicas {
		sort.Slice(rest, func(i, j int) bool {
			ri, rj := racks[rest[i].ep.Rack], racks[rest[j].ep.Rack]
			if ri != rj {
				return ri < rj
			}
			if rest[i].free() != rest[j].free() {
				return rest[i].free() > rest[j].free()
			}
			return rest[i].ep.ID < rest[j].ep.ID
		})
		pick := rest[0]
		rest = rest[1:]
		racks[pick.ep.Rack]++
		pipeline = append(pipeline, pick.ep)
	}

	r.lastPrimary[path] = primary.ep.ID
	return pipeline, nil
}
