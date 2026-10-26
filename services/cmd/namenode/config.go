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
	defaultGRPCPort            = 9000 // igual que NAMENODE_ADDRS por defecto en los DataNodes, el Dockerfile y la guia AWS
	defaultReplicationFactor   = 3
	defaultCommitWaitMS        = 3000              // espera maxima de commit por las replicas (US-52)
	defaultHeartbeatSec        = 3                 // HEARTBEAT_INTERVAL_SEC: lo que se pide a los DataNodes (US-22)
	defaultDeadAfterSec        = 10                // DEAD_AFTER_SEC: sin heartbeat este tiempo (~3 perdidos) el DataNode es DEAD
	defaultDataDir             = "./data"          // DATA_DIR: en los contenedores es el volumen /data
	defaultSnapshotEvery       = 1000              // operaciones entre snapshots; 0 = solo por tiempo
	defaultSnapshotIntervalSec = 300               // segundos entre snapshots periodicos; 0 = desactivado
	defaultHATimeoutMS         = 2000              // espera maxima del ACK del standby (US-71)
	defaultHASyncIntervalSec   = 5                 // cada cuanto el activo pone al dia al standby; 0 = desactivado
	defaultFailoverIntervalMS  = 1000              // entre pings al otro NameNode (US-72)
	defaultFailoverFailures    = 3                 // fallos seguidos para promover
	defaultRecoveryWaitSec     = 30                // espera maxima de block reports tras promover
	defaultRecoveryThreshold   = 0.999             // fraccion de bloques con replica conocida para terminar
	defaultTLSCertFile         = "/certs/node.crt" // certificado de este nodo (gen_certs.sh deja <nodo>/node.crt, que se monta en /certs)
	defaultTLSKeyFile          = "/certs/node.key" // su clave privada
	defaultTLSCAFile           = "/certs/ca.crt"   // CA del proyecto
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
	HeartbeatInterval time.Duration // HEARTBEAT_INTERVAL_SEC: intervalo que se pide a los DataNodes
	DeadAfter         time.Duration // DEAD_AFTER_SEC: sin heartbeat este tiempo el DataNode se marca DEAD
	AdvertiseAddr     string        // ADVERTISE_ADDR: direccion gRPC con la que otros nodos alcanzan a este NameNode
	PeerAddr          string        // PEER_NAMENODE_ADDR (o el otro de NAMENODE_ADDRS): direccion gRPC del otro NameNode
	MetaDir           string        // META_DIR (por defecto DATA_DIR/meta): edit log y snapshot; vacio = sin persistencia
	SnapshotEvery     int           // SNAPSHOT_EVERY: operaciones entre snapshots
	SnapshotInterval  time.Duration // SNAPSHOT_INTERVAL_SEC: tiempo entre snapshots periodicos
	HARequireAck      bool          // HA_REQUIRE_ACK: el cliente solo recibe confirmacion si el standby aplico la operacion
	HATimeout         time.Duration // HA_TIMEOUT_MS: espera maxima del ACK del standby
	HASyncInterval    time.Duration // HA_SYNC_INTERVAL_SEC: cada cuanto se pone al dia al standby
	FailoverInterval  time.Duration // FAILOVER_INTERVAL_MS: entre pings al otro NameNode
	FailoverFailures  int           // FAILOVER_FAILURES: fallos seguidos para promover
	RecoveryWait      time.Duration // RECOVERY_WAIT_SEC: espera maxima de block reports tras promover
	RecoveryThreshold float64       // RECOVERY_THRESHOLD: fraccion de bloques con replica conocida
	TLSEnabled        bool          // TLS_ENABLED: HTTPS en la REST y mTLS en el gRPC; false solo en desarrollo
	TLSCertFile       string        // TLS_CERT_FILE
	TLSKeyFile        string        // TLS_KEY_FILE
	TLSCAFile         string        // TLS_CA_FILE
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
	hbSec, err := envInt(getenv, "HEARTBEAT_INTERVAL_SEC", defaultHeartbeatSec)
	if err != nil {
		return config{}, err
	}
	if hbSec <= 0 {
		return config{}, fmt.Errorf("HEARTBEAT_INTERVAL_SEC debe ser mayor que 0")
	}
	cfg.HeartbeatInterval = time.Duration(hbSec) * time.Second
	deadSec, err := envInt(getenv, "DEAD_AFTER_SEC", defaultDeadAfterSec)
	if err != nil {
		return config{}, err
	}
	if deadSec <= hbSec {
		return config{}, fmt.Errorf("DEAD_AFTER_SEC debe ser mayor que HEARTBEAT_INTERVAL_SEC")
	}
	cfg.DeadAfter = time.Duration(deadSec) * time.Second

	cfg.AdvertiseAddr = envString(getenv, "ADVERTISE_ADDR", "")
	cfg.PeerAddr = envString(getenv, "PEER_NAMENODE_ADDR", "")
	if cfg.PeerAddr == "" {
		cfg.PeerAddr = otherNameNode(getenv("NAMENODE_ADDRS"), cfg.AdvertiseAddr)
	}
	if cfg.PeerAddr != "" && cfg.PeerAddr == cfg.AdvertiseAddr {
		return config{}, fmt.Errorf("PEER_NAMENODE_ADDR no puede ser la direccion de este mismo NameNode (%s)", cfg.PeerAddr)
	}

	// Los metadatos van por defecto en DATA_DIR/meta: en los contenedores DATA_DIR=/data
	// es el volumen que no se borra al redesplegar.
	dataDir := strings.TrimRight(envString(getenv, "DATA_DIR", defaultDataDir), "/\\")
	if dataDir == "" {
		dataDir = "/"
	}
	if cfg.MetaDir, err = envDir(getenv, "META_DIR", strings.TrimRight(dataDir, "/")+"/meta"); err != nil {
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

	foMS, err := envNonNegative(getenv, "FAILOVER_INTERVAL_MS", defaultFailoverIntervalMS)
	if err != nil {
		return config{}, err
	}
	if foMS == 0 {
		return config{}, fmt.Errorf("FAILOVER_INTERVAL_MS debe ser mayor que 0")
	}
	cfg.FailoverInterval = time.Duration(foMS) * time.Millisecond
	if cfg.FailoverFailures, err = envInt(getenv, "FAILOVER_FAILURES", defaultFailoverFailures); err != nil {
		return config{}, err
	}
	if cfg.FailoverFailures <= 0 {
		return config{}, fmt.Errorf("FAILOVER_FAILURES debe ser mayor que 0")
	}
	recSec, err := envNonNegative(getenv, "RECOVERY_WAIT_SEC", defaultRecoveryWaitSec)
	if err != nil {
		return config{}, err
	}
	cfg.RecoveryWait = time.Duration(recSec) * time.Second
	if cfg.RecoveryThreshold, err = envFraction(getenv, "RECOVERY_THRESHOLD", defaultRecoveryThreshold); err != nil {
		return config{}, err
	}

	if cfg.TLSEnabled, err = envBool(getenv, "TLS_ENABLED", true); err != nil {
		return config{}, err
	}
	cfg.TLSCertFile = envString(getenv, "TLS_CERT_FILE", defaultTLSCertFile)
	cfg.TLSKeyFile = envString(getenv, "TLS_KEY_FILE", defaultTLSKeyFile)
	cfg.TLSCAFile = envString(getenv, "TLS_CA_FILE", defaultTLSCAFile)

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

func envFraction(getenv func(string) string, key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s debe ser un número: %w", key, err)
	}
	if v <= 0 || v > 1 {
		return 0, fmt.Errorf("%s debe estar entre 0 (sin incluir) y 1", key)
	}
	return v, nil
}

// otherNameNode devuelve, de la lista NAMENODE_ADDRS ("host:puerto,host:puerto"),
// la dirección del otro NameNode: la única que no es la propia (ADVERTISE_ADDR).
// Si no se puede saber con certeza devuelve "" (sin par: el NameNode funciona solo).
func otherNameNode(list, self string) string {
	if self == "" {
		return ""
	}
	var others []string
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" && a != self {
			others = append(others, a)
		}
	}
	if len(others) == 1 {
		return others[0]
	}
	return ""
}
