package namenode

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestFileBlocksEnOrden(t *testing.T) {
	m := NewBlockMap()
	if got := m.FileBlocks("/a"); got != nil {
		t.Errorf("archivo sin bloques: quería nil, dio %v", got)
	}
	for i, id := range []string{"b0", "b1", "b2"} {
		if err := m.AddFileBlock("/a", id, int64(10*(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"b0", "b1", "b2"}
	if got := m.FileBlocks("/a"); !reflect.DeepEqual(got, want) {
		t.Errorf("FileBlocks = %v; quería %v", got, want)
	}
	// Es una copia: modificarla no afecta al mapa.
	got := m.FileBlocks("/a")
	got[0] = "intruso"
	if m.FileBlocks("/a")[0] != "b0" {
		t.Error("FileBlocks debe devolver una copia")
	}
	if err := m.AddFileBlock("/otro", "b0", 5); !errors.Is(err, ErrBlockExists) {
		t.Errorf("bloque repetido: quería ErrBlockExists, dio %v", err)
	}
}

func TestCommitYBlock(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 100))

	info, ok := m.Block("b0")
	if !ok || info.Size != 100 || info.Committed() || len(info.Replicas) != 0 {
		t.Fatalf("bloque recién asignado incorrecto: %+v, %v", info, ok)
	}
	must(t, m.CommitBlock("b0", 90, "abc"))
	info, _ = m.Block("b0")
	if info.Size != 90 || info.Checksum != "abc" || !info.Committed() {
		t.Errorf("después de commit: %+v", info)
	}
	if err := m.CommitBlock("nope", 1, "x"); !errors.Is(err, ErrBlockNotFound) {
		t.Errorf("commit de bloque inexistente: quería ErrBlockNotFound, dio %v", err)
	}
	if _, ok := m.Block("nope"); ok {
		t.Error("Block(nope) no debía existir")
	}
}

func TestAddReplica(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 100))

	// Antes del commit se acepta cualquier checksum (BlockReceived puede llegar primero).
	must(t, m.AddReplica("b0", "dn2", "abc"))
	must(t, m.AddReplica("b0", "dn1", "abc"))
	must(t, m.AddReplica("b0", "dn1", "abc")) // idempotente
	info, _ := m.Block("b0")
	if !reflect.DeepEqual(info.Replicas, []string{"dn1", "dn2"}) {
		t.Errorf("réplicas = %v; quería [dn1 dn2]", info.Replicas)
	}

	must(t, m.CommitBlock("b0", 100, "abc"))
	// Después del commit, un checksum distinto se rechaza y no se registra.
	if err := m.AddReplica("b0", "dn3", "zzz"); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("checksum distinto: quería ErrChecksumMismatch, dio %v", err)
	}
	info, _ = m.Block("b0")
	if len(info.Replicas) != 2 {
		t.Errorf("la réplica rechazada no debía registrarse: %v", info.Replicas)
	}
	if err := m.AddReplica("nope", "dn1", "abc"); !errors.Is(err, ErrBlockNotFound) {
		t.Errorf("réplica de bloque desconocido: quería ErrBlockNotFound, dio %v", err)
	}
}

func TestCountReplicas(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 100))
	must(t, m.AddReplica("b0", "dn1", "abc"))
	must(t, m.AddReplica("b0", "dn2", "abc"))
	must(t, m.AddReplica("b0", "dn3", "otro")) // aún sin commit: se acepta

	if got := m.CountReplicas("b0", "abc"); got != 2 {
		t.Errorf("CountReplicas(abc) = %d; quería 2", got)
	}
	if got := m.CountReplicas("b0", "otro"); got != 1 {
		t.Errorf("CountReplicas(otro) = %d; quería 1", got)
	}
	if got := m.CountReplicas("nope", "abc"); got != 0 {
		t.Errorf("CountReplicas de bloque desconocido = %d; quería 0", got)
	}
}

