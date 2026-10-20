package namenode

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// do ejecuta una petición contra el handler y devuelve la respuesta.
func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// wantError comprueba el código HTTP y el "code" del formato de error común.
func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("HTTP %d; quería %d (body: %s)", w.Code, status, w.Body.String())
		return
	}
	var e errorJSON
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Errorf("el error no es JSON: %v (body: %s)", err, w.Body.String())
		return
	}
	if e.Code != code || e.Error == "" {
		t.Errorf("error = %+v; quería code=%s y mensaje no vacío", e, code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; quería application/json", ct)
	}
}

func newTestHandler(t *testing.T) (http.Handler, *Namespace) {
	t.Helper()
	ns := NewNamespace()
	return NewRESTServer(ns, NewBlockMap(), newFakePlacer(4)).Handler(), ns
}

func TestMkdirREST(t *testing.T) {
	h, ns := newTestHandler(t)

	w := do(t, h, "POST", "/fs/mkdir", `{"path":"/user"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mkdir: HTTP %d (%s)", w.Code, w.Body.String())
	}
	var resp pathResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Path != "/user" {
		t.Errorf("path = %q; quería /user", resp.Path)
	}
	if e, err := ns.Stat("/user"); err != nil || e.Type != TypeDir || e.Owner != anonymousUser {
		t.Errorf("el directorio no quedó creado: %+v, %v", e, err)
	}

	// La ruta se devuelve normalizada.
	w = do(t, h, "POST", "/fs/mkdir", `{"path":"/user//ana/../bob"}`)
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusCreated || resp.Path != "/user/bob" {
		t.Errorf("ruta normalizada: HTTP %d, path %q", w.Code, resp.Path)
	}

	// Errores.
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/user"}`), 409, "ALREADY_EXISTS")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/x/y"}`), 404, "PARENT_NOT_FOUND")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"relativa"}`), 400, "INVALID_PATH")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{}`), 400, "INVALID_REQUEST")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":`), 400, "INVALID_REQUEST")
	wantError(t, do(t, h, "POST", "/fs/mkdir", ``), 400, "INVALID_REQUEST")

	must(t, ns.CreateFile("/user/f.txt", "ana"))
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/user/f.txt/z"}`), 409, "NOT_A_DIRECTORY")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/user/f.txt"}`), 409, "ALREADY_EXISTS")
}

func TestMkdirRESTParents(t *testing.T) {
	h, ns := newTestHandler(t)

	w := do(t, h, "POST", "/fs/mkdir", `{"path":"/a/b/c","parents":true}`)
	if w.Code != http.StatusCreated {
		t.Errorf("mkdir -p nuevo: HTTP %d; quería 201 (%s)", w.Code, w.Body.String())
	}
	if _, err := ns.Stat("/a/b/c"); err != nil {
		t.Errorf("/a/b/c debía existir: %v", err)
	}
	// Repetirlo no falla y responde 200.
	w = do(t, h, "POST", "/fs/mkdir", `{"path":"/a/b/c","parents":true}`)
	if w.Code != http.StatusOK {
		t.Errorf("mkdir -p repetido: HTTP %d; quería 200", w.Code)
	}
	// Un archivo en el camino sigue siendo error.
	must(t, ns.CreateFile("/a/f", "ana"))
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/a/f/z","parents":true}`), 409, "NOT_A_DIRECTORY")
}

