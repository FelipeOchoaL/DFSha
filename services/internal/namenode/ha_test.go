package namenode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// loopback conecta el replicador del activo con un standby sin red.
type loopback struct {
	mu      sync.Mutex
	standby *RESTServer
	down    bool   // simula un standby inalcanzable
	before  func() // se ejecuta antes de entregar cada envío (para bloquearlo)
}

func (c *loopback) ReplicateEditLog(ctx context.Context, entries []EditEntry) (int64, error) {
	c.mu.Lock()
	down, hook, standby := c.down, c.before, c.standby
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if down {
		return 0, errors.New("standby inalcanzable")
	}
	return standby.ReplicateEditLog(append([]EditEntry(nil), entries...))
}

func (c *loopback) setDown(v bool) {
	c.mu.Lock()
	c.down = v
	c.mu.Unlock()
}

// haPair es un NameNode activo y un standby unidos por un loopback.
type haPair struct {
	t             *testing.T
	active, stand *writeEnv
	dirA, dirB    string
	lb            *loopback
	rep           *Replicator
}

func newHAPair(t *testing.T, opts ReplicatorOptions) *haPair {
	t.Helper()
	p := &haPair{t: t, dirA: t.TempDir(), dirB: t.TempDir()}
	p.active = newPersistEnv(t, p.dirA, 0)
	p.stand = newPersistEnv(t, p.dirB, 0)
	p.stand.srv.SetStandby(true)
	p.lb = &loopback{standby: p.stand.srv}
	rep, err := p.active.srv.AttachStandby(p.lb, opts)
	if err != nil {
		t.Fatal(err)
	}
	p.rep = rep
	return p
}

// restartStandby simula reiniciar el standby: recupera su estado de dirB.
func (p *haPair) restartStandby() {
	p.t.Helper()
	p.stand = restartEnv(p.t, p.stand, p.dirB, 0)
	p.stand.srv.SetStandby(true)
	p.lb.mu.Lock()
	p.lb.standby = p.stand.srv
	p.lb.mu.Unlock()
}

// wantInSync comprueba que activo y standby tienen el mismo estado y el mismo edit log.
func (p *haPair) wantInSync() {
	p.t.Helper()
	wantSameState(p.t, dumpState(p.active), dumpState(p.stand))
	a, _ := json.Marshal(logEntries(p.t, p.dirA))
	b, _ := json.Marshal(logEntries(p.t, p.dirB))
	if string(a) != string(b) {
		p.t.Errorf("los edit logs difieren\nactivo:  %s\nstandby: %s", a, b)
	}
}

func (p *haPair) mkdirCode(path string) int {
	return p.active.post("/fs/mkdir", fmt.Sprintf(`{"path":%q}`, path)).Code
}

func TestStandbyResponde503ATodaLaAPI(t *testing.T) {
	e := newPersistEnv(t, t.TempDir(), 0)
	e.srv.Nodes = newFakeDirectory("dn1")
	e.srv.SetStandby(true)
	if !e.srv.IsStandby() {
		t.Fatal("IsStandby debía ser true")
	}

	requests := []struct{ method, target, body string }{
		{"GET", "/fs/ls?path=/", ""},
		{"POST", "/fs/mkdir", `{"path":"/d"}`},
		{"DELETE", "/fs/rmdir?path=/d", ""},
		{"DELETE", "/fs/rm?path=/f", ""},
		{"POST", "/files/allocate", `{"path":"/f","blockIndex":0,"size":10}`},
		{"POST", "/files/commit", `{}`},
		{"POST", "/files/finalize", `{}`},
		{"GET", "/files/locations?path=/f", ""},
		{"POST", "/auth/login", `{"username":"ana","password":"x"}`},
		{"GET", "/ruta/desconocida", ""},
	}
	for _, r := range requests {
		w := do(t, e.h, r.method, r.target, r.body)
		wantError(t, w, 503, "NAMENODE_STANDBY")
		if w.Header().Get("Retry-After") != "1" {
			t.Errorf("%s %s: Retry-After = %q; quería 1", r.method, r.target, w.Header().Get("Retry-After"))
		}
	}
	// Ninguna petición cambió el estado.
	if entries, _ := e.ns.List("/"); len(entries) != 0 {
		t.Errorf("el standby no debía cambiar: %+v", entries)
	}
	// Al promoverlo sirve normalmente.
	e.srv.SetStandby(false)
	if w := do(t, e.h, "GET", "/fs/ls?path=/", ""); w.Code != 200 {
		t.Errorf("tras promoverlo: HTTP %d", w.Code)
	}
}

