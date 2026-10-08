package failover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

// fakeAPI answers the handful of Hetzner Cloud API calls the adapter makes and
// records the bodies of the mutating ones.
type fakeAPI struct {
	t      *testing.T
	bodies map[string]map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: body %q: %v", key, raw, err)
		}
		f.bodies[key] = body
	}

	action := `{"action":{"id":1,"status":"success","command":"x","progress":100}}`
	responses := map[string]string{
		"GET /floating_ips": `{"floating_ips":[
		  {"id":1,"ip":"203.0.113.10","type":"ipv4","server":200},
		  {"id":2,"ip":"2001:db8::/64","type":"ipv6","server":null}]}`,
		"GET /servers": `{"servers":[{"id":100,"private_net":[
		  {"network":7,"ip":"10.0.1.2","alias_ips":["10.0.1.50"]}]}]}`,
		"GET /networks/7": `{"network":{"id":7,"ip_range":"10.0.0.0/16",
		  "subnets":[{"type":"cloud","ip_range":"10.0.1.0/24","network_zone":"eu-central","gateway":"10.0.1.1"}],
		  "routes":[{"destination":"0.0.0.0/0","gateway":"10.0.1.3"}]}}`,
		"POST /floating_ips/1/actions/assign":        action,
		"POST /servers/100/actions/change_alias_ips": action,
		"POST /networks/7/actions/add_route":         action,
		"POST /networks/7/actions/delete_route":      action,
	}
	body, ok := responses[key]
	if !ok {
		f.t.Errorf("unexpected request %s", key)
		http.Error(w, `{"error":{"code":"not_found","message":"not found"}}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusCreated)
	}
	_, _ = io.WriteString(w, body)
}

func newTestCloud(t *testing.T) (Cloud, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{t: t, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return NewHetznerCloud("token", WithEndpoint(srv.URL)), api
}

func TestHetznerCloudReads(t *testing.T) {
	cloud, _ := newTestCloud(t)
	ctx := context.Background()

	fips, err := cloud.FloatingIPs(ctx)
	if err != nil {
		t.Fatalf("FloatingIPs: %v", err)
	}
	wantFIPs := []FloatingIP{
		{ID: 1, IP: addr("203.0.113.10"), ServerID: 200},
		{ID: 2, IP: addr("2001:db8::")},
	}
	if !slices.Equal(fips, wantFIPs) {
		t.Errorf("FloatingIPs = %+v, want %+v", fips, wantFIPs)
	}

	servers, err := cloud.Servers(ctx)
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(servers) != 1 || len(servers[0].PrivateNets) != 1 {
		t.Fatalf("Servers = %+v", servers)
	}
	pn := servers[0].PrivateNets[0]
	if pn.NetworkID != 7 || pn.IP != addr("10.0.1.2") || !slices.Equal(pn.Aliases, []netip.Addr{addr("10.0.1.50")}) {
		t.Errorf("private net = %+v", pn)
	}

	network, err := cloud.Network(ctx, 7)
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	if !slices.Equal(network.Subnets, []netip.Prefix{prefix("10.0.1.0/24")}) {
		t.Errorf("subnets = %v", network.Subnets)
	}
	wantRoutes := []NetworkRoute{{Destination: prefix("0.0.0.0/0"), Gateway: addr("10.0.1.3")}}
	if !slices.Equal(network.Routes, wantRoutes) {
		t.Errorf("routes = %v, want %v", network.Routes, wantRoutes)
	}
}

func TestHetznerCloudWrites(t *testing.T) {
	cloud, api := newTestCloud(t)
	ctx := context.Background()
	route := NetworkRoute{Destination: prefix("192.168.0.0/16"), Gateway: addr("10.0.1.2")}

	if err := cloud.AssignFloatingIP(ctx, 1, 100); err != nil {
		t.Errorf("AssignFloatingIP: %v", err)
	}
	if err := cloud.SetAliasIPs(ctx, 100, 7, []netip.Addr{addr("10.0.1.50"), addr("10.0.1.100")}); err != nil {
		t.Errorf("SetAliasIPs: %v", err)
	}
	if err := cloud.AddRoute(ctx, 7, route); err != nil {
		t.Errorf("AddRoute: %v", err)
	}
	if err := cloud.DeleteRoute(ctx, 7, route); err != nil {
		t.Errorf("DeleteRoute: %v", err)
	}

	if got := api.bodies["POST /floating_ips/1/actions/assign"]["server"]; got != float64(100) {
		t.Errorf("assign server = %v", got)
	}
	aliases := api.bodies["POST /servers/100/actions/change_alias_ips"]
	if aliases["network"] != float64(7) || len(aliases["alias_ips"].([]any)) != 2 {
		t.Errorf("change_alias_ips body = %v", aliases)
	}
	for _, key := range []string{"POST /networks/7/actions/add_route", "POST /networks/7/actions/delete_route"} {
		body := api.bodies[key]
		if body["destination"] != "192.168.0.0/16" || body["gateway"] != "10.0.1.2" {
			t.Errorf("%s body = %v", key, body)
		}
	}
}

func TestServerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/instance-id" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "4711\n")
	}))
	defer srv.Close()

	id, err := ServerID(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ServerID: %v", err)
	}
	if id != 4711 {
		t.Errorf("id = %d", id)
	}
}
