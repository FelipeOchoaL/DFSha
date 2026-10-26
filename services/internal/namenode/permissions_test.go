package namenode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// permEnv es un servidor cuyas peticiones se hacen "como" el usuario elegido con as.
type permEnv struct {
	*writeEnv
	who Principal
}

func permify(e *writeEnv) *permEnv {
	p := &permEnv{writeEnv: e}
	e.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	e.srv.Identify = func(*http.Request) (Principal, error) { return p.who, nil }
	return p
}

func (p *permEnv) as(user string, groups ...string) {
	p.who = Principal{User: user, Groups: groups}
}

var (
	ana   = Principal{User: "ana", Groups: []string{"estudiantes"}}
	carla = Principal{User: "carla", Groups: []string{"estudiantes"}}
	bob   = Principal{User: "bob", Groups: []string{"otros"}}
	lupe  = Principal{User: "lupe", Groups: []string{"admin"}}
)

// newPermEnv crea el escenario base: los directorios personales de ana, carla y
// bob, y un archivo completo de ana en /user/ana/datos.
func newPermEnv(t *testing.T) *permEnv {
	t.Helper()
	p := permify(newWriteEnv(t, 3))
	p.setup(t)
	return p
}

func (p *permEnv) setup(t *testing.T) {
	t.Helper()
	for _, u := range []Principal{ana, carla, bob} {
		must(t, p.srv.EnsureHome(u))
	}
	p.as(ana.User, ana.Groups...)
	putFile(t, p.writeEnv, "/user/ana/datos", []int64{10}, "dn1", "dn2", "dn3")
}

func (p *permEnv) chmod(path, mode string) *httptest.ResponseRecorder {
	return p.post("/fs/chmod", fmt.Sprintf(`{"path":%q,"mode":%q}`, path, mode))
}

func (p *permEnv) ls(path string) *httptest.ResponseRecorder {
	return do(p.t, p.h, "GET", "/fs/ls?path="+url.QueryEscape(path), "")
}

func (p *permEnv) mkdir(path string) *httptest.ResponseRecorder {
	return p.post("/fs/mkdir", fmt.Sprintf(`{"path":%q}`, path))
}

func TestHomeDeUsuario(t *testing.T) {
	p := newPermEnv(t)
	for _, c := range []struct {
		path, owner, group string
		mode               uint32
	}{
		{"/user", "root", "", 0o755},
		{"/user/ana", "ana", "estudiantes", 0o750},
		{"/user/bob", "bob", "otros", 0o750},
	} {
		e, err := p.ns.Stat(c.path)
		if err != nil || e.Type != TypeDir || e.Owner != c.owner || e.Group != c.group || e.Mode != c.mode {
			t.Errorf("%s = %+v, %v; quería dueño %s grupo %q modo %04o", c.path, e, err, c.owner, c.group, c.mode)
		}
	}
	// El archivo de ana nace privado: dueño todo, grupo leer, los demás nada.
	f, _ := p.ns.Stat("/user/ana/datos")
	if f.Owner != "ana" || f.Group != "estudiantes" || f.Mode != 0o640 {
		t.Errorf("archivo = %+v", f)
	}
	// ls informa grupo y permisos.
	var ls lsResponse
	must(t, json.Unmarshal(p.ls("/user/ana").Body.Bytes(), &ls))
	if len(ls.Entries) != 1 || ls.Entries[0].Group != "estudiantes" ||
		ls.Entries[0].Mode != "0640" || ls.Entries[0].Permissions != "rw-r-----" {
		t.Errorf("ls = %+v", ls.Entries)
	}
}

func TestEnsureHomeEsIdempotenteYValida(t *testing.T) {
	dir := t.TempDir()
	p := permify(newPersistEnv(t, dir, 0))
	must(t, p.srv.EnsureHome(ana))
	must(t, p.srv.EnsureHome(ana))
	must(t, p.srv.EnsureHome(bob))
	// /user, /user/ana y /user/bob: tres entradas, sin repetir.
	got := logEntries(t, dir)
	if len(got) != 3 || got[0].Path != "/user" || got[1].Path != "/user/ana" || got[2].Path != "/user/bob" {
		t.Fatalf("entradas del log: %+v", got)
	}
	if got[1].Owner != "ana" || got[1].Group != "estudiantes" || got[1].Mode != 0o750 {
		t.Errorf("entrada del home: %+v", got[1])
	}
	for _, bad := range []string{"", "..", ".", "a/b", "x\x00y"} {
		if err := p.srv.EnsureHome(Principal{User: bad}); err == nil {
			t.Errorf("EnsureHome(%q) debía fallar", bad)
		}
	}
}

