package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Valores por defecto. Los nombres de variable y los defaults coinciden con
// services/internal/common/config.go (US-06, Carlos) para que, cuando esa
// configuración compartida esté en main, basta con reemplazar loadConfig por
// common.LoadConfig sin tocar el resto del arranque.
const (
	defaultNodeID            = "namenode-1"
	defaultRole              = roleActive
	defaultRESTPort          = 8080
	defaultGRPCPort          = 50050 // el default de NAMENODE_ADDRS en los DataNodes es localhost:50050
	defaultReplicationFactor = 3
	defaultCommitWaitMS      = 3000 // espera maxima de commit por las replicas (US-52)
)

// Roles del NameNode. Solo "active" está implementado; "standby" llega con la
// alta disponibilidad (US-71 y US-72).
const (
	roleActive  = "active"
	roleStandby = "standby"
)

// config es la configuración del NameNode, leída de variables de entorno.
type config struct {
	NodeID            string        // NODE_ID
	Role              string        // NAMENODE_ROLE: active | standby
	RESTPort          int           // REST_PORT
	GRPCPort          int           // GRPC_PORT
	ReplicationFactor int           // REPLICATION_FACTOR
	CommitWait        time.Duration // COMMIT_WAIT_MS: milisegundos que commit espera las replicas
}

// loadConfig lee la configuración usando getenv (os.Getenv en producción).
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		NodeID: envString(getenv, "NODE_ID", defaultNodeID),
		Role:   strings.ToLower(envString(getenv, "NAMENODE_ROLE", defaultRole)),
	}
	var err error
	if cfg.RESTPort, err = envPort(getenv, "REST_PORT", defaultRESTPort); err != nil {
		return config{}, err
	}
	if cfg.GRPCPort, err = envPort(getenv, "GRPC_PORT", defaultGRPCPort); err != nil {
		return config{}, err
	}
	if cfg.RESTPort == cfg.GRPCPort {
		return config{}, fmt.Errorf("REST_PORT y GRPC_PORT no pueden ser el mismo (%d)", cfg.RESTPort)
	}
	if cfg.ReplicationFactor, err = envInt(getenv, "REPLICATION_FACTOR", defaultReplicationFactor); err != nil {
		return config{}, err
	}
	if cfg.ReplicationFactor <= 0 {
		return config{}, fmt.Errorf("REPLICATION_FACTOR debe ser mayor que 0")
	}
	waitMS, err := envInt(getenv, "COMMIT_WAIT_MS", defaultCommitWaitMS)
	if err != nil {
		return config{}, err
	}
	if waitMS < 0 {
		return config{}, fmt.Errorf("COMMIT_WAIT_MS no puede ser negativo")
	}
	cfg.CommitWait = time.Duration(waitMS) * time.Millisecond

	switch cfg.Role {
	case roleActive:
	case roleStandby:
		return config{}, fmt.Errorf("NAMENODE_ROLE=standby aún no está soportado (llega con US-71/US-72)")
	default:
		return config{}, fmt.Errorf("NAMENODE_ROLE debe ser %q o %q, no %q", roleActive, roleStandby, cfg.Role)
	}
	return cfg, nil
}

func envString(getenv func(string) string, key, fallback string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(getenv func(string) string, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s debe ser un entero: %w", key, err)
	}
	return v, nil
}

func envPort(getenv func(string) string, key string, fallback int) (int, error) {
	v, err := envInt(getenv, key, fallback)
	if err != nil {
		return 0, err
	}
	if v <= 0 || v > 65535 {
		return 0, fmt.Errorf("%s debe estar entre 1 y 65535", key)
	}
	return v, nil
}
