package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfigPorDefecto(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := config{NodeID: "namenode-1", Role: "active", RESTPort: 8080, GRPCPort: 50050, ReplicationFactor: 3,
		CommitWait: 3 * time.Second, MetaDir: "./data/meta", SnapshotEvery: 1000, SnapshotInterval: 300 * time.Second,
		HARequireAck: true, HATimeout: 2 * time.Second, HASyncInterval: 5 * time.Second,
		FailoverInterval: time.Second, FailoverFailures: 3, RecoveryWait: 30 * time.Second, RecoveryThreshold: 0.999,
		TLSEnabled: true, TLSCertFile: "./certs/namenode.crt", TLSKeyFile: "./certs/namenode.key", TLSCAFile: "./certs/ca.crt"}
	if cfg != want {
		t.Errorf("cfg = %+v; quería %+v", cfg, want)
	}
}

func TestLoadConfigDesdeEntorno(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"NODE_ID":               " namenode-active ",
		"NAMENODE_ROLE":         "ACTIVE",
		"REST_PORT":             "9090",
		"GRPC_PORT":             "9000",
		"REPLICATION_FACTOR":    "2",
		"COMMIT_WAIT_MS":        "250",
		"META_DIR":              "/var/dfsha/meta",
		"SNAPSHOT_EVERY":        "10",
		"SNAPSHOT_INTERVAL_SEC": "60",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{NodeID: "namenode-active", Role: "active", RESTPort: 9090, GRPCPort: 9000, ReplicationFactor: 2,
		CommitWait: 250 * time.Millisecond, MetaDir: "/var/dfsha/meta", SnapshotEvery: 10, SnapshotInterval: time.Minute,
		HARequireAck: true, HATimeout: 2 * time.Second, HASyncInterval: 5 * time.Second,
		FailoverInterval: time.Second, FailoverFailures: 3, RecoveryWait: 30 * time.Second, RecoveryThreshold: 0.999,
		TLSEnabled: true, TLSCertFile: "./certs/namenode.crt", TLSKeyFile: "./certs/namenode.key", TLSCAFile: "./certs/ca.crt"}
	if cfg != want {
		t.Errorf("cfg = %+v; quería %+v", cfg, want)
	}
}

func TestLoadConfigInvalida(t *testing.T) {
	casos := map[string]map[string]string{
		"puerto no numérico":             {"REST_PORT": "abc"},
		"puerto fuera de rango":          {"GRPC_PORT": "70000"},
		"puerto cero":                    {"REST_PORT": "0"},
		"puertos iguales":                {"REST_PORT": "8080", "GRPC_PORT": "8080"},
		"réplicas no numéricas":          {"REPLICATION_FACTOR": "tres"},
		"réplicas cero":                  {"REPLICATION_FACTOR": "0"},
		"snapshot negativo":              {"SNAPSHOT_EVERY": "-1"},
		"intervalo negativo":             {"SNAPSHOT_INTERVAL_SEC": "-5"},
		"intervalo no numerico":          {"SNAPSHOT_INTERVAL_SEC": "x"},
		"espera negativa":                {"COMMIT_WAIT_MS": "-1"},
		"espera no numerica":             {"COMMIT_WAIT_MS": "x"},
		"rol desconocido":                {"NAMENODE_ROLE": "primary"},
		"standby sin persistencia":       {"NAMENODE_ROLE": "standby", "META_DIR": "-"},
		"HA_REQUIRE_ACK invalido":        {"HA_REQUIRE_ACK": "quizas"},
		"HA_TIMEOUT_MS invalido":         {"HA_TIMEOUT_MS": "x"},
		"FAILOVER_INTERVAL_MS cero":      {"FAILOVER_INTERVAL_MS": "0"},
		"FAILOVER_FAILURES cero":         {"FAILOVER_FAILURES": "0"},
		"RECOVERY_WAIT_SEC negativo":     {"RECOVERY_WAIT_SEC": "-1"},
		"RECOVERY_THRESHOLD mayor que 1": {"RECOVERY_THRESHOLD": "1.5"},
		"RECOVERY_THRESHOLD cero":        {"RECOVERY_THRESHOLD": "0"},
		"RECOVERY_THRESHOLD no numerico": {"RECOVERY_THRESHOLD": "x"},
		"HA_SYNC_INTERVAL_SEC negativo":  {"HA_SYNC_INTERVAL_SEC": "-1"},
	}
	for nombre, vars := range casos {
		if _, err := loadConfig(env(vars)); err == nil {
			t.Errorf("%s: debía fallar", nombre)
		}
	}
}

