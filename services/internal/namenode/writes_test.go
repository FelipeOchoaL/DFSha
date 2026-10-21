package namenode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlacer devuelve los primeros n DataNodes de una lista fija. Falla si hay
// menos nodos que réplicas pedidas, como haría la colocación real.
type fakePlacer struct {
	mu        sync.Mutex
	nodes     []PipelineNode
	lastPath  string
	lastIndex int
	calls     int
}

func newFakePlacer(n int) *fakePlacer {
	p := &fakePlacer{}
	for i := 1; i <= n; i++ {
		rack := "rack-a"
		if i%2 == 0 {
			rack = "rack-b"
		}
		p.nodes = append(p.nodes, PipelineNode{
			ID: fmt.Sprintf("datanode-%d", i), Host: fmt.Sprintf("10.0.1.%d", 10+i), Port: 50051, Rack: rack,
		})
	}
	return p
}

func (p *fakePlacer) Place(path string, blockIndex, replicas int) ([]PipelineNode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPath, p.lastIndex = path, blockIndex
	p.calls++
	if len(p.nodes) < replicas {
		return nil, fmt.Errorf("hay %d DataNodes vivos y se piden %d", len(p.nodes), replicas)
	}
	return append([]PipelineNode(nil), p.nodes[:replicas]...), nil
}

// writeEnv agrupa un servidor de pruebas con sus dependencias.
type writeEnv struct {
	t      *testing.T
	h      http.Handler
	ns     *Namespace
	blocks *BlockMap
	placer *fakePlacer
	srv    *RESTServer

	// autoConfirm hace que mustCommit complete las réplicas que falten hasta el
	// factor de réplica, simulando los BlockReceived de los DataNodes.
	autoConfirm bool
}

func newWriteEnv(t *testing.T, dataNodes int) *writeEnv {
	t.Helper()
	ns, bm, pl := NewNamespace(), NewBlockMap(), newFakePlacer(dataNodes)
	srv := NewRESTServer(ns, bm, pl)
	srv.CommitWait = 150 * time.Millisecond
	srv.CommitPollInterval = 5 * time.Millisecond
	return &writeEnv{t: t, h: srv.Handler(), ns: ns, blocks: bm, placer: pl, srv: srv, autoConfirm: true}
}

func (e *writeEnv) post(path, body string) *httptest.ResponseRecorder {
	return do(e.t, e.h, "POST", path, body)
}

func (e *writeEnv) allocate(path string, index int, size int64, uploadID string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"path":%q,"blockIndex":%d,"size":%d,"uploadId":%q}`, path, index, size, uploadID)
	return e.post("/files/allocate", body)
}

func (e *writeEnv) commit(path, uploadID, blockID string, size int64, checksum string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"path":%q,"uploadId":%q,"blockId":%q,"size":%d,"checksumSha256":%q}`,
		path, uploadID, blockID, size, checksum)
	return e.post("/files/commit", body)
}

func (e *writeEnv) finalize(path, uploadID string) *httptest.ResponseRecorder {
	return e.post("/files/finalize", fmt.Sprintf(`{"path":%q,"uploadId":%q}`, path, uploadID))
}

