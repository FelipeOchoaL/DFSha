package namenode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	namenodev1 "dfsha/services/gen/namenode/v1"

	"google.golang.org/grpc"
)

// ms devuelve una hora con precisión de milisegundos, la que viaja en el proto.
func ms(sec int64, millis int64) time.Time { return time.UnixMilli(sec*1000 + millis).UTC() }

func TestConversionDeEntradasConElProto(t *testing.T) {
	entries := []EditEntry{
		{TxID: 1, Op: OpMkdir, Time: ms(1700000000, 123), Path: "/user/ana", Owner: "ana", Group: "estudiantes", Mode: 0o750, Parents: true},
		{TxID: 2, Op: OpRmdir, Time: ms(1700000001, 5), Path: "/user/ana/x"},
		{TxID: 3, Op: OpDelete, Time: ms(1700000002, 0), Path: "/user/ana/f"},
		{TxID: 4, Op: OpAllocate, Time: ms(1700000003, 999), Path: "/f", Owner: "ana", Group: "g", Mode: 0o640,
			UploadID: "up-1", NewUpload: true, BlockID: "blk_up-1_0", BlockIndex: 0, Size: 8 << 20},
		{TxID: 5, Op: OpAllocate, Time: ms(1700000004, 1), Path: "/f", UploadID: "up-1", BlockID: "blk_up-1_1_r2", BlockIndex: 1, Size: 10},
		{TxID: 6, Op: OpAllocate, Time: ms(1700000005, 2), Path: "/vacio", Owner: "ana", UploadID: "up-2", NewUpload: true}, // archivo vacío: sin bloque
		{TxID: 7, Op: OpCommit, Time: ms(1700000006, 3), Path: "/f", UploadID: "up-1", BlockID: "blk_up-1_0", Size: 8 << 20, Checksum: strings.Repeat("a", 64)},
		{TxID: 8, Op: OpFinalize, Time: ms(1700000007, 4), Path: "/f", UploadID: "up-1", Size: 8<<20 + 10},
		{TxID: 9, Op: OpChmod, Time: ms(1700000008, 5), Path: "/f", Mode: 0o600},
		{TxID: 10, Op: OpChmod, Time: ms(1700000009, 6), Path: "/f", Mode: 0}, // 000 es un modo válido
	}
	for _, want := range entries {
		p, err := entryToProto(want)
		if err != nil {
			t.Fatalf("entryToProto(%s): %v", want.Op, err)
		}
		got, err := entryFromProto(p)
		if err != nil {
			t.Fatalf("entryFromProto(%s): %v", want.Op, err)
		}
		if got != want {
			t.Errorf("ida y vuelta de %s:\n got %+v\nwant %+v", want.Op, got, want)
		}
	}
}

func TestConversionRechazaOperacionesInvalidas(t *testing.T) {
	if _, err := entryToProto(EditEntry{TxID: 1, Op: "teletransportar"}); err == nil {
		t.Error("una operación desconocida debía fallar")
	}
	if _, err := entryFromProto(&namenodev1.EditLogEntry{Txid: 1}); err == nil {
		t.Error("una entrada sin operación debía fallar")
	}
}

// grpcNode es un NameNode de pruebas con su servicio NameNodeHA escuchando en loopback.
type grpcNode struct {
	env    *writeEnv
	server *grpc.Server
	addr   string
}

func newGRPCNode(t *testing.T, id string) *grpcNode {
	t.Helper()
	e := newPersistEnv(t, t.TempDir(), 0)
	e.srv.NodeID = id
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	namenodev1.RegisterNameNodeHAServer(s, NewHAServer(e.srv))
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(s.Stop)
	return &grpcNode{env: e, server: s, addr: ln.Addr().String()}
}

