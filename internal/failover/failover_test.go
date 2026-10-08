package failover

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	self  int64 = 100
	peer  int64 = 200
	netID int64 = 7
)

// fakeCloud is an in-memory Hetzner project. Every mutation is appended to
// calls, so a test can assert both the end state and that nothing was touched
// unnecessarily.
type fakeCloud struct {
	floatingIPs []FloatingIP
	servers     []Server
	networks    map[int64]Network
	calls       []string
	failAssign  map[int64]error
}

func (f *fakeCloud) FloatingIPs(context.Context) ([]FloatingIP, error) {
	return slices.Clone(f.floatingIPs), nil
}

func (f *fakeCloud) AssignFloatingIP(_ context.Context, id, serverID int64) error {
	if err := f.failAssign[id]; err != nil {
		return err
	}
	f.calls = append(f.calls, fmt.Sprintf("assign fip=%d server=%d", id, serverID))
	for i := range f.floatingIPs {
		if f.floatingIPs[i].ID == id {
			f.floatingIPs[i].ServerID = serverID
		}
	}
	return nil
}

func (f *fakeCloud) Servers(context.Context) ([]Server, error) {
	out := make([]Server, 0, len(f.servers))
	for _, s := range f.servers {
		c := Server{ID: s.ID}
		for _, p := range s.PrivateNets {
			p.Aliases = slices.Clone(p.Aliases)
			c.PrivateNets = append(c.PrivateNets, p)
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCloud) SetAliasIPs(_ context.Context, serverID, networkID int64, aliases []netip.Addr) error {
	f.calls = append(f.calls, fmt.Sprintf("aliases server=%d net=%d %s", serverID, networkID, joinAddrs(aliases)))
	for i := range f.servers {
		if f.servers[i].ID != serverID {
			continue
		}
		for j := range f.servers[i].PrivateNets {
			if f.servers[i].PrivateNets[j].NetworkID == networkID {
				f.servers[i].PrivateNets[j].Aliases = slices.Clone(aliases)
			}
		}
	}
	return nil
}

func (f *fakeCloud) Network(_ context.Context, id int64) (Network, error) {
	n, ok := f.networks[id]
	if !ok {
		return Network{}, fmt.Errorf("network %d not found", id)
	}
	n.Routes = slices.Clone(n.Routes)
	return n, nil
}

func (f *fakeCloud) AddRoute(_ context.Context, networkID int64, r NetworkRoute) error {
	f.calls = append(f.calls, fmt.Sprintf("route add net=%d %s via %s", networkID, r.Destination, r.Gateway))
	n := f.networks[networkID]
	n.Routes = append(n.Routes, r)
	f.networks[networkID] = n
	return nil
}

func (f *fakeCloud) DeleteRoute(_ context.Context, networkID int64, r NetworkRoute) error {
	f.calls = append(f.calls, fmt.Sprintf("route del net=%d %s via %s", networkID, r.Destination, r.Gateway))
	n := f.networks[networkID]
	n.Routes = slices.DeleteFunc(n.Routes, func(x NetworkRoute) bool { return x == r })
	f.networks[networkID] = n
	return nil
}

func joinAddrs(a []netip.Addr) string {
	s := make([]string, len(a))
	for i, x := range a {
		s[i] = x.String()
	}
	return "[" + strings.Join(s, " ") + "]"
}

func addr(s string) netip.Addr     { return netip.MustParseAddr(s) }
func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// newProject is a two-router project: the peer currently holds everything.
func newProject() *fakeCloud {
	return &fakeCloud{
		floatingIPs: []FloatingIP{
			{ID: 1, IP: addr("203.0.113.10"), ServerID: peer},
			{ID: 2, IP: addr("203.0.113.20")},
			{ID: 3, IP: addr("203.0.113.30"), ServerID: self},
			{ID: 4, IP: addr("198.51.100.1"), ServerID: peer},
		},
		servers: []Server{
			{ID: self, PrivateNets: []PrivateNet{{NetworkID: netID, IP: addr("10.0.1.2"), Aliases: []netip.Addr{addr("10.0.1.50")}}}},
			{ID: peer, PrivateNets: []PrivateNet{{NetworkID: netID, IP: addr("10.0.1.3"), Aliases: []netip.Addr{addr("10.0.1.100"), addr("10.0.1.60")}}}},
			{ID: 300},
		},
		networks: map[int64]Network{
			netID: {
				ID:      netID,
				Subnets: []netip.Prefix{prefix("10.0.1.0/24")},
				Routes:  []NetworkRoute{{Destination: prefix("0.0.0.0/0"), Gateway: addr("10.0.1.3")}},
			},
		},
	}
}

func TestTakeoverFloatingIPs(t *testing.T) {
	cloud := newProject()
	group := Group{FloatingIPs: []string{"203.0.113.10", "203.0.113.20", "203.0.113.30"}}

	if err := Takeover(context.Background(), cloud, self, group); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	// The one already here is left alone, the unrelated one is never touched.
	want := []string{"assign fip=1 server=100", "assign fip=2 server=100"}
	if !slices.Equal(cloud.calls, want) {
		t.Errorf("calls = %q, want %q", cloud.calls, want)
	}
	if got := cloud.floatingIPs[3].ServerID; got != peer {
		t.Errorf("unconfigured floating IP moved to %d", got)
	}
}

func TestTakeoverFloatingIPErrorsAreCollected(t *testing.T) {
	cloud := newProject()
	boom := errors.New("locked")
	cloud.failAssign = map[int64]error{1: boom}
	group := Group{FloatingIPs: []string{"203.0.113.10", "203.0.113.99", "203.0.113.20"}}

	err := Takeover(context.Background(), cloud, self, group)

	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
	if err == nil || !strings.Contains(err.Error(), "203.0.113.99") {
		t.Errorf("err = %v, want it to name the unknown floating IP", err)
	}
	// A failure on one address must not keep the others from moving: a
	// partial failover still restores part of the service.
	if got := cloud.floatingIPs[1].ServerID; got != self {
		t.Errorf("floating IP 2 on server %d, want %d", got, self)
	}
}

func TestTakeoverAliasIPs(t *testing.T) {
	cloud := newProject()
	group := Group{AliasIPs: []string{"10.0.1.100"}}

	if err := Takeover(context.Background(), cloud, self, group); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	// Hetzner rejects an alias that is still bound elsewhere, so it has to
	// leave the peer first. Unrelated aliases stay where they are.
	want := []string{
		"aliases server=200 net=7 [10.0.1.60]",
		"aliases server=100 net=7 [10.0.1.50 10.0.1.100]",
	}
	if !slices.Equal(cloud.calls, want) {
		t.Errorf("calls = %q, want %q", cloud.calls, want)
	}
}

func TestTakeoverAliasIPsAlreadyHere(t *testing.T) {
	cloud := newProject()
	group := Group{AliasIPs: []string{"10.0.1.50"}}

	if err := Takeover(context.Background(), cloud, self, group); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if len(cloud.calls) != 0 {
		t.Errorf("calls = %q, want none", cloud.calls)
	}
}

func TestTakeoverAliasIPOutsideEveryNetwork(t *testing.T) {
	cloud := newProject()
	group := Group{AliasIPs: []string{"192.168.9.9"}}

	err := Takeover(context.Background(), cloud, self, group)

	if err == nil || !strings.Contains(err.Error(), "192.168.9.9") {
		t.Fatalf("err = %v, want it to name the alias IP", err)
	}
	if len(cloud.calls) != 0 {
		t.Errorf("calls = %q, want none", cloud.calls)
	}
}

func TestTakeoverRoutes(t *testing.T) {
	cloud := newProject()
	group := Group{Routes: []Route{
		{Network: netID, Destination: "0.0.0.0/0"},
		{Network: netID, Destination: "192.168.0.0/16"},
	}}

	if err := Takeover(context.Background(), cloud, self, group); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	want := []string{
		"route del net=7 0.0.0.0/0 via 10.0.1.3",
		"route add net=7 0.0.0.0/0 via 10.0.1.2",
		"route add net=7 192.168.0.0/16 via 10.0.1.2",
	}
	if !slices.Equal(cloud.calls, want) {
		t.Errorf("calls = %q, want %q", cloud.calls, want)
	}

	// A second run finds everything in place.
	cloud.calls = nil
	if err := Takeover(context.Background(), cloud, self, group); err != nil {
		t.Fatalf("second Takeover: %v", err)
	}
	if len(cloud.calls) != 0 {
		t.Errorf("second run calls = %q, want none", cloud.calls)
	}
}

func TestTakeoverRouteNeedsAttachedNetwork(t *testing.T) {
	cloud := newProject()
	group := Group{Routes: []Route{{Network: 99, Destination: "0.0.0.0/0"}}}

	err := Takeover(context.Background(), cloud, self, group)

	if err == nil || !strings.Contains(err.Error(), "99") {
		t.Fatalf("err = %v, want it to name the network", err)
	}
}

func TestTakeoverUnknownServer(t *testing.T) {
	cloud := newProject()
	group := Group{AliasIPs: []string{"10.0.1.100"}}

	if err := Takeover(context.Background(), cloud, 999, group); err == nil {
		t.Fatal("Takeover for a server that is not in the project succeeded")
	}
}

// flakyCloud fails the first assignments of a Floating IP, the way Hetzner
// does while the server that held it is being deleted.
type flakyCloud struct {
	*fakeCloud
	failures int
}

func (f *flakyCloud) AssignFloatingIP(ctx context.Context, id, serverID int64) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("floating IP is locked")
	}
	return f.fakeCloud.AssignFloatingIP(ctx, id, serverID)
}

func TestTakeoverUntilDoneRetries(t *testing.T) {
	cloud := &flakyCloud{fakeCloud: newProject(), failures: 2}
	group := Group{FloatingIPs: []string{"203.0.113.10"}}
	var reported []error

	err := TakeoverUntilDone(context.Background(), cloud, self, group, time.Millisecond, func(err error) { reported = append(reported, err) })

	if err != nil {
		t.Fatalf("TakeoverUntilDone: %v", err)
	}
	if got := cloud.floatingIPs[0].ServerID; got != self {
		t.Errorf("floating IP on server %d, want %d", got, self)
	}
	// Each failure is reported: a failover that needed three attempts is
	// something to know about.
	if len(reported) != 2 {
		t.Errorf("%d failures reported, want 2", len(reported))
	}
}

func TestTakeoverUntilDoneGivesUpWithTheContext(t *testing.T) {
	cloud := &flakyCloud{fakeCloud: newProject(), failures: 1 << 30}
	group := Group{FloatingIPs: []string{"203.0.113.10"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := TakeoverUntilDone(ctx, cloud, self, group, time.Millisecond, func(error) {})

	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("err = %v, want the last failure", err)
	}
}
