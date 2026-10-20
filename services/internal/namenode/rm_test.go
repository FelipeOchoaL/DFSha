package namenode

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
)

// fakeDeleter registra las órdenes de borrado que recibe.
type fakeDeleter struct {
	mu    sync.Mutex
	calls []deleteCall
}

type deleteCall struct {
	node   string
	blocks []string
}

func (d *fakeDeleter) QueueDelete(nodeID string, blockIDs []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deleteCall{nodeID, append([]string(nil), blockIDs...)})
}

func (e *writeEnv) rm(path string) *httptest.ResponseRecorder {
	return do(e.t, e.h, "DELETE", "/fs/rm?path="+url.QueryEscape(path), "")
}

func TestRmArchivoCompleto(t *testing.T) {
	e := newWriteEnv(t, 3)
	del := &fakeDeleter{}
	e.srv.Deletes = del
	e.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")

	// 3 bloques: b0 y b1 en dn1+dn2+dn3 (3 réplicas); el último solo en dn3 y auto-nodos.
	ids := putFile(t, e, "/datos", []int64{10, 10, 5}, "dn1", "dn2", "dn3")
	if len(ids) != 3 {
		t.Fatal("se esperaban 3 bloques")
	}

	w := e.rm("/datos")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("rm: HTTP %d (%s); quería 204 sin cuerpo", w.Code, w.Body.String())
	}
	// Fuera del namespace y del mapa de bloques.
	if _, err := e.ns.Stat("/datos"); !errors.Is(err, ErrNotFound) {
		t.Errorf("el archivo debía desaparecer del namespace: %v", err)
	}
	if e.blocks.FileBlocks("/datos") != nil {
		t.Error("el archivo no debía conservar bloques")
	}
	for _, id := range ids {
		if _, ok := e.blocks.Block(id); ok {
			t.Errorf("el bloque %s debía desaparecer del mapa", id)
		}
	}
	// Un aviso por DataNode con todos sus bloques, en orden de archivo.
	want := []deleteCall{
		{"dn1", ids},
		{"dn2", ids},
		{"dn3", ids},
	}
	if !reflect.DeepEqual(del.calls, want) {
		t.Errorf("órdenes de borrado = %+v; quería %+v", del.calls, want)
	}
	// Un segundo rm ya no encuentra el archivo y no encola nada más.
	wantError(t, e.rm("/datos"), 404, "NOT_FOUND")
	if len(del.calls) != 3 {
		t.Errorf("el segundo rm no debía encolar nada: %+v", del.calls)
	}
}

func TestRmSoloAvisaALosNodosQueTienenBloques(t *testing.T) {
	e := newWriteEnv(t, 3)
	del := &fakeDeleter{}
	e.srv.Deletes = del
	e.autoConfirm = false

	// b0 en dn1, dn2, dn3; b1 en dn2, dn3, dn4.
	a0 := e.mustAllocate("/f", 0, 10, "")
	for _, n := range []string{"dn1", "dn2", "dn3"} {
		must(t, e.blocks.AddReplica(a0.BlockID, n, sum('a')))
	}
	e.mustCommit("/f", a0.UploadID, a0.BlockID, 10, sum('a'))
	a1 := e.mustAllocate("/f", 1, 10, a0.UploadID)
	for _, n := range []string{"dn2", "dn3", "dn4"} {
		must(t, e.blocks.AddReplica(a1.BlockID, n, sum('b')))
	}
	e.mustCommit("/f", a0.UploadID, a1.BlockID, 10, sum('b'))
	e.finalize("/f", a0.UploadID)

	if w := e.rm("/f"); w.Code != 204 {
		t.Fatalf("rm: HTTP %d", w.Code)
	}
	want := []deleteCall{
		{"dn1", []string{a0.BlockID}},
		{"dn2", []string{a0.BlockID, a1.BlockID}},
		{"dn3", []string{a0.BlockID, a1.BlockID}},
		{"dn4", []string{a1.BlockID}},
	}
	if !reflect.DeepEqual(del.calls, want) {
		t.Errorf("órdenes de borrado = %+v; quería %+v", del.calls, want)
	}
}

func TestRmArchivoSinBloquesNoEncolaNada(t *testing.T) {
	e := newWriteEnv(t, 3)
	del := &fakeDeleter{}
	e.srv.Deletes = del
	putFile(t, e, "/vacio", nil)

	if w := e.rm("/vacio"); w.Code != 204 {
		t.Fatalf("rm: HTTP %d", w.Code)
	}
	if len(del.calls) != 0 {
		t.Errorf("no debía encolar órdenes: %+v", del.calls)
	}
}

