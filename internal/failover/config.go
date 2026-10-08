// Package failover moves Hetzner Cloud resources to the router that has just
// become VRRP master.
//
// VRRP alone is not enough on Hetzner Cloud: a gratuitous ARP does not make
// the platform deliver a Floating IP, an alias IP or a routed prefix to
// another server. Each of them has to be reassigned through the API, and that
// has to happen on the router itself so that a failover does not depend on
// anything outside the pair being reachable.
package failover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// DefaultConfigPath is where the helper looks for its configuration. /config
// is the one directory VyOS keeps across image upgrades.
const DefaultConfigPath = "/config/hetzner/failover.json"

// Config is the helper's configuration file.
type Config struct {
	// TokenFile holds the Hetzner Cloud API token. It is a separate file so
	// that this one can be world readable.
	TokenFile string `json:"tokenFile"`
	// Groups maps a VRRP group or sync-group name to what its master owns.
	Groups map[string]Group `json:"groups"`
}

// Group is everything that follows one VRRP group to its master.
type Group struct {
	// FloatingIPs are assigned to the master.
	FloatingIPs []string `json:"floatingIPs,omitempty"`
	// AliasIPs are private addresses bound to the master's network interface.
	AliasIPs []string `json:"aliasIPs,omitempty"`
	// Routes are network routes whose gateway becomes the master.
	Routes []Route `json:"routes,omitempty"`
}

// Route is a route of a Hetzner Cloud Network that points at the master.
type Route struct {
	// Network is the ID of the network the route belongs to.
	Network int64 `json:"network"`
	// Destination is the routed prefix in CIDR notation.
	Destination string `json:"destination"`
}

// LoadConfig reads and validates the configuration file at path.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's to choose
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	// A misspelled key would otherwise silently drop an address from the
	// failover, which only shows the day the failover is needed.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	var errs []error
	if c.TokenFile == "" {
		errs = append(errs, errors.New("tokenFile is required"))
	}
	for name, group := range c.Groups {
		for _, ip := range group.FloatingIPs {
			if _, err := netip.ParseAddr(ip); err != nil {
				errs = append(errs, fmt.Errorf("group %q: floating IP %q is not an IP address", name, ip))
			}
		}
		for _, ip := range group.AliasIPs {
			if _, err := netip.ParseAddr(ip); err != nil {
				errs = append(errs, fmt.Errorf("group %q: alias IP %q is not an IP address", name, ip))
			}
		}
		for _, route := range group.Routes {
			if route.Network == 0 {
				errs = append(errs, fmt.Errorf("group %q: route to %q needs a network", name, route.Destination))
			}
			if _, err := netip.ParsePrefix(route.Destination); err != nil {
				errs = append(errs, fmt.Errorf("group %q: route destination %q is not a CIDR", name, route.Destination))
			}
		}
	}
	return errors.Join(errs...)
}

// Group returns the configuration of the named VRRP group.
func (c Config) Group(name string) (Group, error) {
	group, ok := c.Groups[name]
	if !ok {
		return Group{}, fmt.Errorf("no failover configuration for VRRP group %q", name)
	}
	return group, nil
}

// ReadToken returns the API token stored in TokenFile.
func (c Config) ReadToken() (string, error) {
	raw, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", c.TokenFile)
	}
	return token, nil
}
