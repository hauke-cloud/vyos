package failover

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/metadata"
)

// Option configures the Hetzner Cloud client.
type Option func(*[]hcloud.ClientOption)

// WithEndpoint points the client at another API endpoint, for tests.
func WithEndpoint(endpoint string) Option {
	return func(opts *[]hcloud.ClientOption) {
		*opts = append(*opts, hcloud.WithEndpoint(endpoint))
	}
}

// WithApplication sets the name and version sent in the User-Agent header.
func WithApplication(name, version string) Option {
	return func(opts *[]hcloud.ClientOption) {
		*opts = append(*opts, hcloud.WithApplication(name, version))
	}
}

type hetznerCloud struct {
	client *hcloud.Client
}

// NewHetznerCloud returns a Cloud backed by the Hetzner Cloud API.
func NewHetznerCloud(token string, options ...Option) Cloud {
	opts := []hcloud.ClientOption{
		hcloud.WithToken(token),
		// Traffic is down until the action completes, so poll at a steady
		// short interval instead of backing off.
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(500 * time.Millisecond)}),
	}
	for _, option := range options {
		option(&opts)
	}
	return &hetznerCloud{client: hcloud.NewClient(opts...)}
}

func (h *hetznerCloud) FloatingIPs(ctx context.Context) ([]FloatingIP, error) {
	all, err := h.client.FloatingIP.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]FloatingIP, 0, len(all))
	for _, fip := range all {
		ip, ok := addrFromIP(fip.IP)
		if !ok {
			continue
		}
		converted := FloatingIP{ID: fip.ID, IP: ip}
		if fip.Server != nil {
			converted.ServerID = fip.Server.ID
		}
		out = append(out, converted)
	}
	return out, nil
}

func (h *hetznerCloud) AssignFloatingIP(ctx context.Context, floatingIPID, serverID int64) error {
	action, _, err := h.client.FloatingIP.Assign(ctx, &hcloud.FloatingIP{ID: floatingIPID}, &hcloud.Server{ID: serverID})
	return h.wait(ctx, action, err)
}

func (h *hetznerCloud) Servers(ctx context.Context) ([]Server, error) {
	all, err := h.client.Server.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(all))
	for _, server := range all {
		converted := Server{ID: server.ID}
		for _, pn := range server.PrivateNet {
			ip, ok := addrFromIP(pn.IP)
			if !ok || pn.Network == nil {
				continue
			}
			private := PrivateNet{NetworkID: pn.Network.ID, IP: ip}
			for _, alias := range pn.Aliases {
				if a, ok := addrFromIP(alias); ok {
					private.Aliases = append(private.Aliases, a)
				}
			}
			converted.PrivateNets = append(converted.PrivateNets, private)
		}
		out = append(out, converted)
	}
	return out, nil
}

func (h *hetznerCloud) SetAliasIPs(ctx context.Context, serverID, networkID int64, aliases []netip.Addr) error {
	ips := make([]net.IP, len(aliases))
	for i, alias := range aliases {
		ips[i] = alias.AsSlice()
	}
	action, _, err := h.client.Server.ChangeAliasIPs(ctx, &hcloud.Server{ID: serverID}, hcloud.ServerChangeAliasIPsOpts{
		Network:  &hcloud.Network{ID: networkID},
		AliasIPs: ips,
	})
	return h.wait(ctx, action, err)
}

func (h *hetznerCloud) Network(ctx context.Context, id int64) (Network, error) {
	network, _, err := h.client.Network.GetByID(ctx, id)
	if err != nil {
		return Network{}, err
	}
	if network == nil {
		return Network{}, fmt.Errorf("network %d does not exist", id)
	}
	out := Network{ID: network.ID}
	for _, subnet := range network.Subnets {
		if p, ok := prefixFromIPNet(subnet.IPRange); ok {
			out.Subnets = append(out.Subnets, p)
		}
	}
	for _, route := range network.Routes {
		destination, ok := prefixFromIPNet(route.Destination)
		gateway, gok := addrFromIP(route.Gateway)
		if ok && gok {
			out.Routes = append(out.Routes, NetworkRoute{Destination: destination, Gateway: gateway})
		}
	}
	return out, nil
}

func (h *hetznerCloud) AddRoute(ctx context.Context, networkID int64, route NetworkRoute) error {
	action, _, err := h.client.Network.AddRoute(ctx, &hcloud.Network{ID: networkID}, hcloud.NetworkAddRouteOpts{Route: toHcloudRoute(route)})
	return h.wait(ctx, action, err)
}

func (h *hetznerCloud) DeleteRoute(ctx context.Context, networkID int64, route NetworkRoute) error {
	action, _, err := h.client.Network.DeleteRoute(ctx, &hcloud.Network{ID: networkID}, hcloud.NetworkDeleteRouteOpts{Route: toHcloudRoute(route)})
	return h.wait(ctx, action, err)
}

func (h *hetznerCloud) wait(ctx context.Context, action *hcloud.Action, err error) error {
	if err != nil {
		return err
	}
	return h.client.Action.WaitFor(ctx, action)
}

func toHcloudRoute(route NetworkRoute) hcloud.NetworkRoute {
	return hcloud.NetworkRoute{
		Destination: &net.IPNet{
			IP:   route.Destination.Addr().AsSlice(),
			Mask: net.CIDRMask(route.Destination.Bits(), route.Destination.Addr().BitLen()),
		},
		Gateway: route.Gateway.AsSlice(),
	}
}

func addrFromIP(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	return addr.Unmap(), ok
}

func prefixFromIPNet(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	addr, ok := addrFromIP(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr, ones), true
}

// ServerID asks the Hetzner metadata service for the ID of the server this
// process runs on. An empty endpoint selects the real service.
func ServerID(ctx context.Context, endpoint string) (int64, error) {
	var opts []metadata.ClientOption
	if endpoint != "" {
		opts = append(opts, metadata.WithEndpoint(endpoint))
	}
	id, err := metadata.NewClient(opts...).InstanceIDWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("ask metadata service for the server ID: %w", err)
	}
	if id == 0 {
		return 0, errors.New("metadata service returned no server ID")
	}
	return id, nil
}