// mustAllocate hace allocate esperando 200 y devuelve la respuesta decodificada.
func (e *writeEnv) mustAllocate(path string, index int, size int64, uploadID string) allocateResponse {
	e.t.Helper()
	w := e.allocate(path, index, size, uploadID)
	if w.Code != http.StatusOK {
		e.t.Fatalf("allocate %s[%d]: HTTP %d (%s)", path, index, w.Code, w.Body.String())
	}
	var resp allocateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// confirmReplicas registra réplicas del bloque hasta alcanzar el factor de
// réplica (los DataNodes simulados se llaman auto-1, auto-2...).
func (e *writeEnv) confirmReplicas(blockID, checksum string) {
	e.t.Helper()
	for i := 1; e.blocks.CountReplicas(blockID, checksum) < e.srv.ReplicationFactor; i++ {
		must(e.t, e.blocks.AddReplica(blockID, fmt.Sprintf("auto-%d", i), checksum))
	}
}

func (e *writeEnv) mustCommit(path, uploadID, blockID string, size int64, checksum string) commitResponse {
	e.t.Helper()
	if e.autoConfirm {
		e.confirmReplicas(blockID, checksum)
	}
	w := e.commit(path, uploadID, blockID, size, checksum)
	if w.Code != http.StatusOK {
		e.t.Fatalf("commit %s: HTTP %d (%s)", blockID, w.Code, w.Body.String())
	}
	var resp commitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

// sum devuelve un SHA-256 hexadecimal falso pero con formato válido.
func sum(c byte) string { return strings.Repeat(string(c), 64) }

func TestPutCompleto(t *testing.T) {
	e := newWriteEnv(t, 4)
	must(t, e.ns.Mkdir("/user/ana", "ana", true))
	const path = "/user/ana/datos.csv"
	const mb = 1 << 20

	a0 := e.mustAllocate(path, 0, 8*mb, "")
	if a0.UploadID == "" || a0.BlockID == "" || a0.Offset != 0 || a0.Size != 8*mb || a0.BlockIndex != 0 {
		t.Errorf("allocate 0 = %+v", a0)
	}
	if len(a0.Pipeline) != 3 || a0.Pipeline[0].ID != "datanode-1" || a0.Pipeline[0].Port != 50051 {
		t.Errorf("pipeline = %+v; quería 3 nodos", a0.Pipeline)
	}
	if e.placer.lastPath != path || e.placer.lastIndex != 0 {
		t.Errorf("el placer recibió path=%q index=%d", e.placer.lastPath, e.placer.lastIndex)
	}
	// Tras el primer allocate el archivo existe en construcción.
	if en, err := e.ns.Stat(path); err != nil || en.State != StateUnderConstruction {
		t.Errorf("Stat tras allocate = %+v, %v", en, err)
	}

	c0 := e.mustCommit(path, a0.UploadID, a0.BlockID, 8*mb, sum('a'))
	if c0.BlockID != a0.BlockID {
		t.Errorf("commit devolvió %+v", c0)
	}

	a1 := e.mustAllocate(path, 1, 8*mb, a0.UploadID)
	if a1.Offset != 8*mb || a1.UploadID != a0.UploadID || a1.BlockID == a0.BlockID {
		t.Errorf("allocate 1 = %+v", a1)
	}
	e.mustCommit(path, a0.UploadID, a1.BlockID, 8*mb, sum('b'))

	a2 := e.mustAllocate(path, 2, 4*mb, a0.UploadID) // último bloque, más chico
	if a2.Offset != 16*mb {
		t.Errorf("offset del bloque 2 = %d; quería %d", a2.Offset, 16*mb)
	}
	e.mustCommit(path, a0.UploadID, a2.BlockID, 4*mb, sum('c'))

	w := e.finalize(path, a0.UploadID)
	if w.Code != http.StatusCreated {
		t.Fatalf("finalize: HTTP %d (%s)", w.Code, w.Body.String())
	}
	var fin finalizeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &fin)
	if fin.Path != path || fin.Size != 20*mb || fin.Blocks != 3 || fin.State != "COMPLETE" {
		t.Errorf("finalize = %+v", fin)
	}

	// Estado final del namespace y del mapa de bloques.
	en, _ := e.ns.Stat(path)
	if en.State != StateComplete || en.Size != 20*mb || en.Owner != "anonymous" {
		t.Errorf("archivo final = %+v", en)
	}
	ids := e.blocks.FileBlocks(path)
	if len(ids) != 3 || ids[0] != a0.BlockID || ids[1] != a1.BlockID || ids[2] != a2.BlockID {
		t.Errorf("bloques del archivo = %v", ids)
	}
	if b, _ := e.blocks.Block(a1.BlockID); b.Checksum != sum('b') || b.Size != 8*mb {
		t.Errorf("bloque 1 = %+v", b)
	}
	if len(e.srv.up.byPath) != 0 {
		t.Error("la subida debía cerrarse al finalizar")
	}
}

func TestAllocateErrores(t *testing.T) {
	e := newWriteEnv(t, 3)
	must(t, e.ns.Mkdir("/d", "ana", false))

	wantError(t, e.allocate("/nodir/f", 0, 10, ""), 404, "PARENT_NOT_FOUND")
	wantError(t, e.allocate("/d", 0, 10, ""), 409, "ALREADY_EXISTS") // es un directorio
	wantError(t, e.allocate("/", 0, 10, ""), 400, "INVALID_PATH")
	wantError(t, e.allocate("relativa", 0, 10, ""), 400, "INVALID_PATH")
	wantError(t, e.allocate("", 0, 10, ""), 400, "INVALID_REQUEST")
	wantError(t, e.allocate("/d/f", 0, -5, ""), 400, "INVALID_REQUEST")
	wantError(t, e.allocate("/d/f", -1, 10, ""), 400, "INVALID_REQUEST")
	wantError(t, e.allocate("/d/f", 1, 10, ""), 400, "INVALID_REQUEST") // sin uploadId
	wantError(t, e.post("/files/allocate", `{"path":`), 400, "INVALID_REQUEST")
	wantError(t, do(t, e.h, "GET", "/files/allocate", ""), 405, "METHOD_NOT_ALLOWED")

	// Ninguno de los intentos fallidos debe haber dejado archivos.
	if entries, _ := e.ns.List("/d"); len(entries) != 0 {
		t.Errorf("no debía quedar nada en /d: %+v", entries)
	}
	if e.placer.calls != 0 {
		t.Errorf("el placer no debía llamarse en peticiones inválidas (%d llamadas)", e.placer.calls)
	}
}

func TestSegundoPutAlMismoPath(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")

	// Otro put mientras el primero está en curso.
	wantError(t, e.allocate("/f", 0, 10, ""), 409, "FILE_BEING_WRITTEN")

	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	if w := e.finalize("/f", a.UploadID); w.Code != 201 {
		t.Fatalf("finalize: HTTP %d", w.Code)
	}
	// Con el archivo COMPLETE ya no se puede volver a escribir.
	wantError(t, e.allocate("/f", 0, 10, ""), 409, "ALREADY_EXISTS")
}

func TestUploadIdYOrden(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")

	wantError(t, e.allocate("/f", 1, 10, "otro-id"), 409, "FILE_BEING_WRITTEN")
	// El bloque 0 está pendiente: no se puede pedir el 1 todavía.
	wantError(t, e.allocate("/f", 1, 10, a.UploadID), 409, "BLOCK_OUT_OF_ORDER")

	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	// El bloque 0 ya está confirmado: no se puede repetir; el 2 se saltaría el 1.
	wantError(t, e.allocate("/f", 0, 10, a.UploadID), 409, "BLOCK_OUT_OF_ORDER")
	wantError(t, e.allocate("/f", 2, 10, a.UploadID), 409, "BLOCK_OUT_OF_ORDER")
	e.mustAllocate("/f", 1, 10, a.UploadID)

	// Path sin subida.
	wantError(t, e.allocate("/otro", 1, 10, a.UploadID), 404, "NOT_FOUND")
}

func TestReintentoDeBloque(t *testing.T) {
	e := newWriteEnv(t, 3)
	a1 := e.mustAllocate("/f", 0, 10, "")
	// Un DataNode reportó una copia del primer intento.
	must(t, e.blocks.AddReplica(a1.BlockID, "datanode-1", sum('x')))

	// El cliente repite allocate del mismo índice con el mismo uploadId.
	a2 := e.mustAllocate("/f", 0, 10, a1.UploadID)
	if a2.BlockID == a1.BlockID || a2.UploadID != a1.UploadID || a2.Offset != 0 {
		t.Errorf("reintento = %+v (primero %+v)", a2, a1)
	}
	if _, ok := e.blocks.Block(a1.BlockID); ok {
		t.Error("el bloque del primer intento debía descartarse")
	}
	if ids := e.blocks.FileBlocks("/f"); len(ids) != 1 || ids[0] != a2.BlockID {
		t.Errorf("bloques del archivo = %v; quería solo %s", ids, a2.BlockID)
	}
	// El commit del bloque descartado ya no es válido.
	wantError(t, e.commit("/f", a1.UploadID, a1.BlockID, 10, sum('a')), 404, "NOT_FOUND")
	e.mustCommit("/f", a1.UploadID, a2.BlockID, 10, sum('a'))
	// Un tercer intento tras confirmar ya no es posible.
	a3 := e.mustAllocate("/f", 1, 10, a1.UploadID)
	a4 := e.mustAllocate("/f", 1, 10, a1.UploadID)
	if a3.BlockID == a4.BlockID {
		t.Error("los ids de reintento deben ser únicos")
	}
}

func TestSinSuficientesDataNodes(t *testing.T) {
	e := newWriteEnv(t, 2) // factor 3 y solo 2 nodos
	wantError(t, e.allocate("/f", 0, 10, ""), 503, "INSUFFICIENT_DATANODES")
	// El allocate fallido no debe dejar el archivo ni una subida abierta.
	if _, err := e.ns.Stat("/f"); err == nil {
		t.Error("/f no debía quedar creado tras un allocate fallido")
	}
	if len(e.srv.up.byPath) != 0 {
		t.Error("no debía quedar una subida abierta")
	}
	// Al recuperarse los nodos, el mismo put funciona.
	e.placer.nodes = newFakePlacer(3).nodes
	e.mustAllocate("/f", 0, 10, "")

	// Fallo en un bloque posterior: se conserva la subida.
	a := e.mustAllocate("/g", 0, 10, "")
	e.mustCommit("/g", a.UploadID, a.BlockID, 10, sum('a'))
	e.placer.nodes = e.placer.nodes[:1]
	wantError(t, e.allocate("/g", 1, 10, a.UploadID), 503, "INSUFFICIENT_DATANODES")
	if _, err := e.ns.Stat("/g"); err != nil {
		t.Errorf("/g debía conservarse: %v", err)
	}
}

func TestFactorDeReplicacionConfigurable(t *testing.T) {
	e := newWriteEnv(t, 4)
	e.srv.ReplicationFactor = 2
	a := e.mustAllocate("/f", 0, 10, "")
	if len(a.Pipeline) != 2 {
		t.Errorf("pipeline con factor 2 = %+v", a.Pipeline)
	}
}

func TestCommitErrores(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")

	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, "corto"), 400, "INVALID_REQUEST")
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, strings.ToUpper(sum('a'))), 400, "INVALID_REQUEST")
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 0, sum('a')), 400, "INVALID_REQUEST")
	wantError(t, e.commit("/f", a.UploadID, "", 10, sum('a')), 400, "INVALID_REQUEST")
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 11, sum('a')), 400, "INVALID_REQUEST") // size distinto al asignado
	wantError(t, e.commit("/f", a.UploadID, "blk_desconocido", 10, sum('a')), 404, "NOT_FOUND")
	wantError(t, e.commit("/f", "otro-id", a.BlockID, 10, sum('a')), 409, "FILE_BEING_WRITTEN")
	wantError(t, e.commit("/nope", a.UploadID, a.BlockID, 10, sum('a')), 404, "NOT_FOUND")

	// Nada de lo anterior debe haber confirmado el bloque.
	if b, _ := e.blocks.Block(a.BlockID); b.Committed() {
		t.Error("el bloque no debía quedar confirmado")
	}
}

