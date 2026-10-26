package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Pruebas de extremo a extremo de la alta disponibilidad con DOS procesos del
// NameNode (run) reales: replicación del edit log por gRPC, caída del activo,
// promoción automática del standby y regreso del antiguo activo como standby.

// haNode es la configuración de uno de los dos NameNode.
type haNode struct {
	cfg    config
	client *http.Client
	base   string
}

func (n *haNode) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := n.client.Get(n.base + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (n *haNode) post(t *testing.T, path, body string) int {
	t.Helper()
	resp, err := n.client.Post(n.base+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// responde indica si el NameNode ya contesta (con cualquier código).
func (n *haNode) responde(t *testing.T) bool {
	code, _ := n.get(t, "/fs/ls")
	return code != 0
}

// newHANodes crea la configuración de un activo (nn1) y un standby (nn2) que se ven por gRPC.
func newHANodes(t *testing.T, useTLS bool) (active, standby *haNode) {
	t.Helper()
	mk := func(id, role string) *haNode {
		cfg := config{
			NodeID: id, Role: role, ReplicationFactor: 3,
			RESTPort: freePort(t), GRPCPort: freePort(t), MetaDir: t.TempDir(),
			SnapshotEvery: 1000, HARequireAck: true, HATimeout: 2 * time.Second,
			HASyncInterval: 100 * time.Millisecond, FailoverInterval: 50 * time.Millisecond,
			FailoverFailures: 3, RecoveryWait: time.Second, RecoveryThreshold: 0.999,
		}
		return &haNode{cfg: cfg, client: &http.Client{Timeout: 3 * time.Second}}
	}
	active, standby = mk("nn1", roleActive), mk("nn2", roleStandby)
	active.cfg.PeerAddr = fmt.Sprintf("127.0.0.1:%d", standby.cfg.GRPCPort)
	standby.cfg.PeerAddr = fmt.Sprintf("127.0.0.1:%d", active.cfg.GRPCPort)

	scheme := "http"
	if useTLS {
		scheme = "https"
		ca := newPKI(t, "ca-ha")
		for _, n := range []*haNode{active, standby} {
			crt, key := ca.issue(t, n.cfg.NodeID)
			n.cfg.TLSEnabled, n.cfg.TLSCertFile, n.cfg.TLSKeyFile, n.cfg.TLSCAFile = true, crt, key, ca.caFile
			n.client = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs: clientTLS(t, ca, "", "").RootCAs, MinVersion: tls.VersionTLS12,
			}}}
		}
	}
	for _, n := range []*haNode{active, standby} {
		n.base = fmt.Sprintf("%s://127.0.0.1:%d", scheme, n.cfg.RESTPort)
	}
	return active, standby
}

func testParDeNameNodes(t *testing.T, useTLS bool) {
	active, standby := newHANodes(t, useTLS)

	// El activo arranca primero (su par aún no existe: sigue como activo) y luego el standby.
	stopActive := startNameNodeWith(t, active.cfg, func() bool { return active.responde(t) })
	stopStandby := startNameNodeWith(t, standby.cfg, func() bool { return standby.responde(t) })
	time.Sleep(time.Second) // el canal gRPC del activo estaba en espera de reintento: le da tiempo a reconectar

	// El standby responde 503 a toda la API; el activo sirve.
	code, body := standby.get(t, "/fs/ls")
	if code != 503 || !strings.Contains(body, "NAMENODE_STANDBY") {
		t.Fatalf("standby: HTTP %d, %s", code, body)
	}
	if code := active.post(t, "/fs/mkdir", `{"path":"/user/ana/docs","parents":true}`); code != 201 {
		t.Fatalf("mkdir en el activo: HTTP %d", code)
	}

	// Cae el activo: el standby lo detecta por Ping, se promueve y conserva lo replicado.
	if err := stopActive(); err != nil {
		t.Fatalf("apagar el activo: %v", err)
	}
	waitFor(t, "el standby se promueve", func() bool {
		code, _ := standby.get(t, "/fs/ls")
		return code == 200
	})
	code, body = standby.get(t, "/fs/ls?path=/user/ana")
	if code != 200 || !strings.Contains(body, `"name":"docs"`) {
		t.Fatalf("ls en el nuevo activo: HTTP %d, %s", code, body)
	}
	if code := standby.post(t, "/fs/mkdir", `{"path":"/user/bob"}`); code != 201 {
		t.Fatalf("mkdir en el nuevo activo: HTTP %d", code)
	}

	// El antiguo activo vuelve (configurado como activo): ve que el otro ya lo es y arranca como standby.
	stopOld := startNameNodeWith(t, active.cfg, func() bool { return active.responde(t) })
	code, body = active.get(t, "/fs/ls")
	if code != 503 || !strings.Contains(body, "NAMENODE_STANDBY") {
		t.Fatalf("el antiguo activo debía arrancar como standby: HTTP %d, %s", code, body)
	}
	// Nunca hay dos activos: el nuevo sigue sirviendo.
	if code, _ := standby.get(t, "/fs/ls"); code != 200 {
		t.Fatalf("el nuevo activo dejó de servir: HTTP %d", code)
	}

	// El nuevo activo le entrega al antiguo lo que se perdió: si el nuevo cae, el antiguo
	// toma el control y tiene /user/bob (creado mientras estaba caído).
	if code := standby.post(t, "/fs/mkdir", `{"path":"/user/carla"}`); code != 201 {
		t.Fatalf("mkdir con redundancia: HTTP %d", code)
	}
	time.Sleep(400 * time.Millisecond) // deja que el replicador (Sync cada 100 ms) alcance al antiguo
	if err := stopStandby(); err != nil {
		t.Fatalf("apagar el nuevo activo: %v", err)
	}
	waitFor(t, "el antiguo activo vuelve a promoverse", func() bool {
		code, _ := active.get(t, "/fs/ls")
		return code == 200
	})
	code, body = active.get(t, "/fs/ls?path=/user")
	for _, want := range []string{`"name":"ana"`, `"name":"bob"`, `"name":"carla"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("ls /user tras el segundo failover: HTTP %d, falta %s en %s", code, want, body)
		}
	}
	if err := stopOld(); err != nil {
		t.Errorf("apagar: %v", err)
	}
}

func TestParDeNameNodesReplicaYSePromueve(t *testing.T) { testParDeNameNodes(t, false) }

func TestParDeNameNodesConTLSReplicaYSePromueve(t *testing.T) { testParDeNameNodes(t, true) }

// Con la replicación estricta el activo no confirma sin el standby; con best effort sí.
func TestActivoSinStandbySegunElModoDeReplicacion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		requireAck  bool
		wantCreated int
	}{
		{"estricto", true, 500},
		{"best effort", false, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active, _ := newHANodes(t, false) // el standby nunca arranca
			active.cfg.HARequireAck = tc.requireAck
			active.cfg.HATimeout = 300 * time.Millisecond
			stop := startNameNodeWith(t, active.cfg, func() bool { return active.responde(t) })
			if code := active.post(t, "/fs/mkdir", `{"path":"/d"}`); code != tc.wantCreated {
				t.Errorf("mkdir sin standby (%s): HTTP %d; quería %d", tc.name, code, tc.wantCreated)
			}
			if err := stop(); err != nil {
				t.Errorf("apagar: %v", err)
			}
		})
	}
}

// waitFor espera hasta 10 s a que cond sea cierta.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no se cumplió a tiempo: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
