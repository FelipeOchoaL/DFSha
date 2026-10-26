package common

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultNodeID            = "datanode-1"
	DefaultRack              = "rack-a"
	DefaultGRPCPort          = 50051
	DefaultRESTPort          = 8080
	DefaultDataDir           = "./data"
	DefaultNameNodeAddr      = "localhost:9000"
	DefaultAdvertiseAddr     = "localhost"
	DefaultBlockSizeMB       = 64
	DefaultReplicationFactor = 3
)

// Config holds the environment shared by NameNode and DataNode binaries.
type Config struct {
	NodeID            string
	Rack              string
	GRPCPort          int
	RESTPort          int
	DataDir           string
	NameNodeAddrs     []string
	AdvertiseAddr     string
	BlockSizeMB       int
	ReplicationFactor int
}

// LoadConfig reads the service configuration from environment variables.
func LoadConfig() (Config, error) {
	cfg := Config{
		NodeID:            envString("NODE_ID", DefaultNodeID),
		Rack:              envString("RACK", DefaultRack),
		DataDir:           envString("DATA_DIR", DefaultDataDir),
		NameNodeAddrs:     envList("NAMENODE_ADDRS", []string{DefaultNameNodeAddr}),
		AdvertiseAddr:     envString("ADVERTISE_ADDR", DefaultAdvertiseAddr),
		GRPCPort:          DefaultGRPCPort,
		RESTPort:          DefaultRESTPort,
		BlockSizeMB:       DefaultBlockSizeMB,
		ReplicationFactor: DefaultReplicationFactor,
	}

	var err error
	if cfg.GRPCPort, err = envInt("GRPC_PORT", DefaultGRPCPort); err != nil {
		return Config{}, err
	}
	if cfg.RESTPort, err = envInt("REST_PORT", DefaultRESTPort); err != nil {
		return Config{}, err
	}
	if cfg.BlockSizeMB, err = envInt("BLOCK_SIZE_MB", DefaultBlockSizeMB); err != nil {
		return Config{}, err
	}
	if cfg.ReplicationFactor, err = envInt("REPLICATION_FACTOR", DefaultReplicationFactor); err != nil {
		return Config{}, err
	}

	if cfg.GRPCPort <= 0 || cfg.GRPCPort > 65535 {
		return Config{}, fmt.Errorf("GRPC_PORT must be between 1 and 65535")
	}
	if cfg.RESTPort <= 0 || cfg.RESTPort > 65535 {
		return Config{}, fmt.Errorf("REST_PORT must be between 1 and 65535")
	}
	if cfg.BlockSizeMB <= 0 {
		return Config{}, fmt.Errorf("BLOCK_SIZE_MB must be greater than 0")
	}
	if cfg.ReplicationFactor <= 0 {
		return Config{}, fmt.Errorf("REPLICATION_FACTOR must be greater than 0")
	}

	return cfg, nil
}

func envString(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envList(key string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}

	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return fallback
	}
	return values
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

// AdvertiseHostPort extrae host y puerto de ADVERTISE_ADDR.
// Acepta "host:puerto" (AWS: la IP pública y 50051) o solo "host" (usa fallbackPort).
func AdvertiseHostPort(addr string, fallbackPort int) (host string, port uint32, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, fmt.Errorf("ADVERTISE_ADDR is empty")
	}
	h, p, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		if fallbackPort <= 0 || fallbackPort > 65535 {
			return "", 0, fmt.Errorf("ADVERTISE_ADDR %q has no port and GRPC_PORT is invalid", addr)
		}
		return addr, uint32(fallbackPort), nil
	}
	n, convErr := strconv.Atoi(p)
	if convErr != nil || n <= 0 || n > 65535 || h == "" {
		return "", 0, fmt.Errorf("ADVERTISE_ADDR %q must include host and a valid port", addr)
	}
	return h, uint32(n), nil
}
