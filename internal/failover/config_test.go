package failover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeFile(t, "failover.json", `{
	  "tokenFile": "/config/hetzner/token",
	  "groups": {
	    "wan": {
	      "floatingIPs": ["203.0.113.10"],
	      "aliasIPs": ["10.0.1.100"],
	      "routes": [{"network": 7, "destination": "0.0.0.0/0"}]
	    }
	  }
	}`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	group, err := cfg.Group("wan")
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(group.FloatingIPs) != 1 || len(group.AliasIPs) != 1 || len(group.Routes) != 1 {
		t.Errorf("group = %+v", group)
	}
	if _, err := cfg.Group("lan"); err == nil {
		t.Error("Group for an unconfigured VRRP group succeeded")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
	}{
		"unknown field":    {`{"tokenFile":"t","groups":{},"floating_ips":[]}`, "floating_ips"},
		"no token file":    {`{"groups":{"wan":{}}}`, "tokenFile"},
		"bad floating IP":  {`{"tokenFile":"t","groups":{"wan":{"floatingIPs":["nope"]}}}`, "nope"},
		"bad alias IP":     {`{"tokenFile":"t","groups":{"wan":{"aliasIPs":["10.0.0.1/32"]}}}`, "10.0.0.1/32"},
		"bad destination":  {`{"tokenFile":"t","groups":{"wan":{"routes":[{"network":7,"destination":"10.0.0.1"}]}}}`, "10.0.0.1"},
		"route no network": {`{"tokenFile":"t","groups":{"wan":{"routes":[{"destination":"0.0.0.0/0"}]}}}`, "network"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeFile(t, "failover.json", tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestReadToken(t *testing.T) {
	cfg := Config{TokenFile: writeFile(t, "token", "  s3cret\n")}
	token, err := cfg.ReadToken()
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if token != "s3cret" {
		t.Errorf("token = %q", token)
	}

	cfg.TokenFile = writeFile(t, "empty", "\n")
	if _, err := cfg.ReadToken(); err == nil {
		t.Error("ReadToken for an empty file succeeded")
	}
}