// dial devuelve la conexión de n hacia other.
func (n *grpcNode) dial(t *testing.T, other *grpcNode) *GRPCPeer {
	t.Helper()
	p, err := DialPeer(other.addr, nil, n.env.srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestReplicacionPorGRPC(t *testing.T) {
	active, standby := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	standby.env.srv.SetStandby(true)
	if _, err := active.env.srv.AttachStandby(active.dial(t, standby), ReplicatorOptions{}); err != nil {
		t.Fatal(err)
	}

	populate(t, active.env) // directorios, archivos, rm, subida a medias con reintentos...

	wantSameState(t, dumpState(active.env), dumpState(standby.env))
	a, b := logEntries(t, active.env.srv.log.dir), logEntries(t, standby.env.srv.log.dir)
	if len(a) == 0 || len(a) != len(b) {
		t.Errorf("los edit logs deben coincidir: %d y %d entradas", len(a), len(b))
	}
	for i := range a {
		if !sameEntry(a[i], b[i]) {
			t.Errorf("entrada %d distinta:\n activo  %+v\n standby %+v", i, a[i], b[i])
		}
	}
}

func TestPermisosYUsuariosSeReplicanPorGRPC(t *testing.T) {
	active, standby := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	standby.env.srv.SetStandby(true)
	if _, err := active.env.srv.AttachStandby(active.dial(t, standby), ReplicatorOptions{}); err != nil {
		t.Fatal(err)
	}
	p := permify(active.env)
	p.setup(t)
	p.chmod("/user/ana/datos", "0604")
	wantSameState(t, dumpState(active.env), dumpState(standby.env))
	f, _ := standby.env.ns.Stat("/user/ana/datos")
	if f.Owner != "ana" || f.Group != "estudiantes" || f.Mode != 0o604 {
		t.Errorf("en el standby: %+v", f)
	}
}

func TestPingPorGRPC(t *testing.T) {
	a, b := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	b.env.srv.SetStandby(true)
	a.env.post("/fs/mkdir", `{"path":"/x"}`)

	info, err := a.dial(t, b).Ping(context.Background())
	if err != nil || info.NodeID != "nn-b" || info.Active || info.LastTxID != 0 {
		t.Errorf("ping a un standby: %+v, %v", info, err)
	}
	info, err = b.dial(t, a).Ping(context.Background())
	if err != nil || info.NodeID != "nn-a" || !info.Active || info.LastTxID != 1 {
		t.Errorf("ping a un activo: %+v, %v", info, err)
	}
}

func TestEstadosDeReplicacionPorGRPC(t *testing.T) {
	active, standby := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	for _, d := range []string{"/a", "/b", "/c"} {
		active.env.post("/fs/mkdir", fmt.Sprintf(`{"path":%q}`, d))
	}
	entries := logEntries(t, active.env.srv.log.dir) // txid 1..3
	peer := active.dial(t, standby)
	ctx := context.Background()

	// Un NameNode que no es standby responde ErrNotStandby.
	if _, err := peer.ReplicateEditLog(ctx, entries); !errors.Is(err, ErrNotStandby) {
		t.Errorf("a un activo: %v; quería ErrNotStandby", err)
	}
	standby.env.srv.SetStandby(true)

	// Hueco: se devuelve el último txid aplicado para que el activo reenvíe desde ahí.
	last, err := peer.ReplicateEditLog(ctx, entries[1:])
	if !errors.Is(err, ErrReplicationGap) || last != 0 {
		t.Errorf("hueco: last=%d err=%v", last, err)
	}
	// En orden y repetidas: se aplican y se ignoran las ya aplicadas.
	if last, err = peer.ReplicateEditLog(ctx, entries[:2]); err != nil || last != 2 {
		t.Errorf("en orden: last=%d err=%v", last, err)
	}
	if last, err = peer.ReplicateEditLog(ctx, entries); err != nil || last != 3 {
		t.Errorf("con repetidas: last=%d err=%v", last, err)
	}
	// Consulta sin entradas.
	if last, err = peer.ReplicateEditLog(ctx, nil); err != nil || last != 3 {
		t.Errorf("consulta: last=%d err=%v", last, err)
	}
	// Una entrada distinta con un txid ya aplicado: divergencia.
	other := entries[2]
	other.Path = "/otra"
	if _, err = peer.ReplicateEditLog(ctx, []EditEntry{other}); !errors.Is(err, ErrLogsDiverged) {
		t.Errorf("divergencia: %v; quería ErrLogsDiverged", err)
	}
	// Entradas inválidas.
	if _, err = peer.ReplicateEditLog(ctx, []EditEntry{{TxID: 4, Op: "x"}}); err == nil {
		t.Error("una operación inválida debía fallar")
	}
}

// Poner al día a un standby muy atrasado exige varios mensajes (lotes).
func TestPonerAlDiaPorLotes(t *testing.T) {
	active, standby := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	standby.env.srv.SetStandby(true)
	const n = replicateBatch*2 + 123
	for i := 0; i < n; i++ {
		if w := active.env.post("/fs/mkdir", fmt.Sprintf(`{"path":"/d%d"}`, i)); w.Code != 201 {
			t.Fatalf("mkdir %d: HTTP %d", i, w.Code)
		}
	}
	rep, err := active.env.srv.AttachStandby(active.dial(t, standby), ReplicatorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	must(t, rep.Sync())
	if rep.Acked() != n {
		t.Errorf("Acked = %d; quería %d", rep.Acked(), n)
	}
	wantSameState(t, dumpState(active.env), dumpState(standby.env))
}

func TestParNoDisponibleEsUnError(t *testing.T) {
	active := newGRPCNode(t, "nn-a")
	peer, err := DialPeer("127.0.0.1:1", nil, active.env.srv) // nadie escucha
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := peer.Ping(ctx); err == nil {
		t.Error("Ping a un par caído debía fallar")
	}
	if _, err := peer.ReplicateEditLog(ctx, nil); err == nil {
		t.Error("ReplicateEditLog a un par caído debía fallar")
	}
	if _, err := DialPeer("", nil, active.env.srv); err == nil {
		t.Error("sin dirección debía fallar")
	}
}

// Caída del activo de extremo a extremo sobre gRPC: el standby lo detecta con Ping y se promueve.
func TestFailoverPorGRPC(t *testing.T) {
	active, standby := newGRPCNode(t, "nn-a"), newGRPCNode(t, "nn-b")
	standby.env.srv.SetStandby(true)
	if _, err := active.env.srv.AttachStandby(active.dial(t, standby), ReplicatorOptions{}); err != nil {
		t.Fatal(err)
	}
	active.env.post("/fs/mkdir", `{"path":"/user/ana","parents":true}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go standby.env.srv.RunFailover(ctx, standby.dial(t, active), FailoverOptions{
		Interval: 30 * time.Millisecond, Promote: PromoteOptions{ReportWait: time.Second},
	})

	time.Sleep(150 * time.Millisecond)
	if !standby.env.srv.IsStandby() {
		t.Fatal("con el activo vivo no debe promoverse")
	}
	active.server.Stop() // el activo deja de responder
	eventually(t, "el standby se promueve", func() bool { return !standby.env.srv.IsStandby() })
	eventually(t, "termina la recuperación", func() bool { return !standby.env.srv.Recovering() })

	w := do(t, standby.env.h, "GET", "/fs/ls?path=/user", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"ana"`) {
		t.Errorf("ls en el nuevo activo: HTTP %d, %s", w.Code, w.Body.String())
	}
}
