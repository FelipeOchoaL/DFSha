package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
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
	want := config{NodeID: "namenode-1", Role: "active", RESTPort: 8080, GRPCPort: 50050, ReplicationFactor: 3}
	if cfg != want {
		t.Errorf("cfg = %+v; quería %+v", cfg, want)
	}
}

func TestLoadConfigDesdeEntorno(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"NODE_ID":            " namenode-active ",
		"NAMENODE_ROLE":      "ACTIVE",
		"REST_PORT":          "9090",
		"GRPC_PORT":          "9000",
		"REPLICATION_FACTOR": "2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{NodeID: "namenode-active", Role: "active", RESTPort: 9090, GRPCPort: 9000, ReplicationFactor: 2}
	if cfg != want {
		t.Errorf("cfg = %+v; quería %+v", cfg, want)
	}
}

func TestLoadConfigInvalida(t *testing.T) {
	casos := map[string]map[string]string{
		"puerto no numérico":       {"REST_PORT": "abc"},
		"puerto fuera de rango":    {"GRPC_PORT": "70000"},
		"puerto cero":              {"REST_PORT": "0"},
		"puertos iguales":          {"REST_PORT": "8080", "GRPC_PORT": "8080"},
		"réplicas no numéricas":    {"REPLICATION_FACTOR": "tres"},
		"réplicas cero":            {"REPLICATION_FACTOR": "0"},
		"rol desconocido":          {"NAMENODE_ROLE": "primary"},
		"standby aún no soportado": {"NAMENODE_ROLE": "standby"},
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
	handler, grpcServer := newServers(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- serve(ctx, cfg, restLn, grpcLn, handler, grpcServer) }()

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
	handler, grpcServer := newServers(cfg)

	result := make(chan error, 1)
	go func() { result <- serve(context.Background(), cfg, restLn, grpcLn, handler, grpcServer) }()

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
