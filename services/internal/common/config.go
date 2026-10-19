package common

import (
	"fmt"
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
	DefaultNameNodeAddr      = "localhost:50050"
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
