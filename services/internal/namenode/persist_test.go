package namenode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newPersistEnv crea un servidor de pruebas con persistencia en dir.
func newPersistEnv(t *testing.T, dir string, snapshotEvery int) *writeEnv {
	t.Helper()
	e := newWriteEnv(t, 3)
	must(t, e.srv.EnablePersistence(dir, snapshotEvery))
	t.Cleanup(func() { _ = e.srv.Close() })
	return e
}

// restartEnv simula reiniciar el NameNode: cierra el servidor y crea uno nuevo,
// vacío, que recupera su estado de dir.
func restartEnv(t *testing.T, e *writeEnv, dir string, snapshotEvery int) *writeEnv {
	t.Helper()
	must(t, e.srv.Close())
	return newPersistEnv(t, dir, snapshotEvery)
}

// dumpState devuelve todo el estado de metadatos (sin réplicas) como JSON, para
// comparar el estado antes y después de un reinicio.
func dumpState(e *writeEnv) string {
	e.srv.up.mu.Lock()
	defer e.srv.up.mu.Unlock()
	snap := e.srv.buildSnapshot(0)
	snap.Time = time.Time{}
	b, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func wantSameState(t *testing.T, before, after string) {
	t.Helper()
	if before != after {
		t.Errorf("el estado recuperado no es idéntico\n--- antes ---\n%s\n--- después ---\n%s", before, after)
	}
}

// logEntries lee las entradas que hay ahora mismo en edits.log.
func logEntries(t *testing.T, dir string) []EditEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, editLogFileName))
	if err != nil {
		t.Fatal(err)
	}
	var out []EditEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var en EditEntry
		must(t, json.Unmarshal(line, &en))
		out = append(out, en)
	}
	return out
}

// inProgress describe una subida que se deja a medias en populate.
type inProgress struct {
	path, uploadID, pendingBlock string
}

// populate ejecuta un caso de uso variado: directorios, archivos completos,
// borrados, un archivo vacío y una subida a medias con un bloque pendiente
// reintentado.
func populate(t *testing.T, e *writeEnv) inProgress {
	t.Helper()
	if w := e.post("/fs/mkdir", `{"path":"/user/ana/docs","parents":true}`); w.Code != 201 {
		t.Fatalf("mkdir: HTTP %d (%s)", w.Code, w.Body.String())
	}
	e.post("/fs/mkdir", `{"path":"/tmp"}`)
	putFile(t, e, "/user/ana/a.csv", []int64{10, 10, 5}, "dn1", "dn2", "dn3")
	putFile(t, e, "/user/ana/b.csv", []int64{7}, "dn1", "dn2", "dn3")
	if w := e.rm("/user/ana/b.csv"); w.Code != 204 {
		t.Fatalf("rm: HTTP %d", w.Code)
	}
	e.post("/fs/mkdir", `{"path":"/vacia"}`)
	if w := do(t, e.h, "DELETE", "/fs/rmdir?path=/vacia", ""); w.Code != 204 {
		t.Fatalf("rmdir: HTTP %d", w.Code)
	}
	putFile(t, e, "/user/ana/vacio", nil)

	// Subida a medias: bloque 0 confirmado, bloque 1 asignado dos veces (reintento).
	a0 := e.mustAllocate("/enproceso", 0, 10, "")
	e.mustCommit("/enproceso", a0.UploadID, a0.BlockID, 10, sum('a'))
	e.mustAllocate("/enproceso", 1, 10, a0.UploadID)
	a1 := e.mustAllocate("/enproceso", 1, 10, a0.UploadID)
	if !strings.HasSuffix(a1.BlockID, "_r1") {
		t.Fatalf("el reintento debía tener sufijo _r1: %s", a1.BlockID)
	}

	// Peticiones inválidas: no deben dejar rastro.
	e.allocate("/user/ana/a.csv", 0, 10, "")
	e.post("/fs/mkdir", `{"path":"/user/ana/a.csv/x"}`)
	e.rm("/no-existe")
	return inProgress{"/enproceso", a0.UploadID, a1.BlockID}
}

