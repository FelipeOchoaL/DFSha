package namenode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// fakeDirectory es un NodeDirectory de pruebas: solo los ids registrados están vivos.
type fakeDirectory struct{ alive map[string]PipelineNode }

func newFakeDirectory(ids ...string) *fakeDirectory {
	d := &fakeDirectory{alive: make(map[string]PipelineNode)}
	for i, id := range ids {
		d.alive[id] = PipelineNode{ID: id, Host: "10.0.2." + string(rune('1'+i)), Port: 50051, Rack: "rack-a"}
	}
	return d
}

func (d *fakeDirectory) LiveNode(id string) (PipelineNode, bool) {
	n, ok := d.alive[id]
	return n, ok
}

// putFile sube un archivo completo con bloques de los tamaños dados. El bloque i
// queda con checksum sum('a'+i) y réplicas en los nodos indicados.
func putFile(t *testing.T, e *writeEnv, path string, sizes []int64, replicas ...string) []string {
	t.Helper()
	uploadID := ""
	var blockIDs []string
	for i, size := range sizes {
		a := e.mustAllocate(path, i, size, uploadID)
		uploadID = a.UploadID
		checksum := sum(byte('a' + i))
		for _, n := range replicas {
			must(t, e.blocks.AddReplica(a.BlockID, n, checksum))
		}
		e.mustCommit(path, uploadID, a.BlockID, size, checksum)
		blockIDs = append(blockIDs, a.BlockID)
	}
	if uploadID == "" { // archivo vacío
		uploadID = e.mustAllocate(path, 0, 0, "").UploadID
	}
	if w := e.finalize(path, uploadID); w.Code != http.StatusCreated {
		t.Fatalf("finalize %s: HTTP %d (%s)", path, w.Code, w.Body.String())
	}
	return blockIDs
}

func (e *writeEnv) locations(path string) *httptest.ResponseRecorder {
	return do(e.t, e.h, "GET", "/files/locations?path="+url.QueryEscape(path), "")
}

func (e *writeEnv) mustLocations(path string) locationsResponse {
	e.t.Helper()
	w := e.locations(path)
	if w.Code != http.StatusOK {
		e.t.Fatalf("locations %s: HTTP %d (%s)", path, w.Code, w.Body.String())
	}
	var resp locationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func nodeIDs(nodes []PipelineNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func TestLocationsCompleto(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	blockIDs := putFile(t, e, "/datos.csv", []int64{8, 8, 4}, "dn1", "dn2", "dn3")

	resp := e.mustLocations("/datos.csv")
	if resp.Path != "/datos.csv" || resp.Size != 20 || len(resp.Blocks) != 3 {
		t.Fatalf("respuesta = %+v", resp)
	}
	wantOffsets := []int64{0, 8, 16}
	wantSizes := []int64{8, 8, 4}
	for i, b := range resp.Blocks {
		if b.BlockID != blockIDs[i] || b.BlockIndex != i || b.Offset != wantOffsets[i] ||
			b.Size != wantSizes[i] || b.ChecksumSha256 != sum(byte('a'+i)) {
			t.Errorf("bloque %d = %+v", i, b)
		}
	}
	// Las ubicaciones rotan por bloque para repartir las lecturas en paralelo.
	want := [][]string{{"dn1", "dn2", "dn3"}, {"dn2", "dn3", "dn1"}, {"dn3", "dn1", "dn2"}}
	for i, b := range resp.Blocks {
		if got := nodeIDs(b.Locations); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("ubicaciones del bloque %d = %v; quería %v", i, got, want[i])
		}
		if b.Locations[0].Host == "" || b.Locations[0].Port != 50051 {
			t.Errorf("la ubicación debe traer host y puerto: %+v", b.Locations[0])
		}
	}
}

func TestLocationsSoloNodosVivos(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1", "dn3") // dn2 está muerto
	putFile(t, e, "/f", []int64{5}, "dn1", "dn2", "dn3")

	resp := e.mustLocations("/f")
	if got := nodeIDs(resp.Blocks[0].Locations); !reflect.DeepEqual(got, []string{"dn1", "dn3"}) {
		t.Errorf("ubicaciones = %v; quería [dn1 dn3]", got)
	}
}

func TestLocationsBloqueSinNodosVivos(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory() // ninguno vivo
	putFile(t, e, "/f", []int64{5}, "dn1")

	w := e.locations("/f")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"locations":[]`) {
		t.Errorf("sin nodos vivos debe dar locations:[]: HTTP %d, %s", w.Code, w.Body.String())
	}
}

func TestLocationsExcluyeReplicaConOtroChecksum(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1", "dn2")
	a := e.mustAllocate("/f", 0, 5, "")
	must(t, e.blocks.AddReplica(a.BlockID, "dn1", sum('a')))
	must(t, e.blocks.AddReplica(a.BlockID, "dn2", sum('z'))) // copia corrupta, llegó antes del commit
	e.mustCommit("/f", a.UploadID, a.BlockID, 5, sum('a'))
	e.finalize("/f", a.UploadID)

	resp := e.mustLocations("/f")
	if got := nodeIDs(resp.Blocks[0].Locations); !reflect.DeepEqual(got, []string{"dn1"}) {
		t.Errorf("ubicaciones = %v; quería solo [dn1]", got)
	}
}

func TestLocationsArchivoVacio(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1")
	putFile(t, e, "/vacio", nil)

	w := e.locations("/vacio")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"blocks":[]`) || !strings.Contains(w.Body.String(), `"size":0`) {
		t.Errorf("archivo vacío: HTTP %d, %s", w.Code, w.Body.String())
	}
}

func TestLocationsErrores(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1")
	must(t, e.ns.Mkdir("/dir", "x", false))
	e.mustAllocate("/enconstruccion", 0, 5, "")

	wantError(t, e.locations("/no-existe"), 404, "NOT_FOUND")
	wantError(t, e.locations("/enconstruccion"), 409, "FILE_UNDER_CONSTRUCTION")
	wantError(t, e.locations("/dir"), 400, "IS_A_DIRECTORY")
	wantError(t, e.locations("relativa"), 400, "INVALID_PATH")
	wantError(t, do(t, e.h, "GET", "/files/locations", ""), 400, "INVALID_REQUEST")
	wantError(t, do(t, e.h, "POST", "/files/locations?path=/f", ""), 405, "METHOD_NOT_ALLOWED")
	wantError(t, e.locations("/dir/x/y"), 404, "NOT_FOUND")
}

func TestLocationsSinAutenticacion(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Nodes = newFakeDirectory("dn1")
	e.srv.Authenticate = func(*http.Request) (string, error) { return "", errors.New("sin token") }
	wantError(t, e.locations("/f"), 401, "UNAUTHORIZED")
}

// Si el servidor se arma sin NodeDirectory, locations falla con 500 en lugar de entrar en pánico.
func TestLocationsSinDirectorioConfigurado(t *testing.T) {
	e := newWriteEnv(t, 3)
	putFile(t, e, "/f", []int64{5}, "dn1")
	wantError(t, e.locations("/f"), 500, "INTERNAL_ERROR")
}