// listener abre un puerto efímero en loopback.
func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeLevantaRESTyGRPCYCierraLimpio(t *testing.T) {
	cfg := config{NodeID: "nn-test", Role: roleActive, ReplicationFactor: 3}
	restLn, grpcLn := listener(t), listener(t)
	restAddr, grpcAddr := restLn.Addr().String(), grpcLn.Addr().String()
	rest, grpcServer, err := newServers(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- serve(ctx, cfg, restLn, grpcLn, rest.Handler(), grpcServer) }()

	// REST: mkdir y ls funcionan.
	resp, err := http.Post("http://"+restAddr+"/fs/mkdir", "application/json", strings.NewReader(`{"path":"/user/ana","parents":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("mkdir: HTTP %d; quería 201", resp.StatusCode)
	}
	resp, err = http.Get("http://" + restAddr + "/fs/ls?path=/user")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"ana"`) {
		t.Errorf("ls: HTTP %d, %s", resp.StatusCode, body)
	}

	// Sin registro de DataNodes integrado, allocate responde 503 con el código del contrato.
	resp, err = http.Post("http://"+restAddr+"/files/allocate", "application/json",
		strings.NewReader(`{"path":"/user/ana/f","blockIndex":0,"size":10}`))
	if err != nil {
		t.Fatal(err)
	}
	var e struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || e.Code != "INSUFFICIENT_DATANODES" {
		t.Errorf("allocate sin DataNodes: HTTP %d code %q", resp.StatusCode, e.Code)
	}

	// gRPC: el puerto acepta conexiones.
	conn, err := net.DialTimeout("tcp", grpcAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("el puerto gRPC no responde: %v", err)
	}
	conn.Close()

	// Cierre limpio al cancelar el contexto (equivale a Ctrl+C).
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("serve devolvió %v; quería nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve no terminó tras cancelar el contexto")
	}
	if _, err := http.Get("http://" + restAddr + "/fs/ls"); err == nil {
		t.Error("REST debía estar apagado")
	}
	if c, err := net.DialTimeout("tcp", grpcAddr, time.Second); err == nil {
		c.Close()
		t.Error("gRPC debía estar apagado")
	}
}

// Si un servidor no puede arrancar, serve devuelve el error y apaga el otro.
func TestServeFallaSiUnServidorFalla(t *testing.T) {
	cfg := config{NodeID: "nn-test", Role: roleActive, ReplicationFactor: 3}
	restLn, grpcLn := listener(t), listener(t)
	restAddr := restLn.Addr().String()
	grpcLn.Close() // gRPC no podrá aceptar conexiones
	rest, grpcServer, err := newServers(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() { result <- serve(context.Background(), cfg, restLn, grpcLn, rest.Handler(), grpcServer) }()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "gRPC") {
			t.Errorf("serve devolvió %v; quería un error de gRPC", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve debía terminar con error")
	}
	if _, err := http.Get("http://" + restAddr + "/fs/ls"); err == nil {
		t.Error("REST debía apagarse cuando falla gRPC")
	}
}

func TestRunFallaSiElPuertoEstaOcupado(t *testing.T) {
	// run escucha en todas las interfaces (:puerto): el puerto ocupado tambien debe estarlo.
	ocupado, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ocupado.Close()
	port := ocupado.Addr().(*net.TCPAddr).Port

	err = run(context.Background(), config{NodeID: "nn", Role: roleActive, RESTPort: port, GRPCPort: port + 1, ReplicationFactor: 3})
	if err == nil || !strings.Contains(err.Error(), "REST") {
		t.Errorf("run devolvió %v; quería un error de REST", err)
	}
}

// freePort devuelve un puerto TCP libre (hay una pequeña ventana hasta que se usa).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startNameNode arranca run() en segundo plano y espera a que atienda REST.
// Devuelve la URL base y una función que lo apaga y devuelve el resultado de run.
func startNameNode(t *testing.T, cfg config) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, cfg) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.RESTPort)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(base + "/fs/ls"); err == nil {
			resp.Body.Close()
			break
		}
		select {
		case err := <-result:
			cancel()
			t.Fatalf("run terminó antes de tiempo: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("el NameNode no arrancó")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, func() error {
		cancel()
		select {
		case err := <-result:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("run no terminó al cancelar")
			return nil
		}
	}
}