func TestPersistenciaRecuperaTodoTrasReiniciar(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	ip := populate(t, e)
	before := dumpState(e)

	e2 := restartEnv(t, e, dir, 0)
	wantSameState(t, before, dumpState(e2))

	// El namespace responde igual, incluidas las fechas.
	w := do(t, e2.h, "GET", "/fs/ls?path=/user/ana", "")
	var ls lsResponse
	must(t, json.Unmarshal(w.Body.Bytes(), &ls))
	if len(ls.Entries) != 3 { // a.csv, docs, vacio (b.csv se borró)
		t.Fatalf("ls = %+v", ls.Entries)
	}
	for _, en := range ls.Entries {
		if en.Name == "a.csv" && (en.State != "COMPLETE" || en.Size != 25 || en.ModifiedAt == "") {
			t.Errorf("a.csv = %+v", en)
		}
	}
	if _, err := e2.ns.Stat("/vacia"); err == nil {
		t.Error("/vacia no debía existir")
	}

	// La subida a medias sigue viva: se puede reintentar, confirmar y cerrar.
	b2 := e2.mustAllocate(ip.path, 1, 10, ip.uploadID)
	if !strings.HasSuffix(b2.BlockID, "_r2") || b2.Offset != 10 {
		t.Errorf("reintento tras reiniciar = %+v; quería sufijo _r2 y offset 10", b2)
	}
	e2.mustCommit(ip.path, ip.uploadID, b2.BlockID, 10, sum('b'))
	if w := e2.finalize(ip.path, ip.uploadID); w.Code != 201 {
		t.Fatalf("finalize tras reiniciar: HTTP %d (%s)", w.Code, w.Body.String())
	}
	// Un archivo ya completo no se puede volver a escribir.
	wantError(t, e2.allocate("/user/ana/a.csv", 0, 10, ""), 409, "ALREADY_EXISTS")
}

func TestPersistenciaNoGuardaLasReplicas(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	e.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	ids := putFile(t, e, "/f", []int64{10, 10}, "dn1", "dn2", "dn3")

	e2 := restartEnv(t, e, dir, 0)
	e2.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	for _, id := range ids {
		b, ok := e2.blocks.Block(id)
		if !ok || !b.Committed() || len(b.Replicas) != 0 {
			t.Errorf("bloque %s tras reiniciar = %+v; debía estar confirmado y sin réplicas", id, b)
		}
	}
	if got := e2.blocks.UnderReplicated(3); len(got) != 2 {
		t.Errorf("los bloques sin réplicas deben figurar como under-replicated: %v", got)
	}
	// Mientras no lleguen los block reports no hay ubicaciones...
	if resp := e2.mustLocations("/f"); len(resp.Blocks) != 2 || len(resp.Blocks[0].Locations) != 0 {
		t.Errorf("locations sin block report = %+v", resp)
	}
	// ...y cuando llegan, get vuelve a funcionar.
	for _, id := range ids {
		for _, n := range []string{"dn1", "dn2", "dn3"} {
			must(t, e2.blocks.AddReplica(id, n, sum(byte('a')+byte(indexOf(ids, id)))))
		}
	}
	resp := e2.mustLocations("/f")
	if resp.Size != 20 || len(resp.Blocks[1].Locations) != 3 {
		t.Errorf("locations tras los block reports = %+v", resp)
	}
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

func TestPersistenciaEscribeAntesDeResponder(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)

	check := func(wantLen int, wantOp EditOp) {
		t.Helper()
		got := logEntries(t, dir)
		if len(got) != wantLen {
			t.Fatalf("el log tiene %d entradas; quería %d", len(got), wantLen)
		}
		if last := got[len(got)-1]; last.Op != wantOp || last.TxID != int64(wantLen) {
			t.Errorf("última entrada = %+v; quería %s con txid %d", last, wantOp, wantLen)
		}
	}
	e.post("/fs/mkdir", `{"path":"/d"}`)
	check(1, OpMkdir)
	a := e.mustAllocate("/d/f", 0, 10, "")
	check(2, OpAllocate)
	e.mustCommit("/d/f", a.UploadID, a.BlockID, 10, sum('a'))
	check(3, OpCommit)
	e.finalize("/d/f", a.UploadID)
	check(4, OpFinalize)
	e.rm("/d/f")
	check(5, OpDelete)
	do(t, e.h, "DELETE", "/fs/rmdir?path=/d", "")
	check(6, OpRmdir)

	// Lo que falla o no cambia nada no se registra.
	e.post("/fs/mkdir", `{"path":"/d2"}`)
	check(7, OpMkdir)
	e.post("/fs/mkdir", `{"path":"/d2"}`)                // 409
	e.post("/fs/mkdir", `{"path":"/d2","parents":true}`) // ya existe: no cambia nada
	e.rm("/nada")                                        // 404
	do(t, e.h, "DELETE", "/fs/rmdir?path=/noexiste", "") // 404
	e.allocate("/x/y/z", 0, 10, "")                      // padre inexistente
	e.commit("/d2", "id", "blk", 1, sum('a'))            // sin subida
	e.finalize("/d2", "id")                              // sin subida
	check(7, OpMkdir)
}