// El criterio de aceptación: el usuario B no puede hacer get ni rm de un archivo privado de A.
func TestOtroUsuarioNoPuedeLeerNiBorrarElArchivoPrivado(t *testing.T) {
	p := newPermEnv(t)
	p.as(bob.User, bob.Groups...)

	wantError(t, p.locations("/user/ana/datos"), 403, "FORBIDDEN") // get
	wantError(t, p.rm("/user/ana/datos"), 403, "FORBIDDEN")        // rm
	wantError(t, p.ls("/user/ana"), 403, "FORBIDDEN")
	wantError(t, p.mkdir("/user/ana/mio"), 403, "FORBIDDEN")
	wantError(t, p.allocate("/user/ana/mio.txt", 0, 10, ""), 403, "FORBIDDEN")
	wantError(t, p.chmod("/user/ana/datos", "0777"), 403, "FORBIDDEN")

	// Nada cambió: el dueño sigue leyéndolo.
	p.as(ana.User, ana.Groups...)
	if resp := p.mustLocations("/user/ana/datos"); resp.Size != 10 {
		t.Errorf("locations del dueño = %+v", resp)
	}
}

func TestNoSeRevelaSiAlgoExisteSinPermisoParaVerlo(t *testing.T) {
	p := newPermEnv(t)
	p.as(bob.User, bob.Groups...)
	// Existe o no, bob recibe lo mismo: no puede entrar en /user/ana.
	wantError(t, p.ls("/user/ana/noexiste"), 403, "FORBIDDEN")
	wantError(t, p.ls("/user/ana/datos"), 403, "FORBIDDEN")
	wantError(t, p.post("/fs/mkdir", `{"path":"/user/ana/a/b","parents":true}`), 403, "FORBIDDEN")
	wantError(t, p.rm("/user/ana/noexiste"), 403, "FORBIDDEN")
	// En un directorio que sí puede ver, lo que no existe es 404.
	wantError(t, p.ls("/user/bob/noexiste"), 404, "NOT_FOUND")
}

func TestUsuarioDelMismoGrupoPuedeLeerPeroNoEscribir(t *testing.T) {
	p := newPermEnv(t)
	p.as(carla.User, carla.Groups...)

	if w := p.ls("/user/ana"); w.Code != 200 {
		t.Errorf("ls del grupo: HTTP %d", w.Code)
	}
	if resp := p.mustLocations("/user/ana/datos"); resp.Size != 10 {
		t.Errorf("locations del grupo = %+v", resp)
	}
	wantError(t, p.rm("/user/ana/datos"), 403, "FORBIDDEN")
	wantError(t, p.mkdir("/user/ana/x"), 403, "FORBIDDEN")
	wantError(t, p.allocate("/user/ana/y", 0, 10, ""), 403, "FORBIDDEN")
	wantError(t, p.chmod("/user/ana/datos", "0666"), 403, "FORBIDDEN") // no es el dueño
}

