package failover

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// FloatingIP is a Hetzner Cloud Floating IP.
type FloatingIP struct {
	ID int64
	IP netip.Addr
	// ServerID is the server the address is assigned to, 0 if unassigned.
	ServerID int64
}

// PrivateNet is a server's attachment to a network.
type PrivateNet struct {
	NetworkID int64
	IP        netip.Addr
	Aliases   []netip.Addr
}

// Server is a Hetzner Cloud server, reduced to what a failover reads.
type Server struct {
	ID          int64
	PrivateNets []PrivateNet
}

// NetworkRoute is a route of a network.
type NetworkRoute struct {
	Destination netip.Prefix
	Gateway     netip.Addr
}

// Network is a Hetzner Cloud Network.
type Network struct {
	ID      int64
	Subnets []netip.Prefix
	Routes  []NetworkRoute
}

// Cloud is the part of the Hetzner Cloud API a failover needs. Mutating calls
// return once the change is in effect.
type Cloud interface {
	FloatingIPs(ctx context.Context) ([]FloatingIP, error)
	AssignFloatingIP(ctx context.Context, floatingIPID, serverID int64) error
	Servers(ctx context.Context) ([]Server, error)
	// SetAliasIPs replaces all alias IPs of a server in a network.
	SetAliasIPs(ctx context.Context, serverID, networkID int64, aliases []netip.Addr) error
	Network(ctx context.Context, id int64) (Network, error)
	AddRoute(ctx context.Context, networkID int64, route NetworkRoute) error
	DeleteRoute(ctx context.Context, networkID int64, route NetworkRoute) error
}

// Takeover makes the server with the given ID the owner of everything in
// group. It is idempotent, and it keeps going after an error: half a failover
// restores half of the service, which beats restoring none of it. All errors
// are returned joined.
func Takeover(ctx context.Context, cloud Cloud, serverID int64, group Group) error {
	return errors.Join(
		takeFloatingIPs(ctx, cloud, serverID, group.FloatingIPs),
		takePrivate(ctx, cloud, serverID, group),
	)
}

// TakeoverUntilDone calls Takeover until it succeeds or ctx ends, waiting
// interval between attempts, and reports every failed attempt to onError.
//
// A failover runs into transient refusals. When a router is replaced, the new
// master tries to take a Floating IP at the moment the server that held it is
// being deleted, and Hetzner answers that the address is locked. Giving up
// there would leave the address assigned to nobody.
func TakeoverUntilDone(ctx context.Context, cloud Cloud, serverID int64, group Group, interval time.Duration, onError func(error)) error {
	for {
		err := Takeover(ctx, cloud, serverID, group)
		if err == nil {
			return nil
		}
		onError(err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(interval):
		}
	}
}

func takeFloatingIPs(ctx context.Context, cloud Cloud, serverID int64, ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	all, err := cloud.FloatingIPs(ctx)
	if err != nil {
		return fmt.Errorf("list floating IPs: %w", err)
	}

	var errs []error
	for _, raw := range ips {
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("floating IP %q: %w", raw, err))
			continue
		}
		i := slices.IndexFunc(all, func(f FloatingIP) bool { return f.IP == ip })
		if i < 0 {
			errs = append(errs, fmt.Errorf("floating IP %s does not exist in this project", ip))
			continue
		}
		if all[i].ServerID == serverID {
			continue
		}
		if err := cloud.AssignFloatingIP(ctx, all[i].ID, serverID); err != nil {
			errs = append(errs, fmt.Errorf("assign floating IP %s: %w", ip, err))
		}
	}
	return errors.Join(errs...)
}

// takePrivate handles alias IPs and routes, which both live in a network the
// server is attached to.
func takePrivate(ctx context.Context, cloud Cloud, serverID int64, group Group) error {
	if len(group.AliasIPs) == 0 && len(group.Routes) == 0 {
		return nil
	}
	servers, err := cloud.Servers(ctx)
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	i := slices.IndexFunc(servers, func(s Server) bool { return s.ID == serverID })
	if i < 0 {
		return fmt.Errorf("server %d does not exist in this project", serverID)
	}
	me := servers[i]

	return errors.Join(
		takeAliasIPs(ctx, cloud, me, servers, group.AliasIPs),
		takeRoutes(ctx, cloud, me, group.Routes),
	)
}