func TestPersistenciaSnapshotYTruncadoDelLog(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 3)
	for i := 0; i < 7; i++ {
		if w := e.post("/fs/mkdir", fmt.Sprintf(`{"path":"/d%d"}`, i)); w.Code != 201 {
			t.Fatalf("mkdir %d: HTTP %d", i, w.Code)
		}
	}
	// Snapshots tras las operaciones 3 y 6: en el log queda solo la 7.
	if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err != nil {
		t.Fatalf("debía existir un snapshot: %v", err)
	}
	if got := logEntries(t, dir); len(got) != 1 || got[0].TxID != 7 {
		t.Errorf("el log debía conservar solo la entrada 7: %+v", got)
	}
	before := dumpState(e)

	e2 := restartEnv(t, e, dir, 3)
	wantSameState(t, before, dumpState(e2))
	// Los txid continúan tras recuperar desde snapshot + log.
	e2.post("/fs/mkdir", `{"path":"/nuevo"}`)
	got := logEntries(t, dir)
	if last := got[len(got)-1]; last.TxID != 8 {
		t.Errorf("el txid siguiente = %d; quería 8", last.TxID)
	}
	wantSameState(t, dumpState(e2), dumpState(restartEnv(t, e2, dir, 3)))
}

func TestPersistenciaCheckpointManual(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	must(t, e.srv.Checkpoint()) // sin cambios: no hace nada
	if _, err := os.Stat(filepath.Join(dir, snapshotFileName)); err == nil {
		t.Error("sin cambios no debía crearse un snapshot")
	}
	populate(t, e)
	must(t, e.srv.Checkpoint())
	if got := logEntries(t, dir); len(got) != 0 {
		t.Errorf("tras el checkpoint el log debía quedar vacío: %d entradas", len(got))
	}
	before := dumpState(e)
	e2 := restartEnv(t, e, dir, 0)
	wantSameState(t, before, dumpState(e2))
}

// Una caída entre guardar el snapshot y vaciar el log deja entradas que el
// snapshot ya incluye: al recuperar deben ignorarse, no aplicarse dos veces.
func TestPersistenciaCaidaEntreSnapshotYTruncado(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	populate(t, e)
	before := dumpState(e)

	e.srv.up.mu.Lock()
	must(t, writeSnapshot(dir, e.srv.buildSnapshot(e.srv.log.LastTxID())))
	e.srv.up.mu.Unlock() // el proceso "cae" aquí: el log no se vació

	if len(logEntries(t, dir)) == 0 {
		t.Fatal("el log debía conservar sus entradas")
	}
	e2 := restartEnv(t, e, dir, 0)
	wantSameState(t, before, dumpState(e2))

	e2.post("/fs/mkdir", `{"path":"/despues"}`)
	after := dumpState(e2)
	e3 := restartEnv(t, e2, dir, 0)
	wantSameState(t, after, dumpState(e3))
}