func TestLsREST(t *testing.T) {
	h, ns := newTestHandler(t)
	must(t, ns.Mkdir("/user/ana", "ana", true))
	must(t, ns.CreateFile("/user/ana/datos.csv", "ana"))
	must(t, ns.CompleteFile("/user/ana/datos.csv", 20971520))
	must(t, ns.Mkdir("/user/ana/docs", "ana", false))

	w := do(t, h, "GET", "/fs/ls?path=/user/ana", "")
	if w.Code != http.StatusOK {
		t.Fatalf("ls: HTTP %d (%s)", w.Code, w.Body.String())
	}
	var resp lsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Path != "/user/ana" || len(resp.Entries) != 2 {
		t.Fatalf("respuesta = %+v", resp)
	}
	file, dir := resp.Entries[0], resp.Entries[1] // ordenadas por nombre: datos.csv, docs
	if file.Name != "datos.csv" || file.Type != "file" || file.Size != 20971520 ||
		file.Owner != "ana" || file.State != "COMPLETE" || file.ModifiedAt == "" {
		t.Errorf("entrada de archivo = %+v", file)
	}
	if dir.Name != "docs" || dir.Type != "dir" || dir.Size != 0 || dir.State != "" {
		t.Errorf("entrada de directorio = %+v", dir)
	}
	// "state" no debe aparecer en directorios.
	if strings.Count(w.Body.String(), `"state"`) != 1 {
		t.Errorf(`"state" debe aparecer solo en el archivo: %s`, w.Body.String())
	}

	// ls de un archivo devuelve una sola entrada.
	w = do(t, h, "GET", "/fs/ls?path=/user/ana/datos.csv", "")
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || len(resp.Entries) != 1 || resp.Entries[0].Name != "datos.csv" {
		t.Errorf("ls de archivo: HTTP %d, %+v", w.Code, resp)
	}

	// Sin path lista la raíz; un directorio vacío devuelve [] (no null).
	w = do(t, h, "GET", "/fs/ls", "")
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || resp.Path != "/" || len(resp.Entries) != 1 || resp.Entries[0].Name != "user" {
		t.Errorf("ls de la raíz: HTTP %d, %+v", w.Code, resp)
	}
	w = do(t, h, "GET", "/fs/ls?path=/user/ana/docs", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Errorf("directorio vacío debe dar entries:[]: HTTP %d, %s", w.Code, w.Body.String())
	}

	// La ruta va codificada como URL.
	must(t, ns.Mkdir("/con espacio", "ana", false))
	w = do(t, h, "GET", "/fs/ls?path="+url.QueryEscape("/con espacio"), "")
	if w.Code != 200 {
		t.Errorf("ls con ruta codificada: HTTP %d (%s)", w.Code, w.Body.String())
	}

	wantError(t, do(t, h, "GET", "/fs/ls?path=/no-existe", ""), 404, "NOT_FOUND")
	wantError(t, do(t, h, "GET", "/fs/ls?path=relativa", ""), 400, "INVALID_PATH")
	wantError(t, do(t, h, "GET", "/fs/ls?path=/user/ana/datos.csv/x", ""), 409, "NOT_A_DIRECTORY")
}

func TestRmdirREST(t *testing.T) {
	h, ns := newTestHandler(t)
	must(t, ns.Mkdir("/a/b", "ana", true))
	must(t, ns.CreateFile("/a/f", "ana"))

	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=/a", ""), 409, "DIRECTORY_NOT_EMPTY")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=/a/f", ""), 409, "NOT_A_DIRECTORY")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=/zzz", ""), 404, "NOT_FOUND")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=/", ""), 400, "INVALID_PATH")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=relativa", ""), 400, "INVALID_PATH")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir", ""), 400, "INVALID_REQUEST")

	w := do(t, h, "DELETE", "/fs/rmdir?path=/a/b", "")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Errorf("rmdir: HTTP %d, body %q; quería 204 sin cuerpo", w.Code, w.Body.String())
	}
	if _, err := ns.Stat("/a/b"); err == nil {
		t.Error("/a/b debía haberse eliminado")
	}
}

func TestMetodosYRutasDesconocidas(t *testing.T) {
	h, _ := newTestHandler(t)

	w := do(t, h, "POST", "/fs/ls", "")
	wantError(t, w, 405, "METHOD_NOT_ALLOWED")
	if w.Header().Get("Allow") != "GET" {
		t.Errorf("Allow = %q; quería GET", w.Header().Get("Allow"))
	}
	wantError(t, do(t, h, "GET", "/fs/mkdir", ""), 405, "METHOD_NOT_ALLOWED")
	wantError(t, do(t, h, "POST", "/fs/rmdir", ""), 405, "METHOD_NOT_ALLOWED")
	wantError(t, do(t, h, "GET", "/no/existe", ""), 404, "NOT_FOUND")
}

func TestAutenticacionRechazada(t *testing.T) {
	ns := NewNamespace()
	s := NewRESTServer(ns, NewBlockMap(), newFakePlacer(4))
	s.Authenticate = func(*http.Request) (string, error) { return "", errors.New("token inválido") }
	h := s.Handler()

	wantError(t, do(t, h, "GET", "/fs/ls", ""), 401, "UNAUTHORIZED")
	wantError(t, do(t, h, "POST", "/fs/mkdir", `{"path":"/x"}`), 401, "UNAUTHORIZED")
	wantError(t, do(t, h, "DELETE", "/fs/rmdir?path=/x", ""), 401, "UNAUTHORIZED")
}

func TestMkdirUsaElUsuarioAutenticado(t *testing.T) {
	ns := NewNamespace()
	s := NewRESTServer(ns, NewBlockMap(), newFakePlacer(4))
	s.Authenticate = func(*http.Request) (string, error) { return "ana", nil }

	w := do(t, s.Handler(), "POST", "/fs/mkdir", `{"path":"/d"}`)
	if w.Code != 201 {
		t.Fatalf("HTTP %d", w.Code)
	}
	if e, _ := ns.Stat("/d"); e.Owner != "ana" {
		t.Errorf("dueño = %q; quería ana", e.Owner)
	}
}