func TestRemoveReplica(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 1))
	must(t, m.AddReplica("b0", "dn1", "x"))
	must(t, m.AddReplica("b0", "dn2", "x"))

	m.RemoveReplica("b0", "dn1")
	m.RemoveReplica("b0", "dn1") // no falla
	m.RemoveReplica("nope", "dn1")
	info, _ := m.Block("b0")
	if !reflect.DeepEqual(info.Replicas, []string{"dn2"}) {
		t.Errorf("réplicas = %v; quería [dn2]", info.Replicas)
	}
	// El índice inverso también se actualizó.
	if got := m.RemoveNode("dn1"); len(got) != 0 {
		t.Errorf("dn1 ya no tenía bloques, RemoveNode dio %v", got)
	}
}

func TestRemoveNode(t *testing.T) {
	m := NewBlockMap()
	for _, id := range []string{"b0", "b1", "b2"} {
		must(t, m.AddFileBlock("/a", id, 1))
		must(t, m.CommitBlock(id, 1, "x"))
	}
	must(t, m.AddReplica("b0", "dn1", "x"))
	must(t, m.AddReplica("b1", "dn1", "x"))
	must(t, m.AddReplica("b1", "dn2", "x"))
	must(t, m.AddReplica("b2", "dn2", "x"))

	affected := m.RemoveNode("dn1")
	if !reflect.DeepEqual(affected, []string{"b0", "b1"}) {
		t.Errorf("bloques afectados = %v; quería [b0 b1]", affected)
	}
	b0, _ := m.Block("b0")
	b1, _ := m.Block("b1")
	if len(b0.Replicas) != 0 || !reflect.DeepEqual(b1.Replicas, []string{"dn2"}) {
		t.Errorf("tras RemoveNode: b0=%v b1=%v", b0.Replicas, b1.Replicas)
	}
	if got := m.RemoveNode("dn1"); len(got) != 0 {
		t.Errorf("segundo RemoveNode debía dar vacío, dio %v", got)
	}
	if got := m.RemoveNode("desconocido"); len(got) != 0 {
		t.Errorf("RemoveNode de nodo desconocido debía dar vacío, dio %v", got)
	}
}

func TestUnderReplicated(t *testing.T) {
	m := NewBlockMap()
	for _, id := range []string{"b0", "b1", "b2", "b3"} {
		must(t, m.AddFileBlock("/a", id, 1))
	}
	// b0: 3 réplicas, b1: 2, b2: 0 (todos confirmados); b3 sin confirmar.
	for _, id := range []string{"b0", "b1", "b2"} {
		must(t, m.CommitBlock(id, 1, "x"))
	}
	for _, n := range []string{"dn1", "dn2", "dn3"} {
		must(t, m.AddReplica("b0", n, "x"))
	}
	must(t, m.AddReplica("b1", "dn1", "x"))
	must(t, m.AddReplica("b1", "dn2", "x"))
	must(t, m.AddReplica("b3", "dn1", "x"))

	if got := m.UnderReplicated(3); !reflect.DeepEqual(got, []string{"b1", "b2"}) {
		t.Errorf("UnderReplicated(3) = %v; quería [b1 b2] (b3 no está confirmado)", got)
	}
	if got := m.UnderReplicated(2); !reflect.DeepEqual(got, []string{"b2"}) {
		t.Errorf("UnderReplicated(2) = %v; quería [b2]", got)
	}
	if got := m.UnderReplicated(0); len(got) != 0 {
		t.Errorf("UnderReplicated(0) = %v; quería vacío", got)
	}
}