// Un reinicio completo del proceso conserva el namespace gracias al edit log y
// al snapshot final que se guarda al apagar.
func TestMetadatosSobrevivenAUnReinicio(t *testing.T) {
	dir := t.TempDir()
	cfg := config{
		NodeID: "nn", Role: roleActive, ReplicationFactor: 3,
		RESTPort: freePort(t), GRPCPort: freePort(t),
		MetaDir: dir, SnapshotEvery: 1000,
	}

	base, stop := startNameNode(t, cfg)
	for _, body := range []string{`{"path":"/user/ana/docs","parents":true}`, `{"path":"/tmp"}`} {
		resp, err := http.Post(base+"/fs/mkdir", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mkdir %s: HTTP %d", body, resp.StatusCode)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("run devolvió %v", err)
	}
	// Al apagar se guardó un snapshot y el log quedó vacío.
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); err != nil {
		t.Errorf("debía existir el snapshot final: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "edits.log")); err != nil || info.Size() != 0 {
		t.Errorf("el edit log debía quedar vacío: %v", err)
	}

	// Segundo arranque, mismo directorio y puertos nuevos.
	cfg.RESTPort, cfg.GRPCPort = freePort(t), freePort(t)
	base, stop = startNameNode(t, cfg)
	resp, err := http.Get(base + "/fs/ls?path=/user/ana")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"docs"`) {
		t.Errorf("tras reiniciar: HTTP %d, %s", resp.StatusCode, body)
	}
	if err := stop(); err != nil {
		t.Errorf("run devolvió %v", err)
	}
}

// Metadatos corruptos: el NameNode no arranca ni abre puertos.
func TestNoArrancaConMetadatosCorruptos(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("{no es json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		NodeID: "nn", Role: roleActive, ReplicationFactor: 3,
		RESTPort: freePort(t), GRPCPort: freePort(t), MetaDir: dir,
	}
	err := run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "recuperar los metadatos") {
		t.Errorf("run devolvió %v; quería un error de metadatos", err)
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.RESTPort), 300*time.Millisecond); err == nil {
		c.Close()
		t.Error("no debía abrirse el puerto REST")
	}
}

func TestLoadConfigStandbyYOpcionesDeHA(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"NAMENODE_ROLE":        "standby",
		"HA_REQUIRE_ACK":       "false",
		"HA_TIMEOUT_MS":        "500",
		"HA_SYNC_INTERVAL_SEC": "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Role != roleStandby || cfg.HARequireAck || cfg.HATimeout != 500*time.Millisecond || cfg.HASyncInterval != 0 {
		t.Errorf("cfg = %+v", cfg)
	}
}

// Un NameNode standby arranca, pero responde 503 a toda la API REST.
func TestStandbyArrancaYResponde503(t *testing.T) {
	cfg := config{
		NodeID: "nn-standby", Role: roleStandby, ReplicationFactor: 3,
		RESTPort: freePort(t), GRPCPort: freePort(t), MetaDir: t.TempDir(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, cfg) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.RESTPort)

	var resp *http.Response
	var err error
	for i := 0; i < 200; i++ {
		if resp, err = http.Get(base + "/fs/ls"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("el standby no arrancó: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "NAMENODE_STANDBY") ||
		resp.Header.Get("Retry-After") != "1" {
		t.Errorf("standby: HTTP %d, Retry-After %q, %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("run devolvió %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run no terminó")
	}
}

func TestLoadConfigFailover(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"FAILOVER_INTERVAL_MS": "250",
		"FAILOVER_FAILURES":    "5",
		"RECOVERY_WAIT_SEC":    "0",
		"RECOVERY_THRESHOLD":   "0.95",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FailoverInterval != 250*time.Millisecond || cfg.FailoverFailures != 5 ||
		cfg.RecoveryWait != 0 || cfg.RecoveryThreshold != 0.95 {
		t.Errorf("cfg = %+v", cfg)
	}
}