func TestReplicaCadaOperacionAlStandby(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	populate(t, p.active)
	p.wantInSync()
	if got := p.rep.Acked(); got != p.active.srv.log.LastTxID() || got == 0 {
		t.Errorf("Acked = %d; quería %d", got, p.active.srv.log.LastTxID())
	}
	// El standby no conoce réplicas (se reconstruyen con block reports tras promoverlo).
	if ids := p.stand.blocks.FileBlocks("/user/ana/a.csv"); len(ids) != 3 {
		t.Fatalf("el standby debía conocer los bloques del archivo: %v", ids)
	}
	if b, _ := p.stand.blocks.Block(p.stand.blocks.FileBlocks("/user/ana/a.csv")[0]); len(b.Replicas) != 0 || !b.Committed() {
		t.Errorf("bloque en el standby = %+v", b)
	}
}

func TestElActivoEsperaElACKAntesDeResponder(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{Timeout: 5 * time.Second})
	release := make(chan struct{})
	p.lb.mu.Lock()
	p.lb.before = func() { <-release }
	p.lb.mu.Unlock()

	done := make(chan int, 1)
	go func() { done <- p.mkdirCode("/d") }()

	select {
	case <-done:
		t.Fatal("el activo respondió antes de que el standby confirmara")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := p.stand.ns.Stat("/d"); err == nil {
		t.Error("el standby no debía tener /d todavía")
	}
	close(release)
	select {
	case code := <-done:
		if code != http.StatusCreated {
			t.Errorf("mkdir: HTTP %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mkdir no terminó")
	}
	if _, err := p.stand.ns.Stat("/d"); err != nil {
		t.Errorf("el standby debía tener /d al responder: %v", err)
	}
}

func TestModoEstrictoSinStandbyNoConfirmaYLuegoSeRecupera(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	p.mkdirCode("/antes")

	p.lb.setDown(true)
	if code := p.mkdirCode("/sin-standby"); code != 500 {
		t.Errorf("sin standby, el cliente no debe recibir confirmación: HTTP %d", code)
	}
	if _, err := p.stand.ns.Stat("/sin-standby"); err == nil {
		t.Error("el standby no podía tener /sin-standby")
	}

	// Al volver el standby, la siguiente operación le entrega también la pendiente.
	p.lb.setDown(false)
	if code := p.mkdirCode("/despues"); code != 201 {
		t.Fatalf("mkdir tras recuperar el standby: HTTP %d", code)
	}
	p.wantInSync()
	if _, err := p.stand.ns.Stat("/sin-standby"); err != nil {
		t.Errorf("el standby debía recibir la entrada pendiente: %v", err)
	}
}

func TestModoBestEffortNoDependeDelStandby(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{BestEffort: true})
	p.lb.setDown(true)
	for _, d := range []string{"/a", "/b", "/c"} {
		if code := p.mkdirCode(d); code != 201 {
			t.Fatalf("mkdir %s con el standby caído: HTTP %d; en best effort debe funcionar", d, code)
		}
	}
	if p.rep.Acked() != 0 {
		t.Errorf("Acked = %d; el standby no confirmó nada", p.rep.Acked())
	}
	p.lb.setDown(false)
	must(t, p.rep.Sync())
	p.wantInSync()
}

func TestStandbyAtrasadoPideLasFaltantes(t *testing.T) {
	// El activo ya tiene historia cuando se conecta un standby vacío.
	dirA, dirB := t.TempDir(), t.TempDir()
	active := newPersistEnv(t, dirA, 0)
	populate(t, active)
	stand := newPersistEnv(t, dirB, 0)
	stand.srv.SetStandby(true)

	rep, err := active.srv.AttachStandby(&loopback{standby: stand.srv}, ReplicatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	must(t, rep.Sync())
	wantSameState(t, dumpState(active), dumpState(stand))
	if rep.Acked() != active.srv.log.LastTxID() {
		t.Errorf("Acked = %d; quería %d", rep.Acked(), active.srv.log.LastTxID())
	}
	// Y sigue sincronizado con las operaciones siguientes.
	active.post("/fs/mkdir", `{"path":"/nuevo"}`)
	wantSameState(t, dumpState(active), dumpState(stand))
}

func TestStandbyQueSeReiniciaSePoneAlDia(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	for _, d := range []string{"/a", "/b", "/c"} {
		p.mkdirCode(d)
	}
	p.restartStandby() // recupera /a, /b y /c de su propio edit log
	for _, d := range []string{"/d", "/e"} {
		if code := p.mkdirCode(d); code != 201 {
			t.Fatalf("mkdir %s: HTTP %d", d, code)
		}
	}
	p.wantInSync()
}

func TestStandbyQueSeReiniciaMientrasElActivoTrabajaEnBestEffort(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{BestEffort: true})
	p.mkdirCode("/a")
	p.lb.setDown(true) // el standby está caído
	p.mkdirCode("/b")
	p.mkdirCode("/c")
	p.restartStandby()
	p.lb.setDown(false)
	must(t, p.rep.Sync())
	p.wantInSync()
}