func TestCommitIdempotenteYReplicas(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")
	// Los DataNodes avisan (BlockReceived) antes del commit del cliente.
	for _, n := range []string{"datanode-1", "datanode-2", "datanode-3"} {
		must(t, e.blocks.AddReplica(a.BlockID, n, sum('a')))
	}
	c := e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	if c.Replicas != 3 {
		t.Errorf("replicas = %d; quería 3", c.Replicas)
	}
	// El cliente no recibió la respuesta y repite el commit: mismo resultado.
	if c2 := e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a')); c2.Replicas != 3 {
		t.Errorf("commit repetido = %+v", c2)
	}
	// Repetirlo con otro checksum no es el mismo commit.
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('b')), 404, "NOT_FOUND")
}

func TestFinalizeErrores(t *testing.T) {
	e := newWriteEnv(t, 3)
	wantError(t, e.finalize("/nope", "x"), 404, "NOT_FOUND")
	wantError(t, e.post("/files/finalize", `{"path":"/f"}`), 400, "INVALID_REQUEST")

	a := e.mustAllocate("/f", 0, 10, "")
	wantError(t, e.finalize("/f", a.UploadID), 409, "INCOMPLETE_FILE") // bloque sin confirmar
	wantError(t, e.finalize("/f", "otro-id"), 409, "FILE_BEING_WRITTEN")

	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	if w := e.finalize("/f", a.UploadID); w.Code != 201 {
		t.Fatalf("finalize: HTTP %d", w.Code)
	}
	wantError(t, e.finalize("/f", a.UploadID), 409, "ALREADY_EXISTS") // ya completo
	must(t, e.ns.Mkdir("/dir", "x", false))
	wantError(t, e.finalize("/dir", a.UploadID), 400, "IS_A_DIRECTORY")
}

