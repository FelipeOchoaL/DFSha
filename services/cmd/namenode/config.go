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
	defaultNodeID              = "namenode-1"
	defaultRole                = roleActive
	defaultRESTPort            = 8080
	defaultGRPCPort            = 50050 // el default de NAMENODE_ADDRS en los DataNodes es localhost:50050
	defaultReplicationFactor   = 3
	defaultCommitWaitMS        = 3000          // espera maxima de commit por las replicas (US-52)
	defaultMetaDir             = "./data/meta" // edit log y snapshot (US-70); el directorio data/ no se sube a git
	defaultSnapshotEvery       = 1000          // operaciones entre snapshots; 0 = solo por tiempo
	defaultSnapshotIntervalSec = 300           // segundos entre snapshots periodicos; 0 = desactivado
	defaultHATimeoutMS         = 2000          // espera maxima del ACK del standby (US-71)
	defaultHASyncIntervalSec   = 5             // cada cuanto el activo pone al dia al standby; 0 = desactivado
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
	MetaDir           string        // META_DIR: donde se guardan el edit log y el snapshot; vacio = sin persistencia
	SnapshotEvery     int           // SNAPSHOT_EVERY: operaciones entre snapshots
	SnapshotInterval  time.Duration // SNAPSHOT_INTERVAL_SEC: tiempo entre snapshots periodicos
	HARequireAck      bool          // HA_REQUIRE_ACK: el cliente solo recibe confirmacion si el standby aplico la operacion
	HATimeout         time.Duration // HA_TIMEOUT_MS: espera maxima del ACK del standby
	HASyncInterval    time.Duration // HA_SYNC_INTERVAL_SEC: cada cuanto se pone al dia al standby
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

	if cfg.MetaDir, err = envDir(getenv, "META_DIR", defaultMetaDir); err != nil {
		return config{}, err
	}
	if cfg.SnapshotEvery, err = envNonNegative(getenv, "SNAPSHOT_EVERY", defaultSnapshotEvery); err != nil {
		return config{}, err
	}
	intervalSec, err := envNonNegative(getenv, "SNAPSHOT_INTERVAL_SEC", defaultSnapshotIntervalSec)
	if err != nil {
		return config{}, err
	}
	cfg.SnapshotInterval = time.Duration(intervalSec) * time.Second

	if cfg.HARequireAck, err = envBool(getenv, "HA_REQUIRE_ACK", true); err != nil {
		return config{}, err
	}
	haTimeoutMS, err := envNonNegative(getenv, "HA_TIMEOUT_MS", defaultHATimeoutMS)
	if err != nil {
		return config{}, err
	}
	cfg.HATimeout = time.Duration(haTimeoutMS) * time.Millisecond
	haSyncSec, err := envNonNegative(getenv, "HA_SYNC_INTERVAL_SEC", defaultHASyncIntervalSec)
	if err != nil {
		return config{}, err
	}
	cfg.HASyncInterval = time.Duration(haSyncSec) * time.Second

	switch cfg.Role {
	case roleActive:
	case roleStandby:
		if cfg.MetaDir == "" {
			return config{}, fmt.Errorf("NAMENODE_ROLE=standby requiere persistencia: META_DIR no puede estar desactivado")
		}
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

// envDir lee un directorio; "-" desactiva la persistencia (devuelve "").
func envDir(getenv func(string) string, key, fallback string) (string, error) {
	v := envString(getenv, key, fallback)
	if v == "-" {
		return "", nil
	}
	return v, nil
}

func envNonNegative(getenv func(string) string, key string, fallback int) (int, error) {
	v, err := envInt(getenv, key, fallback)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("%s no puede ser negativo", key)
	}
	return v, nil
}

func envBool(getenv func(string) string, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s debe ser true o false: %w", key, err)
	}
	return v, nil
}