func TestStandbyRechazaHuecosYReenvios(t *testing.T) {
	dirA := t.TempDir()
	active := newPersistEnv(t, dirA, 0)
	for _, d := range []string{"/a", "/b", "/c", "/d"} {
		active.post("/fs/mkdir", fmt.Sprintf(`{"path":%q}`, d))
	}
	entries := logEntries(t, dirA) // txid 1..4

	stand := newPersistEnv(t, t.TempDir(), 0)
	stand.srv.SetStandby(true)

	// Sin las dos primeras: hueco, no se aplica nada.
	last, err := stand.srv.ReplicateEditLog(entries[2:])
	if !errors.Is(err, ErrReplicationGap) || last != 0 {
		t.Errorf("hueco: last=%d err=%v; quería 0 y ErrReplicationGap", last, err)
	}
	if list, _ := stand.ns.List("/"); len(list) != 0 {
		t.Errorf("un hueco no debe aplicar nada: %+v", list)
	}
	// En orden: se aplican.
	if last, err = stand.srv.ReplicateEditLog(entries[:2]); err != nil || last != 2 {
		t.Errorf("last=%d err=%v; quería 2", last, err)
	}
	// Reenvío con repetidas: las ya aplicadas se ignoran y se aplica lo nuevo.
	if last, err = stand.srv.ReplicateEditLog(entries); err != nil || last != 4 {
		t.Errorf("reenvío: last=%d err=%v; quería 4", last, err)
	}
	// Lista vacía = consulta.
	if last, err = stand.srv.ReplicateEditLog(nil); err != nil || last != 4 {
		t.Errorf("consulta: last=%d err=%v; quería 4", last, err)
	}
	// Una entrada que sigue a un hueco dentro del mismo lote se rechaza desde ahí.
	bad := []EditEntry{entries[3], {TxID: 9, Op: OpMkdir, Time: entries[0].Time, Path: "/x"}}
	if last, err = stand.srv.ReplicateEditLog(bad); !errors.Is(err, ErrReplicationGap) || last != 4 {
		t.Errorf("hueco en el lote: last=%d err=%v", last, err)
	}
	wantSameState(t, dumpState(active), dumpState(stand))
}

func TestReplicateEditLogSoloEnStandby(t *testing.T) {
	active := newPersistEnv(t, t.TempDir(), 0)
	if _, err := active.srv.ReplicateEditLog(nil); !errors.Is(err, ErrNotStandby) {
		t.Errorf("en un activo: %v; quería ErrNotStandby", err)
	}
	// Un standby sin persistencia no puede guardar lo que recibe.
	noPersist := newWriteEnv(t, 3)
	noPersist.srv.SetStandby(true)
	if _, err := noPersist.srv.ReplicateEditLog(nil); err == nil {
		t.Error("un standby sin persistencia debía fallar")
	}
}

func TestAttachStandbyRequisitos(t *testing.T) {
	noPersist := newWriteEnv(t, 3)
	if _, err := noPersist.srv.AttachStandby(&loopback{}, ReplicatorOptions{}); err == nil {
		t.Error("sin persistencia debía fallar")
	}
	stand := newPersistEnv(t, t.TempDir(), 0)
	stand.srv.SetStandby(true)
	if _, err := stand.srv.AttachStandby(&loopback{}, ReplicatorOptions{}); err == nil {
		t.Error("un standby no debe replicar a otro")
	}
}

func TestStandbyDemasiadoAtrasadoRequiereResincronizacion(t *testing.T) {
	// Con BestEffort el activo sigue sirviendo aunque el standby se quede sin remedio.
	p := newHAPair(t, ReplicatorOptions{BestEffort: true, Retain: 3})
	p.lb.setDown(true)
	for i := 0; i < 10; i++ {
		if code := p.mkdirCode(fmt.Sprintf("/d%d", i)); code != 201 {
			t.Fatalf("mkdir con BestEffort: HTTP %d", code)
		}
	}
	p.lb.setDown(false)
	if err := p.rep.Sync(); !errors.Is(err, ErrStandbyNeedsResync) {
		t.Errorf("Sync: %v; quería ErrStandbyNeedsResync", err)
	}

	// En modo estricto el cliente no recibe confirmación de algo que el standby no puede tener.
	s := newHAPair(t, ReplicatorOptions{Retain: 3})
	s.lb.setDown(true)
	for i := 0; i < 6; i++ {
		s.mkdirCode(fmt.Sprintf("/d%d", i)) // 500: el standby no responde
	}
	s.lb.setDown(false)
	if code := s.mkdirCode("/otro"); code != 500 {
		t.Errorf("mkdir estricto con el standby sin remedio: HTTP %d; quería 500", code)
	}
	if err := s.rep.Sync(); !errors.Is(err, ErrStandbyNeedsResync) {
		t.Errorf("Sync estricto: %v; quería ErrStandbyNeedsResync", err)
	}
}
func TestSyncDetectaUnStandbyAdelantado(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	p.mkdirCode("/a")
	// Alguien escribió directamente en el standby: los logs divergieron.
	p.stand.srv.SetStandby(false)
	p.stand.post("/fs/mkdir", `{"path":"/intruso"}`)
	p.stand.post("/fs/mkdir", `{"path":"/intruso2"}`)
	p.stand.srv.SetStandby(true)

	if err := p.rep.Sync(); err == nil || !strings.Contains(err.Error(), "divergieron") {
		t.Errorf("Sync: %v; quería un error de divergencia", err)
	}
}