func TestArchivoVacio(t *testing.T) {
	e := newWriteEnv(t, 3)
	// size 0 en el primer allocate abre la subida sin asignar ningún bloque.
	a := e.mustAllocate("/vacio", 0, 0, "")
	if a.UploadID == "" || a.BlockID != "" || len(a.Pipeline) != 0 {
		t.Errorf("allocate de archivo vacío = %+v", a)
	}
	if e.placer.calls != 0 {
		t.Error("un archivo vacío no necesita colocación")
	}
	w := e.finalize("/vacio", a.UploadID)
	if w.Code != 201 {
		t.Fatalf("finalize: HTTP %d (%s)", w.Code, w.Body.String())
	}
	var fin finalizeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &fin)
	if fin.Size != 0 || fin.Blocks != 0 || fin.State != "COMPLETE" {
		t.Errorf("finalize = %+v", fin)
	}
	if en, _ := e.ns.Stat("/vacio"); en.State != StateComplete || en.Size != 0 {
		t.Errorf("archivo = %+v", en)
	}
	// size 0 no vale para bloques posteriores ni para continuar una subida.
	b := e.mustAllocate("/otro", 0, 10, "")
	wantError(t, e.allocate("/otro", 0, 0, b.UploadID), 400, "INVALID_REQUEST")
	wantError(t, e.allocate("/otro", 1, 0, b.UploadID), 400, "INVALID_REQUEST")
}
func TestAutorizacionDeSubida(t *testing.T) {
	e := newWriteEnv(t, 3)
	user := "ana"
	e.srv.Authenticate = func(*http.Request) (string, error) { return user, nil }
	must(t, e.ns.SetMode("/", 0o777)) // los usuarios de esta prueba pueden crear en la raiz

	a := e.mustAllocate("/f", 0, 10, "")
	if en, _ := e.ns.Stat("/f"); en.Owner != "ana" {
		t.Errorf("dueño = %q; quería ana", en.Owner)
	}
	user = "bob"
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('a')), 403, "FORBIDDEN")
	wantError(t, e.finalize("/f", a.UploadID), 403, "FORBIDDEN")
	wantError(t, e.allocate("/f", 0, 10, a.UploadID), 403, "FORBIDDEN")
	user = "ana"
	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
}

