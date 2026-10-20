// Punto de entrada del NameNode: carga config, arranca la API REST (clientes)
// y el servidor gRPC (DataNodes y HA), y decide si inicia como activo o standby.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dfsha/services/internal/namenode"

	"google.golang.org/grpc"
)

// shutdownTimeout es el tiempo máximo para terminar las peticiones en curso al apagar.
const shutdownTimeout = 5 * time.Second

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("configuración inválida: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

// run recupera los metadatos, abre los puertos y sirve hasta que ctx se cancele
// o un servidor falle. Al terminar guarda un snapshot final para que el próximo
// arranque no tenga que reproducir el edit log.
func run(ctx context.Context, cfg config) error {
	// Primero los metadatos: si no se pueden recuperar no se abre ningún puerto.
	rest, grpcServer, err := newServers(cfg)
	if err != nil {
		return err
	}
	defer rest.Close()

	restLn, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.RESTPort))
	if err != nil {
		return fmt.Errorf("escuchar REST en :%d: %w", cfg.RESTPort, err)
	}
	grpcLn, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		_ = restLn.Close()
		return fmt.Errorf("escuchar gRPC en :%d: %w", cfg.GRPCPort, err)
	}

	go checkpointLoop(ctx, rest, cfg.SnapshotInterval)
	serveErr := serve(ctx, cfg, restLn, grpcLn, rest.Handler(), grpcServer)

	if err := rest.Checkpoint(); err != nil {
		log.Printf("AVISO: no se pudo guardar el snapshot final: %v", err)
	}
	return serveErr
}

// checkpointLoop guarda un snapshot cada every mientras ctx esté activo
// (every <= 0 lo desactiva). Checkpoint no hace nada si no hubo cambios.
func checkpointLoop(ctx context.Context, rest *namenode.RESTServer, every time.Duration) {
	if every <= 0 {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rest.Checkpoint(); err != nil {
				log.Printf("AVISO: no se pudo guardar el snapshot periódico: %v", err)
			}
		}
	}
}

// serve atiende REST y gRPC al mismo tiempo. Devuelve nil si terminó porque ctx
// se canceló (cierre limpio) y un error si alguno de los dos servidores falló.
func serve(ctx context.Context, cfg config, restLn, grpcLn net.Listener, handler http.Handler, grpcServer *grpc.Server) error {
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("NameNode iniciando id=%s rol=%s rest=%s grpc=%s replicacion=%d",
		cfg.NodeID, cfg.Role, restLn.Addr(), grpcLn.Addr(), cfg.ReplicationFactor)
	log.Printf("ADVERTENCIA: la API REST aún no autentica (US-80 pendiente): no exponerla fuera de desarrollo")

	errs := make(chan error, 2)
	go func() {
		if err := httpServer.Serve(restLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("servidor REST: %w", err)
		}
	}()
	go func() {
		// Serve devuelve nil cuando se detiene con Stop o GracefulStop.
		if err := grpcServer.Serve(grpcLn); err != nil {
			errs <- fmt.Errorf("servidor gRPC: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Printf("NameNode %s apagándose", cfg.NodeID)
	case runErr = <-errs:
		log.Printf("NameNode %s se detiene por un error: %v", cfg.NodeID, runErr)
	}

	shutdown(httpServer, grpcServer)
	return runErr
}

// shutdown detiene ambos servidores esperando a las peticiones en curso hasta
// shutdownTimeout; pasado ese tiempo los corta.
func shutdown(httpServer *http.Server, grpcServer *grpc.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()
	if err := httpServer.Shutdown(ctx); err != nil {
		_ = httpServer.Close()
	}
	select {
	case <-done:
	case <-ctx.Done():
		grpcServer.Stop()
		<-done
	}
}