func takeAliasIPs(ctx context.Context, cloud Cloud, me Server, servers []Server, ips []string) error {
	if len(ips) == 0 {
		return nil
	}

	// An alias IP belongs to whichever of our networks has a subnet that
	// contains it.
	subnets := make(map[int64][]netip.Prefix, len(me.PrivateNets))
	var errs []error
	for _, pn := range me.PrivateNets {
		network, err := cloud.Network(ctx, pn.NetworkID)
		if err != nil {
			errs = append(errs, fmt.Errorf("get network %d: %w", pn.NetworkID, err))
			continue
		}
		subnets[pn.NetworkID] = network.Subnets
	}

	wanted := make(map[int64][]netip.Addr)
	for _, raw := range ips {
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("alias IP %q: %w", raw, err))
			continue
		}
		networkID, ok := networkOf(ip, me, subnets)
		if !ok {
			errs = append(errs, fmt.Errorf("alias IP %s is in none of this server's networks", ip))
			continue
		}
		wanted[networkID] = append(wanted[networkID], ip)
	}

	for _, pn := range me.PrivateNets {
		take := wanted[pn.NetworkID]
		if len(take) == 0 {
			continue
		}
		// Hetzner rejects an alias that is still bound to another server.
		for _, other := range servers {
			if other.ID == me.ID {
				continue
			}
			if err := releaseAliasIPs(ctx, cloud, other, pn.NetworkID, take); err != nil {
				errs = append(errs, err)
			}
		}

		aliases := slices.Clone(pn.Aliases)
		for _, ip := range take {
			if !slices.Contains(aliases, ip) {
				aliases = append(aliases, ip)
			}
		}
		if len(aliases) == len(pn.Aliases) {
			continue
		}
		if err := cloud.SetAliasIPs(ctx, me.ID, pn.NetworkID, aliases); err != nil {
			errs = append(errs, fmt.Errorf("set alias IPs in network %d: %w", pn.NetworkID, err))
		}
	}
	return errors.Join(errs...)
}

func networkOf(ip netip.Addr, me Server, subnets map[int64][]netip.Prefix) (int64, bool) {
	for _, pn := range me.PrivateNets {
		for _, subnet := range subnets[pn.NetworkID] {
			if subnet.Contains(ip) {
				return pn.NetworkID, true
			}
		}
	}
	return 0, false
}

func releaseAliasIPs(ctx context.Context, cloud Cloud, server Server, networkID int64, take []netip.Addr) error {
	for _, pn := range server.PrivateNets {
		if pn.NetworkID != networkID {
			continue
		}
		remaining := slices.DeleteFunc(slices.Clone(pn.Aliases), func(ip netip.Addr) bool {
			return slices.Contains(take, ip)
		})
		if len(remaining) == len(pn.Aliases) {
			continue
		}
		if err := cloud.SetAliasIPs(ctx, server.ID, networkID, remaining); err != nil {
			return fmt.Errorf("release alias IPs from server %d: %w", server.ID, err)
		}
	}
	return nil
}

func takeRoutes(ctx context.Context, cloud Cloud, me Server, routes []Route) error {
	var errs []error
	for _, route := range routes {
		if err := takeRoute(ctx, cloud, me, route); err != nil {
			errs = append(errs, fmt.Errorf("route %s in network %d: %w", route.Destination, route.Network, err))
		}
	}
	return errors.Join(errs...)
}

func takeRoute(ctx context.Context, cloud Cloud, me Server, route Route) error {
	destination, err := netip.ParsePrefix(route.Destination)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(me.PrivateNets, func(pn PrivateNet) bool { return pn.NetworkID == route.Network })
	if i < 0 {
		return errors.New("this server is not attached to the network")
	}
	want := NetworkRoute{Destination: destination, Gateway: me.PrivateNets[i].IP}

	network, err := cloud.Network(ctx, route.Network)
	if err != nil {
		return fmt.Errorf("get network: %w", err)
	}
	// A network holds one route per destination, so the peer's has to go
	// before ours can be added.
	for _, existing := range network.Routes {
		if existing.Destination != destination {
			continue
		}
		if existing == want {
			return nil
		}
		if err := cloud.DeleteRoute(ctx, route.Network, existing); err != nil {
			return fmt.Errorf("delete route via %s: %w", existing.Gateway, err)
		}
	}
	if err := cloud.AddRoute(ctx, route.Network, want); err != nil {
		return fmt.Errorf("add route via %s: %w", want.Gateway, err)
	}
	return nil
}