func TestWriteSinAutenticacion(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Authenticate = func(*http.Request) (string, error) { return "", errors.New("sin token") }
	wantError(t, e.allocate("/f", 0, 10, ""), 401, "UNAUTHORIZED")
	wantError(t, e.commit("/f", "x", "y", 1, sum('a')), 401, "UNAUTHORIZED")
	wantError(t, e.finalize("/f", "x"), 401, "UNAUTHORIZED")
}

// Dos put simultáneos al mismo path: solo uno puede abrir la subida.
func TestPutsSimultaneos(t *testing.T) {
	e := newWriteEnv(t, 3)
	const n = 30
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.allocate("/mismo", 0, 10, "").Code
		}()
	}
	wg.Wait()
	close(codes)
	ok, conflicts := 0, 0
	for c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			conflicts++
		default:
			t.Errorf("código inesperado %d", c)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Errorf("200=%d 409=%d; quería 1 y %d", ok, conflicts, n-1)
	}
}

// Varios archivos distintos subiéndose a la vez (pensado para go test -race).
func TestSubidasConcurrentes(t *testing.T) {
	e := newWriteEnv(t, 3)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/f%d", i)
			a := e.mustAllocate(path, 0, 10, "")
			e.mustCommit(path, a.UploadID, a.BlockID, 10, sum('a'))
			b := e.mustAllocate(path, 1, 5, a.UploadID)
			e.mustCommit(path, a.UploadID, b.BlockID, 5, sum('b'))
			if w := e.finalize(path, a.UploadID); w.Code != 201 {
				t.Errorf("finalize %s: HTTP %d", path, w.Code)
			}
		}(i)
	}
	wg.Wait()
	entries, _ := e.ns.List("/")
	if len(entries) != 20 {
		t.Errorf("quería 20 archivos, hay %d", len(entries))
	}
	for _, en := range entries {
		if en.State != StateComplete || en.Size != 15 {
			t.Errorf("archivo %s = %+v", en.Name, en)
		}
	}
}