func TestElDuenioAbreYCierraSusPermisosConChmod(t *testing.T) {
	p := newPermEnv(t)

	// ana abre su directorio y su archivo a todos: lectura sí, escritura no.
	if w := p.chmod("/user/ana", "755"); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"0755"`) {
		t.Fatalf("chmod: HTTP %d (%s)", w.Code, w.Body.String())
	}
	if w := p.chmod("/user/ana/datos", "0644"); w.Code != 200 {
		t.Fatalf("chmod: HTTP %d", w.Code)
	}
	p.as(bob.User, bob.Groups...)
	if resp := p.mustLocations("/user/ana/datos"); resp.Size != 10 {
		t.Errorf("bob debería poder leerlo ahora: %+v", resp)
	}
	wantError(t, p.rm("/user/ana/datos"), 403, "FORBIDDEN")

	// ana da escritura al grupo: carla ya puede crear y borrar ahí, bob no.
	p.as(ana.User, ana.Groups...)
	p.chmod("/user/ana", "0770")
	p.as(carla.User, carla.Groups...)
	if w := p.mkdir("/user/ana/proyecto"); w.Code != 201 {
		t.Errorf("carla debería poder crear: HTTP %d (%s)", w.Code, w.Body.String())
	}
	p.as(bob.User, bob.Groups...)
	wantError(t, p.mkdir("/user/ana/otro"), 403, "FORBIDDEN")
	p.as(carla.User, carla.Groups...)
	if w := p.rm("/user/ana/datos"); w.Code != 204 {
		t.Errorf("carla debería poder borrar: HTTP %d", w.Code)
	}

	// ana lo cierra de nuevo.
	p.as(ana.User, ana.Groups...)
	p.chmod("/user/ana", "0700")
	p.as(carla.User, carla.Groups...)
	wantError(t, p.ls("/user/ana"), 403, "FORBIDDEN")
}

func TestPermisoDeEjecucionSinLecturaPermiteLlegarPeroNoListar(t *testing.T) {
	p := newPermEnv(t)
	p.chmod("/user/ana", "0710") // el grupo solo puede atravesar el directorio
	p.as(carla.User, carla.Groups...)

	wantError(t, p.ls("/user/ana"), 403, "FORBIDDEN")
	if resp := p.mustLocations("/user/ana/datos"); resp.Size != 10 { // conoce el nombre y el archivo es legible
		t.Errorf("locations = %+v", resp)
	}
}

func TestChmodValidacion(t *testing.T) {
	p := newPermEnv(t)
	for _, mode := range []string{"999", "7777", "rwx", "64", "0o640", "-1", ""} {
		wantError(t, p.chmod("/user/ana/datos", mode), 400, "INVALID_REQUEST")
	}
	wantError(t, p.post("/fs/chmod", `{"mode":"0644"}`), 400, "INVALID_REQUEST")
	wantError(t, p.post("/fs/chmod", `{"path":`), 400, "INVALID_REQUEST")
	wantError(t, p.chmod("/user/ana/no-existe", "0644"), 404, "NOT_FOUND")
	wantError(t, p.chmod("relativa", "0644"), 400, "INVALID_PATH")
	wantError(t, do(t, p.h, "GET", "/fs/chmod", ""), 405, "METHOD_NOT_ALLOWED")
	if f, _ := p.ns.Stat("/user/ana/datos"); f.Mode != 0o640 {
		t.Errorf("los intentos inválidos no deben cambiar nada: %04o", f.Mode)
	}
	// Se aceptan 3 o 4 dígitos.
	for _, mode := range []string{"600", "0600"} {
		if w := p.chmod("/user/ana/datos", mode); w.Code != 200 {
			t.Errorf("chmod %s: HTTP %d", mode, w.Code)
		}
	}
}

func TestSuperusuarioPuedeTodo(t *testing.T) {
	for _, su := range []Principal{{User: "root"}, lupe, {User: "svc", Super: true}} {
		t.Run(su.User, func(t *testing.T) {
			p := newPermEnv(t)
			p.as(su.User, su.Groups...)
			p.who.Super = su.Super
			if w := p.ls("/user/ana"); w.Code != 200 {
				t.Errorf("ls: HTTP %d", w.Code)
			}
			p.mustLocations("/user/ana/datos")
			if w := p.chmod("/user/ana/datos", "0600"); w.Code != 200 {
				t.Errorf("chmod ajeno: HTTP %d", w.Code)
			}
			if w := p.mkdir("/user/ana/dentro"); w.Code != 201 {
				t.Errorf("mkdir ajeno: HTTP %d", w.Code)
			}
			if w := p.rm("/user/ana/datos"); w.Code != 204 {
				t.Errorf("rm ajeno: HTTP %d", w.Code)
			}
		})
	}
}

func TestMkdirPSobreAlgoQueYaExisteNoNecesitaPermisoDeEscritura(t *testing.T) {
	p := newPermEnv(t)
	p.as(ana.User, ana.Groups...) // ana no puede escribir en /user (0755 de root)
	wantError(t, p.mkdir("/user/otra"), 403, "FORBIDDEN")
	if w := p.post("/fs/mkdir", `{"path":"/user/ana","parents":true}`); w.Code != 200 {
		t.Errorf("mkdir -p de su propio directorio: HTTP %d; quería 200", w.Code)
	}
}

func TestLosDirectoriosYArchivosNuevosTomanElGrupoDelUsuario(t *testing.T) {
	p := newPermEnv(t)
	if w := p.post("/fs/mkdir", `{"path":"/user/ana/a/b","parents":true}`); w.Code != 201 {
		t.Fatalf("mkdir -p: HTTP %d", w.Code)
	}
	for _, path := range []string{"/user/ana/a", "/user/ana/a/b"} {
		e, _ := p.ns.Stat(path)
		if e.Owner != "ana" || e.Group != "estudiantes" || e.Mode != 0o750 {
			t.Errorf("%s = %+v", path, e)
		}
	}
}

func TestLaSubidaSigueSiendoSoloDelQueLaAbrio(t *testing.T) {
	p := newPermEnv(t)
	p.chmod("/user/ana", "0770")
	a := p.mustAllocate("/user/ana/nuevo", 0, 10, "")
	p.as(carla.User, carla.Groups...) // carla puede escribir en el directorio, pero la subida no es suya
	wantError(t, p.commit("/user/ana/nuevo", a.UploadID, a.BlockID, 10, sum('a')), 403, "FORBIDDEN")
	wantError(t, p.finalize("/user/ana/nuevo", a.UploadID), 403, "FORBIDDEN")
}

func TestSinAutenticacionElUsuarioAnonimoTieneTodosLosPermisos(t *testing.T) {
	e := newWriteEnv(t, 3) // sin Identify ni Authenticate
	must(t, e.ns.Mkdir("/privado", "alguien", false))
	must(t, e.ns.SetMode("/privado", 0o700))
	if w := do(t, e.h, "GET", "/fs/ls?path=/privado", ""); w.Code != 200 {
		t.Errorf("ls en modo sin autenticación: HTTP %d", w.Code)
	}
}

func TestIdentifyConErrorDa401(t *testing.T) {
	e := newWriteEnv(t, 3)
	e.srv.Identify = func(*http.Request) (Principal, error) { return Principal{}, fmt.Errorf("token vencido") }
	wantError(t, do(t, e.h, "GET", "/fs/ls", ""), 401, "UNAUTHORIZED")
	wantError(t, do(t, e.h, "POST", "/fs/chmod", `{"path":"/","mode":"0755"}`), 401, "UNAUTHORIZED")
}

func TestAuthorizeMatrizDeBits(t *testing.T) {
	ns := NewNamespace()
	must(t, ns.MkdirAs("/d", "ana", "g1", 0o751, false))
	must(t, ns.CreateFileAs("/d/f", "ana", "g1", 0o640))

	owner := Principal{User: "ana"}
	group := Principal{User: "x", Groups: []string{"g1"}}
	other := Principal{User: "y", Groups: []string{"g2"}}

	rw := AccessRead | AccessWrite
	wx := AccessWrite | AccessExec
	cases := []struct {
		name string
		who  Principal
		path string
		req  AccessReq
		ok   bool
	}{
		{"dueño lee su archivo", owner, "/d/f", AccessReq{File: AccessRead}, true},
		{"dueño escribe su archivo", owner, "/d/f", AccessReq{File: rw}, true},
		{"grupo lee", group, "/d/f", AccessReq{File: AccessRead}, true},
		{"grupo no escribe", group, "/d/f", AccessReq{File: AccessWrite}, false},
		{"otro no lee", other, "/d/f", AccessReq{File: AccessRead}, false},
		{"otro puede atravesar (x)", other, "/d/f", AccessReq{}, true},
		{"otro no lista el directorio", other, "/d", AccessReq{Dir: AccessRead}, false},
		{"grupo no crea en /d (sin w)", group, "/d/nuevo", AccessReq{Parent: wx}, false},
		{"dueño crea en /d", owner, "/d/nuevo", AccessReq{Parent: wx}, true},
		{"ruta inexistente en dir accesible: sin error", other, "/d/no/existe", AccessReq{}, true},
		{"intermedio inexistente con CreateParents pide w", other, "/d/a/b", AccessReq{Parent: wx, CreateParents: true}, false},
		{"intermedio inexistente sin CreateParents: sin error", other, "/d/a/b", AccessReq{Parent: wx}, true},
		{"raiz", other, "/", AccessReq{Dir: AccessRead}, true},
		{"super", Principal{User: "q", Super: true}, "/d/f", AccessReq{File: rw}, true},
	}
	for _, c := range cases {
		err := ns.Authorize(c.path, c.who, c.req)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v; quería ok=%v", c.name, err, c.ok)
		}
	}
	// Un archivo sin grupo nunca da permisos de grupo a quien no tiene grupos.
	must(t, ns.CreateFileAs("/d/sg", "ana", "", 0o060))
	if err := ns.Authorize("/d/sg", Principal{User: "z"}, AccessReq{File: AccessRead}); err == nil {
		t.Error("un archivo sin grupo no debe dar permisos de grupo a quien no tiene grupos")
	}
}

func TestParseYFormatMode(t *testing.T) {
	for in, want := range map[string]uint32{"640": 0o640, "0640": 0o640, "000": 0, "0777": 0o777, " 755 ": 0o755} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %o, %v; quería %o", in, got, err, want)
		}
	}
	for mode, want := range map[uint32]string{0o640: "rw-r-----", 0o755: "rwxr-xr-x", 0: "---------", 0o777: "rwxrwxrwx", 0o001: "--------x"} {
		if got := FormatMode(mode); got != want {
			t.Errorf("FormatMode(%04o) = %q; quería %q", mode, got, want)
		}
	}
}

// Dueño, grupo y permisos sobreviven a un reinicio, y se siguen aplicando.
func TestPermisosSePersistenYSeReplican(t *testing.T) {
	dir := t.TempDir()
	p := permify(newPersistEnv(t, dir, 0))
	p.setup(t)
	p.chmod("/user/ana", "0750")
	p.chmod("/user/ana/datos", "0600")
	before := dumpState(p.writeEnv)

	e2 := restartEnv(t, p.writeEnv, dir, 0)
	wantSameState(t, before, dumpState(e2))
	p2 := permify(e2)
	f, _ := p2.ns.Stat("/user/ana/datos")
	if f.Owner != "ana" || f.Group != "estudiantes" || f.Mode != 0o600 {
		t.Errorf("tras reiniciar: %+v", f)
	}
	p2.as(carla.User, carla.Groups...)
	wantError(t, p2.locations("/user/ana/datos"), 403, "FORBIDDEN") // 0600: el grupo ya no lee
	p2.as(bob.User, bob.Groups...)
	wantError(t, p2.ls("/user/ana"), 403, "FORBIDDEN")
	p2.as(ana.User, ana.Groups...)
	p2.mustLocations("/user/ana/datos")

	// Con un standby, recibe también los permisos.
	pair := newHAPair(t, ReplicatorOptions{})
	pa := permify(pair.active)
	pa.setup(t)
	pa.chmod("/user/ana/datos", "0604")
	pair.wantInSync()
	f, _ = pair.stand.ns.Stat("/user/ana/datos")
	if f.Mode != 0o604 || f.Group != "estudiantes" {
		t.Errorf("en el standby: %+v", f)
	}
}

// Un snapshot anterior a los permisos se carga con los permisos por defecto.
func TestSnapshotAnteriorAPermisosSeCargaConValoresPorDefecto(t *testing.T) {
	dir := t.TempDir()
	snap := `{"version":1,"lastTxid":0,"time":"2026-03-01T15:04:05Z",` +
		`"root":{"name":"","dir":true,"owner":"root","modTime":"1970-01-01T00:00:00Z","children":[` +
		`{"name":"d","dir":true,"owner":"ana","modTime":"2026-03-01T15:04:05Z","children":[` +
		`{"name":"f","owner":"ana","modTime":"2026-03-01T15:04:05Z","size":3,"state":"COMPLETE"}]}]},` +
		`"files":{},"blocks":{},"uploads":{}}`
	must(t, os.WriteFile(filepath.Join(dir, snapshotFileName), []byte(snap), 0o600))

	e := newWriteEnv(t, 3)
	must(t, e.srv.EnablePersistence(dir, 0))
	defer e.srv.Close()
	for path, want := range map[string]uint32{"/": 0o755, "/d": 0o750, "/d/f": 0o640} {
		if st, err := e.ns.Stat(path); err != nil || st.Mode != want {
			t.Errorf("%s = %+v, %v; quería modo %04o", path, st, err, want)
		}
	}
}
