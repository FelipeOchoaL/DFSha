package main

import (
	"errors"
	"fmt"

	"dfsha/services/internal/namenode"

	"google.golang.org/grpc"
)

// Este archivo es el único punto donde el arranque conoce las piezas del
// NameNode. Al integrar el trabajo del equipo solo cambia aquí:
//
//   - Registro de DataNodes y colocación (US-22, US-24; David): reemplazar
//     noDataNodes por el registro real. Debe implementar namenode.BlockPlacer
//     (Place) y namenode.NodeDirectory (LiveNode).
//   - Cola de comandos de los DataNodes (US-22; David): asignar rest.Deletes
//     (namenode.BlockDeleter) para que /fs/rm ordene el borrado de bloques.
//   - Servicio gRPC NameNode (US-22, US-23; David): registrarlo en registerGRPC.
//   - Servicio gRPC NameNodeHA (US-71; James): registrarlo en registerGRPC.
//   - Autenticación JWT (US-80; David): asignar rest.Authenticate.

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

// newServers arma el servidor REST y el servidor gRPC del NameNode. Si hay un
// directorio de metadatos configurado recupera el estado guardado (snapshot +
// edit log); si está corrupto devuelve error y el NameNode no arranca.
func newServers(cfg config) (*namenode.RESTServer, *grpc.Server, error) {
	ns := namenode.NewNamespace()
	blocks := namenode.NewBlockMap()
	registry := noDataNodes{}

	rest := namenode.NewRESTServer(ns, blocks, registry)
	rest.Nodes = registry
	rest.ReplicationFactor = cfg.ReplicationFactor
	rest.CommitWait = cfg.CommitWait
	if cfg.MetaDir != "" {
		if err := rest.EnablePersistence(cfg.MetaDir, cfg.SnapshotEvery); err != nil {
			return nil, nil, fmt.Errorf("recuperar los metadatos de %s: %w", cfg.MetaDir, err)
		}
	}

	rest.NodeID = cfg.NodeID
	rest.Reregistrar = registry
	if cfg.Role == roleStandby {
		rest.SetStandby(true) // responde 503 a toda la API REST hasta que se promueva
	} else if err := attachReplication(rest, cfg, !cfg.HARequireAck); err != nil {
		return nil, nil, fmt.Errorf("activar la replicacion al standby: %w", err)
	}

	grpcServer := grpc.NewServer()
	registerGRPC(grpcServer, ns, blocks)

	return rest, grpcServer, nil
}

// registerGRPC registra los servicios gRPC del NameNode. Por ahora no hay
// ninguno: el contrato (proto/namenode.proto, US-03) y su código generado
// (US-04) aún no están en main.
func registerGRPC(_ *grpc.Server, _ *namenode.Namespace, _ *namenode.BlockMap) {}

// newStandbyClient crea el cliente que envía el edit log al NameNode standby,
// o nil si no hay ninguno. Hoy devuelve nil porque falta el código generado del
// servicio NameNodeHA (proto/namenode.proto, US-03 y US-04). Al integrarlo debe
// devolver un cliente gRPC que implemente namenode.StandbyClient:
//
// ReplicateEditLog(ctx, entries) -> traduce []EditEntry a ReplicateEditLogRequest,
// devuelve el last_applied_txid de la respuesta y, si el standby rechazó las
// entradas por un hueco, ese txid junto con un error que envuelva
// namenode.ErrReplicationGap.
//
// En el standby, el servicio gRPC del mismo nombre llama a
// (*namenode.RESTServer).ReplicateEditLog con las entradas recibidas.
func newStandbyClient(cfg config) namenode.StandbyClient { return nil }

// attachReplication conecta el edit log con el otro NameNode, si hay cliente. Al
// arrancar se usa el modo configurado (HA_REQUIRE_ACK); tras un failover se usa
// bestEffort=true: el nuevo activo no puede exigir el ACK de un par que sigue
// caído, o dejaría de aceptar escrituras justo cuando más se necesita.
func attachReplication(rest *namenode.RESTServer, cfg config, bestEffort bool) error {
	client := newStandbyClient(cfg)
	if client == nil {
		return nil
	}
	_, err := rest.AttachStandby(client, namenode.ReplicatorOptions{Timeout: cfg.HATimeout, BestEffort: bestEffort})
	return err
}

// newPeerProbe crea el cliente que consulta al otro NameNode con
// NameNodeHA.Ping, o nil si no hay ninguno. Como newStandbyClient, hoy devuelve
// nil porque falta el código generado de NameNodeHA. Al integrarlo debe devolver
// un cliente que implemente namenode.ActiveProbe: llama al RPC Ping y traduce
// la respuesta (node_id, active, last_txid) a namenode.PingInfo. En el otro
// NameNode, el servicio gRPC responde con (*namenode.RESTServer).PingInfo().
//
// Mientras sea nil no hay detección de caída ni promoción automática.
func newPeerProbe(cfg config) namenode.ActiveProbe { return nil }

// RequestReregistration lo llama el NameNode al ser promovido. Debe hacer que la
// respuesta del siguiente heartbeat de cada DataNode lleve registered=false; el
// registro de DataNodes de David (US-22) lo reemplaza. Hoy no hay nada que
// reiniciar.
func (noDataNodes) RequestReregistration() {}