// --- Commit estricto 3 de 3 (US-52) ---

func TestCommitEstrictoSinReplicas(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	a := e.mustAllocate("/f", 0, 10, "")

	w := e.commit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	wantError(t, w, 409, "BLOCK_NOT_REPLICATED")
	if !strings.Contains(w.Body.String(), "0 de 3") {
		t.Errorf("el mensaje debe indicar cuántas réplicas hay: %s", w.Body.String())
	}
	if b, _ := e.blocks.Block(a.BlockID); b.Committed() {
		t.Error("un commit rechazado no debe confirmar el bloque")
	}
	// La subida sigue viva: al llegar las réplicas el mismo commit funciona.
	for _, n := range []string{"dn1", "dn2", "dn3"} {
		must(t, e.blocks.AddReplica(a.BlockID, n, sum('a')))
	}
	if c := e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a')); c.Replicas != 3 {
		t.Errorf("replicas = %d; quería 3", c.Replicas)
	}
}

func TestCommitEstrictoFaltaUnaReplica(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	a := e.mustAllocate("/f", 0, 10, "")
	must(t, e.blocks.AddReplica(a.BlockID, "dn1", sum('a')))
	must(t, e.blocks.AddReplica(a.BlockID, "dn2", sum('a')))

	w := e.commit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	wantError(t, w, 409, "BLOCK_NOT_REPLICATED")
	if !strings.Contains(w.Body.String(), "2 de 3") {
		t.Errorf("el mensaje debe decir 2 de 3: %s", w.Body.String())
	}
}

// Las réplicas con otro checksum no cuentan para el commit.
func TestCommitEstrictoExigeElMismoChecksum(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	a := e.mustAllocate("/f", 0, 10, "")
	must(t, e.blocks.AddReplica(a.BlockID, "dn1", sum('a')))
	must(t, e.blocks.AddReplica(a.BlockID, "dn2", sum('a')))
	must(t, e.blocks.AddReplica(a.BlockID, "dn3", sum('f'))) // copia distinta

	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('a')), 409, "BLOCK_NOT_REPLICATED")
	// Con el checksum de la copia distinta tampoco: solo 1 replica coincide.
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('f')), 409, "BLOCK_NOT_REPLICATED")
}

func TestCommitEstrictoRespetaElFactorConfigurado(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.ReplicationFactor = 2
	a := e.mustAllocate("/f", 0, 10, "")
	must(t, e.blocks.AddReplica(a.BlockID, "dn1", sum('a')))
	must(t, e.blocks.AddReplica(a.BlockID, "dn2", sum('a')))
	if c := e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a')); c.Replicas != 2 {
		t.Errorf("replicas = %d; quería 2", c.Replicas)
	}
}

// Carrera ACK del cliente vs BlockReceived: las réplicas llegan mientras commit espera.
func TestCommitEsperaLasReplicasQueLleganTarde(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 3 * time.Second
	a := e.mustAllocate("/f", 0, 10, "")

	go func() {
		time.Sleep(80 * time.Millisecond)
		for _, n := range []string{"dn1", "dn2", "dn3"} {
			_ = e.blocks.AddReplica(a.BlockID, n, sum('a'))
		}
	}()
	start := time.Now()
	w := e.commit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	if w.Code != http.StatusOK {
		t.Fatalf("commit: HTTP %d (%s)", w.Code, w.Body.String())
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 2*time.Second {
		t.Errorf("commit tardó %v; debía esperar a las réplicas (~80 ms) y no agotar el máximo", d)
	}
}

func TestCommitNoEsperaMasDelMaximo(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 120 * time.Millisecond
	a := e.mustAllocate("/f", 0, 10, "")

	start := time.Now()
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('a')), 409, "BLOCK_NOT_REPLICATED")
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("commit tardó %v; quería ~120 ms", d)
	}
}