func TestPersistenciaToleraUnaEscrituraIncompleta(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	populate(t, e)
	before := dumpState(e)
	must(t, e.srv.Close())

	f, _ := os.OpenFile(filepath.Join(dir, editLogFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"txid":999,"op":"mkdir","time":"2026-03-01T15:04:05Z","pa`)
	f.Close()

	e2 := newPersistEnv(t, dir, 0)
	wantSameState(t, before, dumpState(e2))
	if w := e2.post("/fs/mkdir", `{"path":"/sigue"}`); w.Code != 201 {
		t.Errorf("tras reparar el log se debe poder seguir escribiendo: HTTP %d", w.Code)
	}
}

func TestPersistenciaNoArrancaConMetadatosCorruptos(t *testing.T) {
	t.Run("log con hueco de txid", func(t *testing.T) {
		dir := t.TempDir()
		content := `{"txid":1,"op":"mkdir","time":"2026-03-01T15:04:05Z","path":"/a"}` + "\n" +
			`{"txid":2,"op":"mkdir","time":"2026-03-01T15:04:05Z","path":"/b"}` + "\n"
		must(t, os.WriteFile(filepath.Join(dir, editLogFileName), []byte(content), 0o600))
		// Un snapshot que llega hasta el txid 0 y un log que arranca en el 5 dejan un hueco.
		snap := `{"version":1,"lastTxid":0,"time":"2026-03-01T15:04:05Z","root":{"name":"","dir":true,"owner":"root","modTime":"1970-01-01T00:00:00Z"},"files":{},"blocks":{},"uploads":{}}`
		must(t, os.WriteFile(filepath.Join(dir, snapshotFileName), []byte(snap), 0o600))
		content = `{"txid":5,"op":"mkdir","time":"2026-03-01T15:04:05Z","path":"/a"}` + "\n"
		must(t, os.WriteFile(filepath.Join(dir, editLogFileName), []byte(content), 0o600))

		err := newWriteEnv(t, 3).srv.EnablePersistence(dir, 0)
		if err == nil || !strings.Contains(err.Error(), "hueco") {
			t.Errorf("quería un error de hueco, dio %v", err)
		}
	})
	t.Run("snapshot corrupto", func(t *testing.T) {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, snapshotFileName), []byte("{no es json"), 0o600))
		if err := newWriteEnv(t, 3).srv.EnablePersistence(dir, 0); err == nil {
			t.Error("un snapshot corrupto debía impedir el arranque")
		}
	})
	t.Run("entrada que no se puede aplicar", func(t *testing.T) {
		dir := t.TempDir()
		// rmdir de un directorio que no existe: el log es incoherente.
		content := `{"txid":1,"op":"rmdir","time":"2026-03-01T15:04:05Z","path":"/fantasma"}` + "\n"
		must(t, os.WriteFile(filepath.Join(dir, editLogFileName), []byte(content), 0o600))
		err := newWriteEnv(t, 3).srv.EnablePersistence(dir, 0)
		if err == nil || !strings.Contains(err.Error(), "entrada 1") {
			t.Errorf("quería un error indicando la entrada 1, dio %v", err)
		}
	})
	t.Run("operación desconocida", func(t *testing.T) {
		dir := t.TempDir()
		content := `{"txid":1,"op":"teletransportar","time":"2026-03-01T15:04:05Z","path":"/a"}` + "\n"
		must(t, os.WriteFile(filepath.Join(dir, editLogFileName), []byte(content), 0o600))
		if err := newWriteEnv(t, 3).srv.EnablePersistence(dir, 0); err == nil {
			t.Error("una operación desconocida debía impedir el arranque")
		}
	})
}

func TestPersistenciaSoloSeActivaUnaVez(t *testing.T) {
	e := newPersistEnv(t, t.TempDir(), 0)
	if err := e.srv.EnablePersistence(t.TempDir(), 0); err == nil {
		t.Error("activar la persistencia dos veces debía fallar")
	}
}

// Si el disco falla, el NameNode deja de aceptar cambios (500) pero sigue sirviendo lecturas.
func TestPersistenciaUnFalloDelLogDetieneLasMutaciones(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	e.post("/fs/mkdir", `{"path":"/ok"}`)

	must(t, e.srv.log.f.Close()) // el disco deja de responder
	wantError(t, e.post("/fs/mkdir", `{"path":"/falla"}`), 500, "INTERNAL_ERROR")
	wantError(t, e.post("/fs/mkdir", `{"path":"/otra"}`), 500, "INTERNAL_ERROR")
	wantError(t, e.allocate("/f", 0, 10, ""), 500, "INTERNAL_ERROR")
	if w := do(t, e.h, "GET", "/fs/ls?path=/", ""); w.Code != 200 {
		t.Errorf("las lecturas deben seguir funcionando: HTTP %d", w.Code)
	}
}

