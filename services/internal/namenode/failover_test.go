package namenode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// probeFunc adapta una función a ActiveProbe.
type probeFunc func(ctx context.Context) (PingInfo, error)

func (f probeFunc) Ping(ctx context.Context) (PingInfo, error) { return f(ctx) }

// fakeRegistrar cuenta las peticiones de re-registro y, si onRequest no es nil,
// la ejecuta (para simular que los DataNodes envían sus block reports).
type fakeRegistrar struct {
	mu        sync.Mutex
	calls     int
	onRequest func()
}

func (r *fakeRegistrar) RequestReregistration() {
	r.mu.Lock()
	r.calls++
	f := r.onRequest
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

func (r *fakeRegistrar) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newStandbyEnv crea un standby con persistencia y el id dado.
func newStandbyEnv(t *testing.T, nodeID string) *writeEnv {
	t.Helper()
	e := newPersistEnv(t, t.TempDir(), 0)
	e.srv.NodeID = nodeID
	e.srv.SetStandby(true)
	return e
}

// eventually espera hasta 5 s a que cond sea cierta.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no se cumplió a tiempo: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPromueveTrasTresFallosSeguidos(t *testing.T) {
	e := newStandbyEnv(t, "nn-b")
	ok := func() (PingInfo, error) { return PingInfo{NodeID: "nn-a", Active: true}, nil }
	fail := func() (PingInfo, error) { return PingInfo{}, errors.New("sin respuesta") }
	// Los fallos no seguidos no cuentan: ok, 2 fallos, ok, 3 fallos.
	script := []func() (PingInfo, error){ok, fail, fail, ok, fail, fail, fail}

	var mu sync.Mutex
	calls := 0
	probe := probeFunc(func(context.Context) (PingInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		i := calls
		calls++
		if i < len(script) {
			return script[i]()
		}
		return fail()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.RunFailover(ctx, probe, FailoverOptions{Interval: 5 * time.Millisecond, Promote: PromoteOptions{ReportWait: time.Second}})

	eventually(t, "el standby se promueve", func() bool { return !e.srv.IsStandby() })
	time.Sleep(80 * time.Millisecond) // una vez activo, ya no hace ping
	mu.Lock()
	defer mu.Unlock()
	if calls != 7 {
		t.Errorf("pings = %d; quería 7 (se promueve en el tercer fallo seguido)", calls)
	}
}

func TestNoPromueveSiElActivoResponde(t *testing.T) {
	e := newStandbyEnv(t, "nn-b")
	var mu sync.Mutex
	calls := 0
	probe := probeFunc(func(context.Context) (PingInfo, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return PingInfo{NodeID: "nn-a", Active: true}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.RunFailover(ctx, probe, FailoverOptions{Interval: 5 * time.Millisecond})

	eventually(t, "se hacen varios pings", func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 20 })
	if !e.srv.IsStandby() {
		t.Error("no debía promoverse con el activo respondiendo")
	}
}

func TestUnPingLentoCuentaComoFallo(t *testing.T) {
	e := newStandbyEnv(t, "nn-b")
	probe := probeFunc(func(ctx context.Context) (PingInfo, error) {
		<-ctx.Done() // el activo no contesta: el ping vence por el timeout
		return PingInfo{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.srv.RunFailover(ctx, probe, FailoverOptions{Interval: 10 * time.Millisecond, MaxFailures: 2})
	eventually(t, "se promueve tras los timeouts", func() bool { return !e.srv.IsStandby() })
}

func TestDosStandbyDesempatanPorTxidYNodeID(t *testing.T) {
	cases := []struct {
		name        string
		myID        string
		myTx        int64
		peer        PingInfo
		wantPromote bool
	}{
		{"más historia gana", "b", 5, PingInfo{NodeID: "a", LastTxID: 3}, true},
		{"menos historia pierde", "a", 2, PingInfo{NodeID: "b", LastTxID: 3}, false},
		{"empate: el de menor id gana", "a", 4, PingInfo{NodeID: "b", LastTxID: 4}, true},
		{"empate: el de mayor id pierde", "b", 4, PingInfo{NodeID: "a", LastTxID: 4}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newStandbyEnv(t, c.myID)
			e.srv.lastTx.Store(c.myTx)
			peer := c.peer
			peer.Active = false // el otro también es standby
			probe := probeFunc(func(context.Context) (PingInfo, error) { return peer, nil })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go e.srv.RunFailover(ctx, probe, FailoverOptions{Interval: 5 * time.Millisecond, Promote: PromoteOptions{ReportWait: time.Second}})

			if c.wantPromote {
				eventually(t, "se promueve", func() bool { return !e.srv.IsStandby() })
				return
			}
			time.Sleep(150 * time.Millisecond)
			if !e.srv.IsStandby() {
				t.Error("no debía promoverse: el otro standby tiene prioridad")
			}
		})
	}
}

// promoted crea un NameNode con un archivo de 3 bloques cuyas réplicas "se
// perdieron" (como en un standby recién promovido) y lo promueve.
func promotedWithBlocks(t *testing.T, reg *fakeRegistrar, opts PromoteOptions) (*writeEnv, []string) {
	t.Helper()
	e := newPersistEnv(t, t.TempDir(), 0)
	e.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	ids := putFile(t, e, "/datos", []int64{10, 10, 10}, "dn1", "dn2", "dn3")
	for _, n := range []string{"dn1", "dn2", "dn3", "auto-1", "auto-2", "auto-3"} {
		e.blocks.RemoveNode(n) // un standby no conoce las réplicas
	}
	e.srv.SetStandby(true)
	if reg != nil {
		e.srv.Reregistrar = reg
	}
	e.srv.Promote(opts)
	return e, ids
}

func reportBlock(t *testing.T, e *writeEnv, id string, nodes ...string) {
	t.Helper()
	info, ok := e.blocks.Block(id)
	if !ok {
		t.Fatalf("bloque %s desconocido", id)
	}
	for _, n := range nodes {
		must(t, e.blocks.AddReplica(id, n, info.Checksum))
	}
}

func TestPromoverEsperaLosBlockReportsAntesDeAllocate(t *testing.T) {
	reg := &fakeRegistrar{}
	e, ids := promotedWithBlocks(t, reg, PromoteOptions{ReportWait: 5 * time.Second})

	if e.srv.IsStandby() || !e.srv.Recovering() {
		t.Fatalf("tras promover: standby=%v recovering=%v; quería false y true", e.srv.IsStandby(), e.srv.Recovering())
	}
	if reg.count() != 1 {
		t.Errorf("RequestReregistration se llamó %d veces; quería 1", reg.count())
	}

	// Mientras no se conocen las replicas: allocate y locations esperan; lo demas funciona.
	w := e.allocate("/nuevo", 0, 10, "")
	wantError(t, w, 503, "NAMENODE_RECOVERING")
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q; queria 1", w.Header().Get("Retry-After"))
	}
	wantError(t, e.locations("/datos"), 503, "NAMENODE_RECOVERING")
	if w := do(t, e.h, "GET", "/fs/ls?path=/", ""); w.Code != 200 {
		t.Errorf("ls durante la recuperación: HTTP %d", w.Code)
	}
	if w := e.post("/fs/mkdir", `{"path":"/ok"}`); w.Code != 201 {
		t.Errorf("mkdir durante la recuperación: HTTP %d", w.Code)
	}

	// Llegan los block reports de dos de los tres bloques: aún no basta.
	reportBlock(t, e, ids[0], "dn1")
	reportBlock(t, e, ids[1], "dn2")
	time.Sleep(150 * time.Millisecond)
	if !e.srv.Recovering() {
		t.Error("con 2 de 3 bloques cubiertos debía seguir en recuperación")
	}
	// El último completa la cobertura.
	reportBlock(t, e, ids[2], "dn3")
	eventually(t, "termina la recuperación", func() bool { return !e.srv.Recovering() })

	// Ya acepta allocate y devuelve ubicaciones.
	e.mustAllocate("/nuevo", 0, 10, "")
	resp := e.mustLocations("/datos")
	if len(resp.Blocks) != 3 || len(resp.Blocks[0].Locations) != 1 {
		t.Errorf("locations tras la recuperación = %+v", resp)
	}
}

func TestPromoverTerminaPorTiempoSiNoLlegaNingunReport(t *testing.T) {
	e, _ := promotedWithBlocks(t, nil, PromoteOptions{ReportWait: 200 * time.Millisecond})
	if !e.srv.Recovering() {
		t.Fatal("debía empezar en recuperación")
	}
	time.Sleep(60 * time.Millisecond)
	if !e.srv.Recovering() {
		t.Error("no debía terminar antes del tiempo máximo")
	}
	eventually(t, "termina por tiempo", func() bool { return !e.srv.Recovering() })
	// Los bloques sin réplicas conocidas quedan como under-replicated.
	if got := e.blocks.UnderReplicated(3); len(got) != 3 {
		t.Errorf("under-replicated = %v; quería los 3 bloques", got)
	}
}

func TestPromoverSinBloquesNoEspera(t *testing.T) {
	e := newStandbyEnv(t, "nn-b")
	e.srv.Promote(PromoteOptions{ReportWait: 10 * time.Second})
	eventually(t, "sin bloques no hay nada que esperar", func() bool { return !e.srv.Recovering() })
	if w := do(t, e.h, "GET", "/fs/ls?path=/", ""); w.Code != 200 {
		t.Errorf("ls: HTTP %d", w.Code)
	}
}

func TestPromoverEsIdempotente(t *testing.T) {
	reg := &fakeRegistrar{}
	e := newStandbyEnv(t, "nn-b")
	e.srv.Reregistrar = reg
	promoted := 0
	opts := PromoteOptions{OnPromoted: func() { promoted++ }}
	e.srv.Promote(opts)
	e.srv.Promote(opts)
	if reg.count() != 1 || promoted != 1 {
		t.Errorf("re-registros=%d OnPromoted=%d; quería 1 y 1", reg.count(), promoted)
	}
}

// failoverPair prepara un activo y un standby replicados y devuelve una sonda que
// consulta al activo mientras no se marque como caído.
func failoverPair(t *testing.T) (*haPair, probeFunc, func()) {
	t.Helper()
	p := newHAPair(t, ReplicatorOptions{})
	p.active.srv.NodeID = "nn-a"
	p.stand.srv.NodeID = "nn-b"
	var mu sync.Mutex
	dead := false
	probe := probeFunc(func(context.Context) (PingInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		if dead {
			return PingInfo{}, errors.New("conexión rechazada")
		}
		return p.active.srv.PingInfo(), nil
	})
	return p, probe, func() { mu.Lock(); dead = true; mu.Unlock() }
}

// Caída del activo de extremo a extremo: detección, promoción, block reports y servicio.
func TestFailoverCompleto(t *testing.T) {
	p, probe, killActive := failoverPair(t)
	p.active.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	populate(t, p.active)
	a0 := p.active.mustAllocate("/enproceso2", 0, 10, "")
	p.active.mustCommit("/enproceso2", a0.UploadID, a0.BlockID, 10, sum('a'))
	pending := p.active.mustAllocate("/enproceso2", 1, 10, a0.UploadID)
	p.wantInSync()

	// Cuando el nuevo activo pide el re-registro, los DataNodes envían sus block reports.
	p.stand.srv.Nodes = newFakeDirectory("dn1", "dn2", "dn3")
	reg := &fakeRegistrar{onRequest: func() {
		go func() {
			for _, path := range []string{"/user/ana/a.csv", "/enproceso", "/enproceso2"} {
				for _, id := range p.stand.blocks.FileBlocks(path) {
					if info, _ := p.stand.blocks.Block(id); info.Committed() {
						for _, n := range []string{"dn1", "dn2", "dn3"} {
							_ = p.stand.blocks.AddReplica(id, n, info.Checksum)
						}
					}
				}
			}
		}()
	}}
	p.stand.srv.Reregistrar = reg

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.stand.srv.RunFailover(ctx, probe, FailoverOptions{
		Interval: 5 * time.Millisecond,
		Promote:  PromoteOptions{ReportWait: 5 * time.Second},
	})

	time.Sleep(50 * time.Millisecond)
	if !p.stand.srv.IsStandby() {
		t.Fatal("con el activo vivo no debe promoverse")
	}
	killActive()
	eventually(t, "el standby se promueve", func() bool { return !p.stand.srv.IsStandby() })
	eventually(t, "termina la recuperación", func() bool { return !p.stand.srv.Recovering() })
	if reg.count() != 1 {
		t.Errorf("re-registros = %d; quería 1", reg.count())
	}

	// Todo lo que el cliente había hecho sigue ahí.
	if w := do(t, p.stand.h, "GET", "/fs/ls?path=/user/ana", ""); w.Code != 200 {
		t.Fatalf("ls: HTTP %d", w.Code)
	}
	resp := p.stand.mustLocations("/user/ana/a.csv")
	if resp.Size != 25 || len(resp.Blocks) != 3 || len(resp.Blocks[0].Locations) != 3 {
		t.Errorf("locations tras el failover = %+v", resp)
	}
	// Un put nuevo funciona y la subida que estaba a medias se puede terminar.
	putFile(t, p.stand, "/user/ana/despues.csv", []int64{5}, "dn1", "dn2", "dn3")
	p.stand.mustCommit("/enproceso2", a0.UploadID, pending.BlockID, 10, sum('b'))
	if w := p.stand.finalize("/enproceso2", a0.UploadID); w.Code != 201 {
		t.Errorf("finalize tras el failover: HTTP %d (%s)", w.Code, w.Body.String())
	}
}

// El antiguo activo vuelve: arranca como standby y el nuevo activo le entrega lo que se perdió.
func TestElAntiguoActivoVuelveComoStandby(t *testing.T) {
	p, _, killActive := failoverPair(t)
	populate(t, p.active)
	killActive()
	p.stand.srv.Promote(PromoteOptions{ReportWait: 200 * time.Millisecond})
	eventually(t, "recuperación", func() bool { return !p.stand.srv.Recovering() })
	if w := p.stand.post("/fs/mkdir", `{"path":"/mientras-estaba-caido"}`); w.Code != 201 {
		t.Fatalf("mkdir en el nuevo activo: HTTP %d", w.Code)
	}

	// El antiguo activo arranca (configurado como activo) y consulta al otro.
	toNew := probeFunc(func(context.Context) (PingInfo, error) { return p.stand.srv.PingInfo(), nil })
	if p.active.srv.IsStandby() {
		t.Fatal("el antiguo activo debía seguir siendo activo antes de arrancar")
	}
	if !p.active.srv.YieldToActivePeer(context.Background(), toNew, time.Second) {
		t.Fatal("YieldToActivePeer debía degradarlo a standby")
	}
	wantError(t, do(t, p.active.h, "GET", "/fs/ls?path=/", ""), 503, "NAMENODE_STANDBY")

	// El nuevo activo empieza a replicarle y lo pone al día.
	rep, err := p.stand.srv.AttachStandby(&loopback{standby: p.active.srv}, ReplicatorOptions{BestEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	must(t, rep.Sync())
	p.stand.post("/fs/mkdir", `{"path":"/ya-con-redundancia"}`)
	wantSameState(t, dumpState(p.stand), dumpState(p.active))
}

func TestYieldToActivePeerSoloSiElOtroEsActivo(t *testing.T) {
	cases := []struct {
		name  string
		probe probeFunc
		want  bool
	}{
		{"el otro es activo", func(context.Context) (PingInfo, error) { return PingInfo{NodeID: "x", Active: true}, nil }, true},
		{"el otro es standby", func(context.Context) (PingInfo, error) { return PingInfo{NodeID: "x"}, nil }, false},
		{"el otro no responde", func(context.Context) (PingInfo, error) { return PingInfo{}, errors.New("caído") }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newPersistEnv(t, t.TempDir(), 0)
			if got := e.srv.YieldToActivePeer(context.Background(), c.probe, time.Second); got != c.want {
				t.Errorf("YieldToActivePeer = %v; quería %v", got, c.want)
			}
			if e.srv.IsStandby() != c.want {
				t.Errorf("IsStandby = %v; quería %v", e.srv.IsStandby(), c.want)
			}
		})
	}
	// Un standby no tiene nada a lo que ceder.
	e := newStandbyEnv(t, "nn")
	if e.srv.YieldToActivePeer(context.Background(), probeFunc(func(context.Context) (PingInfo, error) {
		return PingInfo{Active: true}, nil
	}), time.Second) {
		t.Error("un standby no cede")
	}
}

// Dos activos a la vez (partición de red): el antiguo se degrada al primer intento de replicar.
func TestElAntiguoActivoSeAutoDegradaSiElOtroYaEsActivo(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	p.active.srv.NodeID = "nn-a"
	p.mkdirCode("/antes")

	p.stand.srv.Promote(PromoteOptions{ReportWait: 100 * time.Millisecond}) // el standby cree que el activo cayó

	// El antiguo activo, que sigue vivo, intenta escribir: el par le dice que ya es activo.
	if code := p.mkdirCode("/en-la-particion"); code != 500 {
		t.Errorf("mkdir en el activo antiguo: HTTP %d; no debe confirmarse", code)
	}
	if !p.active.srv.IsStandby() {
		t.Error("el activo antiguo debía degradarse a standby")
	}
	wantError(t, do(t, p.active.h, "GET", "/fs/ls?path=/", ""), 503, "NAMENODE_STANDBY")
	if _, err := p.stand.ns.Stat("/en-la-particion"); err == nil {
		t.Error("el nuevo activo no debe recibir lo que no se confirmó")
	}
}

// Un activo caído que tenía una entrada que el standby nunca recibió: al volver y
// replicarle el nuevo activo, la divergencia se detecta en lugar de ignorarse.
func TestSeDetectanLogsDivergentesTrasUnFailover(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{})
	p.active.srv.NodeID = "nn-a"
	p.mkdirCode("/comun")

	p.lb.setDown(true)
	if code := p.mkdirCode("/solo-en-el-viejo"); code != 500 { // txid 2 solo en el activo
		t.Fatalf("mkdir sin standby: HTTP %d", code)
	}
	// El standby se promueve y hace su propio txid 2.
	p.stand.srv.Promote(PromoteOptions{ReportWait: 50 * time.Millisecond})
	if code := p.stand.post("/fs/mkdir", `{"path":"/solo-en-el-nuevo"}`).Code; code != 201 {
		t.Fatalf("mkdir en el nuevo activo: HTTP %d", code)
	}
	// El antiguo activo vuelve como standby.
	p.active.srv.SetStandby(true)
	rep, err := p.stand.srv.AttachStandby(&loopback{standby: p.active.srv}, ReplicatorOptions{BestEffort: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Sync(); !errors.Is(err, ErrLogsDiverged) {
		t.Errorf("Sync: %v; quería ErrLogsDiverged", err)
	}
	if _, err := p.active.ns.Stat("/solo-en-el-nuevo"); err == nil {
		t.Error("no debía aplicarse nada sobre un log divergente")
	}
}

func TestPingInfoNoTomaLocks(t *testing.T) {
	e := newPersistEnv(t, t.TempDir(), 0)
	e.srv.NodeID = "nn-x"
	e.post("/fs/mkdir", `{"path":"/a"}`)

	e.srv.up.mu.Lock() // como si una escritura estuviera esperando al standby
	done := make(chan PingInfo, 1)
	go func() { done <- e.srv.PingInfo() }()
	select {
	case info := <-done:
		if info.NodeID != "nn-x" || !info.Active || info.LastTxID != 1 {
			t.Errorf("PingInfo = %+v", info)
		}
	case <-time.After(time.Second):
		e.srv.up.mu.Unlock()
		t.Fatal("PingInfo se quedó esperando un lock")
	}
	e.srv.up.mu.Unlock()

	e.srv.SetStandby(true)
	if e.srv.PingInfo().Active {
		t.Error("un standby no es activo")
	}
}

func TestRunStandbySyncUsaLaReplicacionVigente(t *testing.T) {
	p := newHAPair(t, ReplicatorOptions{BestEffort: true})
	p.lb.setDown(true)
	p.mkdirCode("/a")
	p.lb.setDown(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.active.srv.RunStandbySync(ctx, 10*time.Millisecond)
	eventually(t, "el standby se pone al día", func() bool { return p.rep.Acked() == p.active.srv.log.LastTxID() })
	p.wantInSync()
}