func TestCommitSinEsperaConCommitWaitCero(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 0
	a := e.mustAllocate("/f", 0, 10, "")
	start := time.Now()
	wantError(t, e.commit("/f", a.UploadID, a.BlockID, 10, sum('a')), 409, "BLOCK_NOT_REPLICATED")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("con CommitWait=0 no debía esperar, tardó %v", d)
	}
}

// Si el cliente cancela la petición, commit deja de esperar.
func TestCommitSeCancelaConElCliente(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 10 * time.Second
	a := e.mustAllocate("/f", 0, 10, "")

	ctx, cancel := context.WithCancel(context.Background())
	body := fmt.Sprintf(`{"path":"/f","uploadId":%q,"blockId":%q,"size":10,"checksumSha256":%q}`,
		a.UploadID, a.BlockID, sum('a'))
	r := httptest.NewRequest("POST", "/files/commit", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	e.h.ServeHTTP(w, r)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("commit tardó %v tras cancelar el cliente", d)
	}
}

// Mientras un commit espera réplicas no se bloquean las demás subidas.
func TestCommitEnEsperaNoBloqueaOtrasSubidas(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 600 * time.Millisecond
	a := e.mustAllocate("/lenta", 0, 10, "")

	done := make(chan struct{})
	go func() {
		e.commit("/lenta", a.UploadID, a.BlockID, 10, sum('a')) // espera y termina en 409
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // deja que el commit entre en su espera

	start := time.Now()
	e.mustAllocate("/otra", 0, 10, "")
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("allocate de otra subida tardó %v: el commit en espera la bloqueó", d)
	}
	<-done
}

// Dos commits idénticos simultáneos: se aplica uno y el otro responde lo mismo.
func TestCommitsIdenticosSimultaneos(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	e.srv.CommitWait = 3 * time.Second
	a := e.mustAllocate("/f", 0, 10, "")

	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { codes <- e.commit("/f", a.UploadID, a.BlockID, 10, sum('a')).Code }()
	}
	time.Sleep(50 * time.Millisecond)
	for _, n := range []string{"dn1", "dn2", "dn3"} {
		must(t, e.blocks.AddReplica(a.BlockID, n, sum('a')))
	}
	for i := 0; i < 2; i++ {
		if c := <-codes; c != 200 {
			t.Errorf("commit simultáneo: HTTP %d; quería 200", c)
		}
	}
	// El bloque se contó una sola vez: el siguiente tiene offset 10.
	e.autoConfirm = true
	b := e.mustAllocate("/f", 1, 5, a.UploadID)
	if b.Offset != 10 {
		t.Errorf("offset del bloque 1 = %d; quería 10", b.Offset)
	}
}

// Un commit rechazado por falta de réplicas no impide reintentar el bloque con otro allocate.
func TestCommitRechazadoPermiteReintentarElBloque(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.autoConfirm = false
	a1 := e.mustAllocate("/f", 0, 10, "")
	wantError(t, e.commit("/f", a1.UploadID, a1.BlockID, 10, sum('a')), 409, "BLOCK_NOT_REPLICATED")

	a2 := e.mustAllocate("/f", 0, 10, a1.UploadID)
	e.autoConfirm = true
	e.mustCommit("/f", a1.UploadID, a2.BlockID, 10, sum('a'))
	if w := e.finalize("/f", a1.UploadID); w.Code != 201 {
		t.Errorf("finalize: HTTP %d", w.Code)
	}
}

// finalize no vuelve a exigir 3 réplicas: si un DataNode cae después del commit,
// la re-replicación lo repone y el put puede terminar (US-53, US-56).
func TestFinalizeNoExigeReplicasDeNuevo(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")
	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	e.blocks.RemoveNode("auto-1") // un DataNode muere tras el commit

	if w := e.finalize("/f", a.UploadID); w.Code != 201 {
		t.Errorf("finalize: HTTP %d (%s); quería 201", w.Code, w.Body.String())
	}
}

// finalize falla si el mapa de bloques perdió algún bloque del archivo.
func TestFinalizeFallaSiUnBloqueNoEstaConfirmado(t *testing.T) {
	e := newWriteEnv(t, 3)
	a := e.mustAllocate("/f", 0, 10, "")
	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	e.blocks.RemoveBlock("/f", a.BlockID)

	wantError(t, e.finalize("/f", a.UploadID), 409, "INCOMPLETE_FILE")
}
