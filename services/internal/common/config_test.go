package common

import (
	"reflect"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"NODE_ID", "RACK", "GRPC_PORT", "REST_PORT", "DATA_DIR", "NAMENODE_ADDRS", "ADVERTISE_ADDR", "BLOCK_SIZE_MB", "REPLICATION_FACTOR"} {
		t.Setenv(key, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != DefaultNodeID || cfg.Rack != DefaultRack || cfg.GRPCPort != DefaultGRPCPort || cfg.RESTPort != DefaultRESTPort || cfg.DataDir != DefaultDataDir || cfg.AdvertiseAddr != DefaultAdvertiseAddr || cfg.BlockSizeMB != DefaultBlockSizeMB || cfg.ReplicationFactor != DefaultReplicationFactor || !reflect.DeepEqual(cfg.NameNodeAddrs, []string{DefaultNameNodeAddr}) {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigEnvironment(t *testing.T) {
	clearConfigEnv(t)
	for key, value := range map[string]string{"NODE_ID": "dn-2", "RACK": "rack-b", "GRPC_PORT": "50052", "REST_PORT": "8081", "DATA_DIR": "blocks", "NAMENODE_ADDRS": " nn-a:50050, ,nn-b:50050 ", "ADVERTISE_ADDR": "dn-2.example", "BLOCK_SIZE_MB": "32", "REPLICATION_FACTOR": "2"} {
		t.Setenv(key, value)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{NodeID: "dn-2", Rack: "rack-b", GRPCPort: 50052, RESTPort: 8081, DataDir: "blocks", NameNodeAddrs: []string{"nn-a:50050", "nn-b:50050"}, AdvertiseAddr: "dn-2.example", BlockSizeMB: 32, ReplicationFactor: 2}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigRejectsInvalidNumbers(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"GRPC_PORT", "abc"}, {"GRPC_PORT", "0"}, {"GRPC_PORT", "65536"}, {"REST_PORT", "-1"}, {"REST_PORT", "65536"}, {"BLOCK_SIZE_MB", "0"}, {"REPLICATION_FACTOR", "-2"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}