func TestPersistenciaAllocateFallidoPorFaltaDeNodosNoDejaRastro(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 0)
	e.placer.nodes = e.placer.nodes[:1]
	wantError(t, e.allocate("/f", 0, 10, ""), 503, "INSUFFICIENT_DATANODES")
	if got := logEntries(t, dir); len(got) != 0 {
		t.Errorf("un allocate fallido no debe registrarse: %+v", got)
	}
	before := dumpState(e)
	e2 := restartEnv(t, e, dir, 0)
	wantSameState(t, before, dumpState(e2))
	if _, err := e2.ns.Stat("/f"); err == nil {
		t.Error("/f no debía existir tras reiniciar")
	}
}

func TestPersistenciaOnAppendReplicaCadaEntrada(t *testing.T) {
	e := newPersistEnv(t, t.TempDir(), 0)
	var seen []EditEntry
	e.srv.log.OnAppend = func(en EditEntry) error {
		seen = append(seen, en)
		return nil
	}
	e.post("/fs/mkdir", `{"path":"/d"}`)
	a := e.mustAllocate("/d/f", 0, 10, "")
	e.mustCommit("/d/f", a.UploadID, a.BlockID, 10, sum('a'))

	ops := []EditOp{OpMkdir, OpAllocate, OpCommit}
	if len(seen) != 3 {
		t.Fatalf("OnAppend vio %d entradas; quería 3", len(seen))
	}
	for i, en := range seen {
		if en.Op != ops[i] || en.TxID != int64(i+1) {
			t.Errorf("entrada %d = %+v", i, en)
		}
	}
	if seen[1].BlockID != a.BlockID || seen[1].UploadID != a.UploadID || !seen[1].NewUpload || seen[2].Checksum != sum('a') {
		t.Errorf("las entradas no traen los datos necesarios para reproducirlas: %+v", seen)
	}

	// Si la replicación falla, el cliente no recibe confirmación.
	e.srv.log.OnAppend = func(EditEntry) error { return errors.New("el standby no responde") }
	wantError(t, e.post("/fs/mkdir", `{"path":"/e"}`), 500, "INTERNAL_ERROR")
}

// Muchas mutaciones simultáneas: el orden del log debe ser el orden de aplicación.
func TestPersistenciaConcurrenteReproduceElMismoEstado(t *testing.T) {
	dir := t.TempDir()
	e := newPersistEnv(t, dir, 25)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Cada goroutine crea un directorio y un archivo dentro: el orden importa.
			if w := e.post("/fs/mkdir", fmt.Sprintf(`{"path":"/d%d/sub","parents":true}`, i)); w.Code != 201 {
				t.Errorf("mkdir %d: HTTP %d", i, w.Code)
				return
			}
			path := fmt.Sprintf("/d%d/sub/f", i)
			a := e.mustAllocate(path, 0, 10, "")
			e.mustCommit(path, a.UploadID, a.BlockID, 10, sum('a'))
			if i%2 == 0 {
				e.finalize(path, a.UploadID)
			}
			if i%5 == 0 {
				e.rm(path)
			}
		}(i)
	}
	wg.Wait()
	before := dumpState(e)

	e2 := restartEnv(t, e, dir, 25)
	wantSameState(t, before, dumpState(e2))
}

// Un servidor sin persistencia activada se comporta como antes.
func TestSinPersistenciaTodoSigueFuncionando(t *testing.T) {
	e := newWriteEnv(t, 3)
	putFile(t, e, "/f", []int64{10}, "dn1", "dn2", "dn3")
	if w := e.rm("/f"); w.Code != http.StatusNoContent {
		t.Errorf("rm: HTTP %d", w.Code)
	}
	must(t, e.srv.Checkpoint()) // no hace nada
	must(t, e.srv.Close())
}
