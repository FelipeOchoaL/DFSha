package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// TLS del NameNode (US-87).
//
//   - La API REST (clientes) es HTTPS: el servidor presenta su certificado y el
//     cliente lo verifica con la CA del proyecto. No se pide certificado al cliente
//     (en la REST la identidad es el JWT).
//   - El gRPC (DataNodes y el otro NameNode) es mTLS: ambos lados presentan un
//     certificado firmado por la CA del proyecto y lo verifican. Sin certificado
//     válido no se llega a ningún servicio.
//
// Los certificados los genera deploy/certs/gen_certs.sh (US-86). Cada nodo usa el
// suyo para los dos papeles, así que debe incluir las extensiones de uso de clave
// de servidor y de cliente (serverAuth y clientAuth).

// minTLSVersion es la versión mínima aceptada.
const minTLSVersion = tls.VersionTLS12

// tlsSet agrupa las configuraciones TLS ya cargadas.
type tlsSet struct {
	// REST es la configuración del servidor HTTPS.
	REST *tls.Config
	// GRPCServer es la del servidor gRPC, con certificado de cliente obligatorio.
	GRPCServer *tls.Config
	// Peer es la de los clientes gRPC hacia el otro NameNode (mTLS).
	Peer *tls.Config
}

// loadTLS carga el certificado del nodo y la CA. Devuelve nil si TLS está
// desactivado. Si falta o es inválido algún archivo devuelve error: con TLS
// activado el NameNode no debe arrancar sin cifrar por equivocación.
func loadTLS(cfg config) (*tlsSet, error) {
	if !cfg.TLSEnabled {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("cargar el certificado del NameNode (TLS_CERT_FILE=%s, TLS_KEY_FILE=%s): %w",
			cfg.TLSCertFile, cfg.TLSKeyFile, err)
	}
	caPEM, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("leer la CA (TLS_CA_FILE=%s): %w", cfg.TLSCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("la CA de %s no contiene ningún certificado PEM válido", cfg.TLSCAFile)
	}
	return &tlsSet{
		REST: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   minTLSVersion,
		},
		GRPCServer: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   minTLSVersion,
		},
		Peer: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   minTLSVersion,
		},
	}, nil
}

// peerCredentials son las credenciales para que este NameNode se conecte por gRPC
// al otro (replicación del edit log y Ping, US-71 y US-72). Con TLS desactivado
// devuelve nil: el cliente debe usar un canal sin cifrar.
func (t *tlsSet) peerCredentials() credentials.TransportCredentials {
	if t == nil {
		return nil
	}
	return credentials.NewTLS(t.Peer)
}