func TestRemoveFile(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 10))
	must(t, m.AddFileBlock("/a", "b1", 20))
	must(t, m.AddFileBlock("/b", "b2", 30))
	must(t, m.AddReplica("b0", "dn1", "x"))
	must(t, m.AddReplica("b0", "dn2", "x"))
	must(t, m.AddReplica("b1", "dn1", "x"))
	must(t, m.AddReplica("b2", "dn1", "x"))

	removed := m.RemoveFile("/a")
	if len(removed) != 2 || removed[0].ID != "b0" || removed[1].ID != "b1" {
		t.Fatalf("RemoveFile(/a) = %+v", removed)
	}
	if !reflect.DeepEqual(removed[0].Replicas, []string{"dn1", "dn2"}) {
		t.Errorf("réplicas de b0 = %v; quería [dn1 dn2]", removed[0].Replicas)
	}
	if m.FileBlocks("/a") != nil {
		t.Error("/a no debía conservar bloques")
	}
	if _, ok := m.Block("b0"); ok {
		t.Error("b0 debía desaparecer")
	}
	// El otro archivo queda intacto y el índice inverso solo conserva b2 en dn1.
	if _, ok := m.Block("b2"); !ok {
		t.Error("b2 no debía borrarse")
	}
	if got := m.RemoveNode("dn1"); !reflect.DeepEqual(got, []string{"b2"}) {
		t.Errorf("dn1 solo debía conservar b2, dio %v", got)
	}
	if got := m.RemoveFile("/nope"); got != nil {
		t.Errorf("RemoveFile de archivo desconocido debía dar nil, dio %v", got)
	}
	// Se puede reutilizar el id de un bloque borrado.
	must(t, m.AddFileBlock("/c", "b0", 1))
}

func TestBlockDevuelveCopia(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 1))
	must(t, m.AddReplica("b0", "dn1", "x"))
	info, _ := m.Block("b0")
	info.Replicas[0] = "intruso"
	again, _ := m.Block("b0")
	if again.Replicas[0] != "dn1" {
		t.Error("Block debe devolver una copia")
	}
}

// Uso concurrente mezclado (pensado para go test -race).
func TestBlockMapConcurrente(t *testing.T) {
	m := NewBlockMap()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/f%d", i)
			id := fmt.Sprintf("b%d", i)
			node := fmt.Sprintf("dn%d", i%4)
			_ = m.AddFileBlock(path, id, 1)
			_ = m.CommitBlock(id, 1, "x")
			_ = m.AddReplica(id, node, "x")
			_ = m.UnderReplicated(3)
			_ = m.CountReplicas(id, "x")
			_, _ = m.Block(id)
			_ = m.FileBlocks(path)
			_ = m.RemoveNode(node)
			_ = m.RemoveFile(path)
		}(i)
	}
	wg.Wait()
	if got := m.UnderReplicated(3); len(got) != 0 {
		t.Errorf("al final no debían quedar bloques: %v", got)
	}
}

func TestRemoveBlock(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 10))
	must(t, m.AddFileBlock("/a", "b1", 10))
	must(t, m.AddReplica("b1", "dn1", "x"))

	m.RemoveBlock("/a", "b1")
	m.RemoveBlock("/a", "b1") // no falla si ya no existe
	m.RemoveBlock("/nope", "zzz")
	if got := m.FileBlocks("/a"); !reflect.DeepEqual(got, []string{"b0"}) {
		t.Errorf("FileBlocks = %v; quería [b0]", got)
	}
	if _, ok := m.Block("b1"); ok {
		t.Error("b1 debía desaparecer")
	}
	if got := m.RemoveNode("dn1"); len(got) != 0 {
		t.Errorf("el índice inverso debía quedar limpio, dio %v", got)
	}
	// Quitar el último bloque deja el archivo sin bloques.
	m.RemoveBlock("/a", "b0")
	if m.FileBlocks("/a") != nil {
		t.Error("el archivo no debía conservar bloques")
	}
}

func TestReplicasWithChecksum(t *testing.T) {
	m := NewBlockMap()
	must(t, m.AddFileBlock("/a", "b0", 1))
	must(t, m.AddReplica("b0", "dn3", "abc"))
	must(t, m.AddReplica("b0", "dn1", "abc"))
	must(t, m.AddReplica("b0", "dn2", "otro")) // antes del commit se acepta

	if got := m.ReplicasWithChecksum("b0", "abc"); !reflect.DeepEqual(got, []string{"dn1", "dn3"}) {
		t.Errorf("ReplicasWithChecksum = %v; quería [dn1 dn3]", got)
	}
	if got := m.ReplicasWithChecksum("b0", "nada"); len(got) != 0 {
		t.Errorf("sin coincidencias debía dar vacío, dio %v", got)
	}
	if got := m.ReplicasWithChecksum("nope", "abc"); got != nil {
		t.Errorf("bloque desconocido debía dar nil, dio %v", got)
	}
}
