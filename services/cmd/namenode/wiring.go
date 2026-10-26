package main

import (
	"errors"
	"fmt"

	"dfsha/services/internal/namenode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	namenodev1 "dfsha/services/gen/namenode/v1"
)

// Este archivo es el único punto donde el arranque conoce las piezas del
// NameNode. Lo que falta del resto del equipo está detrás de interfaces pequeñas
// del paquete namenode, así que al integrarlo solo cambia aquí:
//
//   - Registro de DataNodes y colocación (US-22, US-24, US-50; David): reemplazar
//     noDataNodes por el registro real. Debe implementar namenode.BlockPlacer
//     (Place), namenode.NodeDirectory (LiveNode) y namenode.Reregistrar
//     (RequestReregistration).
//   - Cola de comandos de los DataNodes (US-22; David): asignar rest.Deletes
//     (namenode.BlockDeleter) para que /fs/rm ordene el borrado de bloques.
//   - Servicio gRPC NameNode (Register, Heartbeat, BlockReport, BlockReceived;
//     David): registrarlo en registerGRPC con namenodev1.RegisterNameNodeServer.
//     BlockReceived y BlockReport deben llamar a blocks.AddReplica; los heartbeats
//     deben rechazarse mientras rest.IsStandby().
//   - Autenticación JWT (US-80; David): asignar rest.Identify, que devuelve el
//     usuario y sus grupos del token (namenode.Principal). Sin esto la API no
//     autentica y todos son superusuario. El login debe llamar a
//     rest.EnsureHome(principal) tras validar la contraseña (US-82).
//
// Ya integrado: el servicio NameNodeHA (replicación del edit log y Ping entre los
// dos NameNode), con mTLS cuando TLS está activado.

// errNoRegistry se devuelve mientras el registro de DataNodes no esté integrado.
var errNoRegistry = errors.New("el registro de DataNodes no está integrado")

// noDataNodes es el reemplazo temporal del registro de DataNodes: no hay nodos
// vivos, así que el NameNode arranca y sirve el namespace, pero allocate
// responde 503 INSUFFICIENT_DATANODES y locations no devuelve ubicaciones.
type noDataNodes struct{}

func (noDataNodes) Place(string, int, int) ([]namenode.PipelineNode, error) {
	return nil, errNoRegistry
}

func (noDataNodes) LiveNode(string) (namenode.PipelineNode, bool) {
	return namenode.PipelineNode{}, false
}

// RequestReregistration lo llama el NameNode al ser promovido. Debe hacer que la
// respuesta del siguiente heartbeat de cada DataNode lleve registered=false; el
// registro de DataNodes real lo reemplaza. Hoy no hay nada que reiniciar.
func (noDataNodes) RequestReregistration() {}

// servers agrupa lo que arma newServers.
type servers struct {
	REST *namenode.RESTServer
	GRPC *grpc.Server
	// Peer es la conexión gRPC con el otro NameNode; nil si no hay otro configurado
	// (PEER_NAMENODE_ADDR o NAMENODE_ADDRS con ADVERTISE_ADDR).
	Peer *namenode.GRPCPeer
}

// Close libera el edit log y la conexión con el otro NameNode.
func (s *servers) Close() {
	_ = s.REST.Close()
	if s.Peer != nil {
		_ = s.Peer.Close()
	}
}

// newServers arma el servidor REST y el servidor gRPC del NameNode. Si hay un
// directorio de metadatos configurado recupera el estado guardado (snapshot +
// edit log); si está corrupto devuelve error y el NameNode no arranca.
func newServers(cfg config, tlsCfg *tlsSet) (*servers, error) {
	ns := namenode.NewNamespace()
	blocks := namenode.NewBlockMap()
	registry := noDataNodes{}

	rest := namenode.NewRESTServer(ns, blocks, registry)
	rest.Nodes = registry
	rest.NodeID = cfg.NodeID
	rest.Reregistrar = registry
	rest.ReplicationFactor = cfg.ReplicationFactor
	rest.CommitWait = cfg.CommitWait
	if cfg.MetaDir != "" {
		if err := rest.EnablePersistence(cfg.MetaDir, cfg.SnapshotEvery); err != nil {
			return nil, fmt.Errorf("recuperar los metadatos de %s: %w", cfg.MetaDir, err)
		}
	}

	srv := &servers{REST: rest}
	if cfg.PeerAddr != "" {
		peer, err := namenode.DialPeer(cfg.PeerAddr, tlsCfg.peerCredentials(), rest)
		if err != nil {
			_ = rest.Close()
			return nil, err
		}
		srv.Peer = peer
	}

	if cfg.Role == roleStandby {
		rest.SetStandby(true) // responde 503 a toda la API REST hasta que se promueva
	} else if err := attachReplication(rest, cfg, srv.Peer, !cfg.HARequireAck); err != nil {
		srv.Close()
		return nil, fmt.Errorf("activar la replicación al standby: %w", err)
	}

	var grpcOpts []grpc.ServerOption
	if tlsCfg != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg.GRPCServer))) // mTLS
	}
	srv.GRPC = grpc.NewServer(grpcOpts...)
	registerGRPC(srv.GRPC, rest, ns, blocks)
	return srv, nil
}

// registerGRPC registra los servicios gRPC del NameNode.
func registerGRPC(s *grpc.Server, rest *namenode.RESTServer, _ *namenode.Namespace, _ *namenode.BlockMap) {
	// Servicio de salud estándar de gRPC: permite comprobar que el puerto responde
	// (y que el mTLS funciona) sin depender de los servicios del NameNode.
	healthpb.RegisterHealthServer(s, health.NewServer())

	// Replicación del edit log y Ping entre los dos NameNode (US-71 y US-72).
	namenodev1.RegisterNameNodeHAServer(s, namenode.NewHAServer(rest))

	// TODO(David, US-22/23/58): namenodev1.RegisterNameNodeServer(s, <servicio>)
	// con Register, Heartbeat, BlockReport y BlockReceived. Mientras no esté
	// registrado, los DataNodes reciben Unimplemented.
}

// attachReplication conecta el edit log con el otro NameNode, si lo hay. Al
// arrancar se usa el modo configurado (HA_REQUIRE_ACK); tras un failover se usa
// bestEffort=true: el nuevo activo no puede exigir el ACK de un par que sigue
// caído, o dejaría de aceptar escrituras justo cuando más se necesita.
func attachReplication(rest *namenode.RESTServer, cfg config, peer *namenode.GRPCPeer, bestEffort bool) error {
	if peer == nil {
		return nil
	}
	_, err := rest.AttachStandby(peer, namenode.ReplicatorOptions{Timeout: cfg.HATimeout, BestEffort: bestEffort})
	return err
}
