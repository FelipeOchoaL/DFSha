package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// pki es una CA de pruebas que emite certificados como lo hará gen_certs.sh (US-86).
type pki struct {
	dir    string
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string
}

func newPKI(t *testing.T, name string) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &pki{dir: dir, caCert: cert, caKey: key, caFile: filepath.Join(dir, "ca.crt")}
	writePEM(t, p.caFile, "CERTIFICATE", der)
	return p
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// issue emite un certificado válido para servidor y cliente, con SAN localhost y 127.0.0.1.
func (p *pki) issue(t *testing.T, cn string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(p.dir, cn+".crt")
	keyFile = filepath.Join(p.dir, cn+".key")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "PRIVATE KEY", keyDER)
	return certFile, keyFile
}

// clientTLS arma la configuración TLS de un cliente: confía en trust y, si cert no
// está vacío, presenta ese certificado.
func clientTLS(t *testing.T, trust *pki, certFile, keyFile string) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	data, err := os.ReadFile(trust.caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool.AppendCertsFromPEM(data)
	c := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		c.Certificates = []tls.Certificate{cert}
	}
	return c
}

// tlsConfig devuelve la configuración del NameNode con TLS activado y el
// certificado "namenode" emitido por ca.
func tlsConfig(t *testing.T, ca *pki) config {
	t.Helper()
	crt, key := ca.issue(t, "namenode")
	return config{
		NodeID: "nn-tls", Role: roleActive, ReplicationFactor: 3,
		RESTPort: freePort(t), GRPCPort: freePort(t),
		TLSEnabled: true, TLSCertFile: crt, TLSKeyFile: key, TLSCAFile: ca.caFile,
	}
}

// startNameNodeWith arranca run() y espera a que ready devuelva true.
func startNameNodeWith(t *testing.T, cfg config, ready func() bool) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, cfg) }()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
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
	return func() error {
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

// grpcHealth hace un Check al puerto gRPC con las credenciales dadas.
func grpcHealth(port int, creds credentials.TransportCredentials) error {
	conn, err := grpc.Dial(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("estado %v", resp.Status)
	}
	return nil
}

func TestLoadTLSDesactivado(t *testing.T) {
	set, err := loadTLS(config{TLSEnabled: false})
	if err != nil || set != nil {
		t.Errorf("con TLS desactivado: %v, %v; quería nil, nil", set, err)
	}
	if (*tlsSet)(nil).peerCredentials() != nil {
		t.Error("sin TLS no hay credenciales para el par")
	}
}

func TestLoadTLSConfiguraRESTyGRPC(t *testing.T) {
	cfg := tlsConfig(t, newPKI(t, "ca"))
	set, err := loadTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if set.REST.ClientAuth != tls.NoClientCert {
		t.Error("la REST no debe exigir certificado de cliente")
	}
	if set.GRPCServer.ClientAuth != tls.RequireAndVerifyClientCert || set.GRPCServer.ClientCAs == nil {
		t.Error("el gRPC debe exigir y verificar el certificado del cliente (mTLS)")
	}
	if set.Peer.RootCAs == nil || len(set.Peer.Certificates) != 1 {
		t.Error("el cliente hacia el otro NameNode debe verificar la CA y presentar su certificado")
	}
	for name, c := range map[string]*tls.Config{"REST": set.REST, "gRPC": set.GRPCServer, "par": set.Peer} {
		if c.MinVersion < tls.VersionTLS12 {
			t.Errorf("%s acepta versiones de TLS anteriores a 1.2", name)
		}
	}
	if set.peerCredentials() == nil {
		t.Error("peerCredentials no debe ser nil con TLS activado")
	}
}

func TestLoadTLSRechazaArchivosInvalidos(t *testing.T) {
	ca := newPKI(t, "ca")
	base := tlsConfig(t, ca)
	otherCrt, _ := ca.issue(t, "otro")

	cases := map[string]func(c *config){
		"certificado inexistente": func(c *config) { c.TLSCertFile = filepath.Join(ca.dir, "no-existe.crt") },
		"clave inexistente":       func(c *config) { c.TLSKeyFile = filepath.Join(ca.dir, "no-existe.key") },
		"clave de otro certificado": func(c *config) {
			c.TLSCertFile = otherCrt // la clave es la de "namenode"
		},
		"CA inexistente": func(c *config) { c.TLSCAFile = filepath.Join(ca.dir, "no-existe-ca.crt") },
		"CA sin certificados": func(c *config) {
			p := filepath.Join(ca.dir, "vacia.crt")
			_ = os.WriteFile(p, []byte("no soy un PEM"), 0o600)
			c.TLSCAFile = p
		},
	}
	for name, mutate := range cases {
		cfg := base
		mutate(&cfg)
		if _, err := loadTLS(cfg); err == nil {
			t.Errorf("%s: debía fallar", name)
		}
	}
}

func TestRESTEsHTTPS(t *testing.T) {
	ca := newPKI(t, "ca")
	cfg := tlsConfig(t, ca)
	base := fmt.Sprintf("https://127.0.0.1:%d", cfg.RESTPort)
	trusting := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS(t, ca, "", "")}}

	stop := startNameNodeWith(t, cfg, func() bool {
		resp, err := trusting.Get(base + "/fs/ls")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})

	// Con la CA del proyecto, sin certificado de cliente: funciona.
	resp, err := trusting.Get(base + "/fs/ls")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS con la CA del proyecto: %v", err)
	}
	resp.Body.Close()

	// Sin confiar en la CA (como un cliente cualquiera) el certificado se rechaza.
	if _, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + "/fs/ls"); err == nil {
		t.Error("un cliente que no confía en la CA debía rechazar el certificado")
	}
	// Confiando en otra CA tampoco.
	other := newPKI(t, "ca-ajena")
	foreign := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: clientTLS(t, other, "", "")}}
	if _, err := foreign.Get(base + "/fs/ls"); err == nil {
		t.Error("un certificado de otra CA debía rechazarse")
	}
	// En claro no se sirve nada.
	plain := &http.Client{Timeout: 3 * time.Second}
	if resp, err := plain.Get(fmt.Sprintf("http://127.0.0.1:%d/fs/ls", cfg.RESTPort)); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("la REST no debe responder en HTTP plano")
		}
	}
	if err := stop(); err != nil {
		t.Errorf("run devolvió %v", err)
	}
}

