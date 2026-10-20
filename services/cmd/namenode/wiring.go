package main

import (
	"errors"
	"net/http"

	"dfsha/services/internal/namenode"

	"google.golang.org/grpc"
)

// Este archivo es el único punto donde el arranque conoce las piezas del
// NameNode. Al integrar el trabajo del equipo solo cambia aquí:
//
//   - Registro de DataNodes y colocación (US-22, US-24; David): reemplazar
//     noDataNodes por el registro real. Debe implementar namenode.BlockPlacer
//     (Place) y namenode.NodeDirectory (LiveNode).
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

// newServers arma el handler REST y el servidor gRPC del NameNode.
func newServers(cfg config) (http.Handler, *grpc.Server) {
	ns := namenode.NewNamespace()
	blocks := namenode.NewBlockMap()
	registry := noDataNodes{}

	rest := namenode.NewRESTServer(ns, blocks, registry)
	rest.Nodes = registry
	rest.ReplicationFactor = cfg.ReplicationFactor

	grpcServer := grpc.NewServer()
	registerGRPC(grpcServer, ns, blocks)

	return rest.Handler(), grpcServer
}

// registerGRPC registra los servicios gRPC del NameNode. Por ahora no hay
// ninguno: el contrato (proto/namenode.proto, US-03) y su código generado
// (US-04) aún no están en main.
func registerGRPC(_ *grpc.Server, _ *namenode.Namespace, _ *namenode.BlockMap) {}