func TestRunSyncPoneAlDiaEnSegundoPlano(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{BestEffort: true})
	p.lb.setDown(true)
	p.mkdirCode("/a")
	p.mkdirCode("/b")
	p.lb.setDown(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.rep.RunSync(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for p.rep.Acked() != p.active.srv.log.LastTxID() {
		if time.Now().After(deadline) {
			t.Fatalf("el standby no se puso al día: Acked=%d", p.rep.Acked())
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.wantInSync()
}

// Si el activo cae, el standby promovido conserva todo y se puede seguir una subida a medias.
func TestPromoverAlStandbyConservaLosMetadatos(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	p.active.post("/fs/mkdir", `{"path":"/user/ana","parents":true}`)
	putFile(t, p.active, "/user/ana/hecho.csv", []int64{10, 10}, "dn1", "dn2", "dn3")
	a0 := p.active.mustAllocate("/user/ana/enproceso", 0, 10, "")
	p.active.mustCommit("/user/ana/enproceso", a0.UploadID, a0.BlockID, 10, sum('a'))
	pending := p.active.mustAllocate("/user/ana/enproceso", 1, 10, a0.UploadID)

	// El activo "cae": el standby se promueve.
	p.stand.srv.SetStandby(false)
	p.stand.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")

	// ls y get (locations) funcionan; las ubicaciones llegan con los block reports.
	w := do(t, p.stand.h, "GET", "/fs/ls?path=/user/ana", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hecho.csv") {
		t.Fatalf("ls en el standby promovido: HTTP %d, %s", w.Code, w.Body.String())
	}
	resp := p.stand.mustLocations("/user/ana/hecho.csv")
	if resp.Size != 20 || len(resp.Blocks) != 2 {
		t.Errorf("locations = %+v", resp)
	}
	// Un put nuevo funciona, y la subida que estaba a medias se puede terminar.
	putFile(t, p.stand, "/user/ana/nuevo.csv", []int64{5}, "dn1", "dn2", "dn3")
	p.stand.mustCommit("/user/ana/enproceso", a0.UploadID, pending.BlockID, 10, sum('b'))
	if w := p.stand.finalize("/user/ana/enproceso", a0.UploadID); w.Code != 201 {
		t.Errorf("finalize en el standby promovido: HTTP %d (%s)", w.Code, w.Body.String())
	}
	// Una vez promovido ya no acepta entradas replicadas.
	if _, err := p.stand.srv.ReplicateEditLog(nil); !errors.Is(err, ErrNotStandby) {
		t.Errorf("tras promoverlo: %v; quería ErrNotStandby", err)
	}
}

// Muchas operaciones simultáneas en el activo: el standby termina idéntico (go test -race).
func TestReplicacionConcurrente(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/d%d/f", i)
			if code := p.active.post("/fs/mkdir", fmt.Sprintf(`{"path":"/d%d"}`, i)).Code; code != 201 {
				t.Errorf("mkdir: HTTP %d", code)
				return
			}
			a := p.active.mustAllocate(path, 0, 10, "")
			p.active.mustCommit(path, a.UploadID, a.BlockID, 10, sum('a'))
			if i%2 == 0 {
				p.active.finalize(path, a.UploadID)
			}
			if i%4 == 0 {
				p.active.rm(path)
			}
		}(i)
	}
	wg.Wait()
	p.wantInSync()
}

// El standby con snapshots automáticos también recupera el mismo estado al reiniciar.
func TestStandbyConSnapshotsRecuperaElEstado(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	active := newPersistEnv(t, dirA, 0)
	stand := newPersistEnv(t, dirB, 4)
	stand.srv.SetStandby(true)
	if _, err := active.srv.AttachStandby(&loopback{standby: stand.srv}, ReplicatorOptions{}); err != nil {
		t.Fatal(err)
	}
	populate(t, active)
	before := dumpState(stand)
	wantSameState(t, dumpState(active), before)

	stand2 := restartEnv(t, stand, dirB, 4)
	wantSameState(t, before, dumpState(stand2))
}