func TestGRPCExigeMTLS(t *testing.T) {
	ca := newPKI(t, "ca")
	cfg := tlsConfig(t, ca)
	stop := startNameNodeWith(t, cfg, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.GRPCPort), 200*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})

	clientCrt, clientKey := ca.issue(t, "datanode-1")
	other := newPKI(t, "ca-ajena")
	foreignCrt, foreignKey := other.issue(t, "intruso")

	// Cliente legítimo: certificado de la CA del proyecto y confía en ella.
	if err := grpcHealth(cfg.GRPCPort, credentials.NewTLS(clientTLS(t, ca, clientCrt, clientKey))); err != nil {
		t.Errorf("el cliente con certificado válido debe poder conectar: %v", err)
	}
	// El otro NameNode usa las credenciales de loadTLS (peerCredentials).
	set, err := loadTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := grpcHealth(cfg.GRPCPort, set.peerCredentials()); err != nil {
		t.Errorf("peerCredentials debe poder conectar: %v", err)
	}

	rejected := map[string]credentials.TransportCredentials{
		"sin certificado de cliente":        credentials.NewTLS(clientTLS(t, ca, "", "")),
		"certificado de cliente de otra CA": credentials.NewTLS(clientTLS(t, ca, foreignCrt, foreignKey)),
		"no confía en el servidor":          credentials.NewTLS(clientTLS(t, other, clientCrt, clientKey)),
		"sin cifrar (plaintext)":            insecure.NewCredentials(),
	}
	for name, creds := range rejected {
		if err := grpcHealth(cfg.GRPCPort, creds); err == nil {
			t.Errorf("%s: la conexión debía rechazarse", name)
		}
	}
	if err := stop(); err != nil {
		t.Errorf("run devolvió %v", err)
	}
}

// Con TLS_ENABLED=false (solo desarrollo) todo va en claro y funciona como antes.
func TestSinTLSTodoVaEnClaro(t *testing.T) {
	cfg := config{NodeID: "nn", Role: roleActive, ReplicationFactor: 3, RESTPort: freePort(t), GRPCPort: freePort(t)}
	base, stop := startNameNode(t, cfg)
	resp, err := http.Get(base + "/fs/ls")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP plano: %v", err)
	}
	resp.Body.Close()
	if err := grpcHealth(cfg.GRPCPort, insecure.NewCredentials()); err != nil {
		t.Errorf("gRPC sin cifrar: %v", err)
	}
	if err := stop(); err != nil {
		t.Errorf("run devolvió %v", err)
	}
}

func TestNoArrancaConTLSSiFaltanLosCertificados(t *testing.T) {
	cfg := config{
		NodeID: "nn", Role: roleActive, ReplicationFactor: 3,
		RESTPort: freePort(t), GRPCPort: freePort(t),
		TLSEnabled: true, TLSCertFile: "/no/existe.crt", TLSKeyFile: "/no/existe.key", TLSCAFile: "/no/existe-ca.crt",
	}
	err := run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "TLS_CERT_FILE") {
		t.Errorf("run devolvió %v; quería un error que mencione TLS_CERT_FILE", err)
	}
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.RESTPort), 300*time.Millisecond); err == nil {
		c.Close()
		t.Error("no debía abrirse ningún puerto sin certificados")
	}
}

func TestLoadConfigTLS(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"TLS_ENABLED":   "false",
		"TLS_CERT_FILE": "/certs/nn.crt",
		"TLS_KEY_FILE":  "/certs/nn.key",
		"TLS_CA_FILE":   "/certs/ca.pem",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSEnabled || cfg.TLSCertFile != "/certs/nn.crt" || cfg.TLSKeyFile != "/certs/nn.key" || cfg.TLSCAFile != "/certs/ca.pem" {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, err := loadConfig(env(map[string]string{"TLS_ENABLED": "quizas"})); err == nil {
		t.Error("TLS_ENABLED inválido debía fallar")
	}
}