// rm funciona sin cola de comandos configurada (Deletes == nil).
func TestRmSinColaDeComandos(t *testing.T) {
	e := newWriteEnv(t, 3)
	putFile(t, e, "/f", []int64{10}, "dn1", "dn2", "dn3")
	if w := e.rm("/f"); w.Code != 204 {
		t.Errorf("rm: HTTP %d (%s)", w.Code, w.Body.String())
	}
}

// rm limpia una subida fallida: cierra la subida y libera el path.
func TestRmDeSubidaEnCurso(t *testing.T) {
	e := newWriteEnv(t, 3)
	del := &fakeDeleter{}
	e.srv.Deletes = del
	a := e.mustAllocate("/f", 0, 10, "")
	must(t, e.blocks.AddReplica(a.BlockID, "dn1", sum('a'))) // una copia ya llegó
	e.mustCommit("/f", a.UploadID, a.BlockID, 10, sum('a'))
	b := e.mustAllocate("/f", 1, 10, a.UploadID) // bloque pendiente

	if w := e.rm("/f"); w.Code != 204 {
		t.Fatalf("rm: HTTP %d (%s)", w.Code, w.Body.String())
	}
	if len(e.srv.up.byPath) != 0 {
		t.Error("la subida debía cerrarse")
	}
	// La subida vieja ya no es válida.
	wantError(t, e.commit("/f", a.UploadID, b.BlockID, 10, sum('b')), 404, "NOT_FOUND")
	wantError(t, e.finalize("/f", a.UploadID), 404, "NOT_FOUND")
	// El borrado llegó al nodo que tenía una copia.
	hasDN1 := false
	for _, c := range del.calls {
		if c.node == "dn1" {
			hasDN1 = true
		}
	}
	if !hasDN1 {
		t.Errorf("órdenes de borrado = %+v", del.calls)
	}
	// El path queda libre para volver a subir el archivo.
	n := e.mustAllocate("/f", 0, 10, "")
	if n.UploadID == a.UploadID {
		t.Error("la nueva subida debe tener otro uploadId")
	}
}

func TestRmDeLaSubidaDeOtroUsuario(t *testing.T) {
	e := newWriteEnv(t, 3)
	user := "ana"
	e.srv.Authenticate = func(*http.Request) (string, error) { return user, nil }
	e.mustAllocate("/f", 0, 10, "")

	user = "bob"
	wantError(t, e.rm("/f"), 403, "FORBIDDEN")
	if _, err := e.ns.Stat("/f"); err != nil {
		t.Errorf("el archivo debía conservarse: %v", err)
	}
	user = "ana"
	if w := e.rm("/f"); w.Code != 204 {
		t.Errorf("el dueño de la subida sí puede: HTTP %d", w.Code)
	}
}

func TestRmErrores(t *testing.T) {
	e := newWriteEnv(t, 3)
	must(t, e.ns.Mkdir("/dir", "x", false))

	wantError(t, e.rm("/dir"), 400, "IS_A_DIRECTORY")
	wantError(t, e.rm("/no-existe"), 404, "NOT_FOUND")
	wantError(t, e.rm("/dir/no/existe"), 404, "NOT_FOUND")
	wantError(t, e.rm("/"), 400, "INVALID_PATH")
	wantError(t, e.rm("relativa"), 400, "INVALID_PATH")
	wantError(t, do(t, e.h, "DELETE", "/fs/rm", ""), 400, "INVALID_REQUEST")
	wantError(t, do(t, e.h, "POST", "/fs/rm?path=/f", ""), 405, "METHOD_NOT_ALLOWED")
	// El directorio sigue ahí.
	if _, err := e.ns.Stat("/dir"); err != nil {
		t.Errorf("el directorio debía conservarse: %v", err)
	}
}

func TestRmSinAutenticacion(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Authenticate = func(*http.Request) (string, error) { return "", errors.New("sin token") }
	wantError(t, e.rm("/f"), 401, "UNAUTHORIZED")
}

// Varios rm a la vez de archivos distintos (pensado para go test -race).
func TestRmConcurrente(t *testing.T) {
	e := newWriteEnv(t, 3)
	del := &fakeDeleter{}
	e.srv.Deletes = del
	const n = 15
	for i := 0; i < n; i++ {
		putFile(t, e, fmt.Sprintf("/f%d", i), []int64{10}, "dn1", "dn2", "dn3")
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if w := e.rm(fmt.Sprintf("/f%d", i)); w.Code != 204 {
				t.Errorf("rm /f%d: HTTP %d", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
	if entries, _ := e.ns.List("/"); len(entries) != 0 {
		t.Errorf("no debía quedar nada: %+v", entries)
	}
	if len(del.calls) != n*3 {
		t.Errorf("órdenes = %d; quería %d", len(del.calls), n*3)
	}
}
