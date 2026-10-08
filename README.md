# vyos

[VyOS](https://vyos.io) rolling release as a container image, plus the one thing a VyOS router on
Hetzner Cloud needs that VyOS does not ship: a helper that moves Floating IPs, alias IPs and
network routes to the router that has just become VRRP master.

The image is what [router-api](https://github.com/hauke-cloud/router-api) runs on its router
instances. Running VyOS as a container on a stock host makes a router upgrade an image pull
instead of an image install.

## How the image is built

VyOS publishes no container image. `make rootfs`:

1. downloads the nightly ISO pinned in [`VYOS_VERSION`](VYOS_VERSION) from
   [vyos-nightly-build](https://github.com/vyos/vyos-nightly-build/releases),
2. verifies its minisign signature against [`keys/vyos-nightly.pub`](keys/vyos-nightly.pub),
3. converts it with [`scripts/iso-to-oci`](scripts/iso-to-oci), an unmodified copy of upstream's
   script (`make update-iso-to-oci` refreshes it). It strips what a container cannot use
   (kernel, firmware, podman) and masks the systemd units that fight a container runtime.

The conversion runs as root or under `fakeroot`: unpacked as a regular user every file would
belong to that user, and VyOS then refuses to commit configuration.

The [`Dockerfile`](Dockerfile) adds that root filesystem to an empty image and copies in
`hcloud-vrrp-failover`.

```sh
make rootfs   # needs xorriso, squashfs-tools, jq, xz, fakeroot and go or minisign
make image
make smoke    # boots it and round-trips configuration through the REST API
```

A weekly workflow proposes the newest nightly as a pull request; CI builds and boots it before
anything is pushed.

## Running it

```sh
podman run -d --name vyos --privileged --network host \
  -v /lib/modules:/lib/modules:ro \
  -v vyos-config:/opt/vyatta/etc/config \
  ghcr.io/hauke-cloud/vyos:<tag>
```

- `--privileged` and a **rootful** engine: VyOS writes sysctls and loads kernel modules. Rootless
  it boots, but `firewall` and `interfaces wireguard` cannot be committed.
- `/lib/modules` from the host, because the image carries no kernel.
- `/opt/vyatta/etc/config` (`/config` inside VyOS) is the only state. Keep it on a volume.
- The image has no default TLS certificate. `service https` needs one from `pki`, otherwise the
  API does not come up.
- The container stops on SIGRTMIN+3 (set as `STOPSIGNAL`); its health is
  `systemctl is-system-running`.

## hcloud-vrrp-failover

VRRP alone does not fail over on Hetzner Cloud: the platform delivers a Floating IP, an alias IP
or a routed prefix to one server until the API says otherwise. VyOS runs the helper when a VRRP
group becomes master:

```
set high-availability vrrp group wan transition-script master '/usr/local/bin/hcloud-vrrp-failover wan'
```

It asks the metadata service which server it is on and then, for the named group:

- assigns the group's **Floating IPs** to this server,
- removes the group's **alias IPs** from whichever server holds them and adds them here,
- repoints the group's **network routes** at this server's address in that network.

It is idempotent, waits for every Hetzner action to complete and keeps going after an error
(half a failover restores half the service). What failed is tried again every two seconds until
it works or `-timeout` is up: when a router is being replaced, the address it held is locked for
a moment while its server is deleted.

The helper talks to `api.hetzner.cloud`, so **the router needs a name server**
(`set system name-server ...`). Without one VRRP still elects a master, and the addresses stay
where they were.

Configuration is `/config/hetzner/failover.json`:

```json
{
  "tokenFile": "/config/hetzner/token",
  "groups": {
    "wan": {
      "floatingIPs": ["203.0.113.10"],
      "aliasIPs": ["10.0.1.100"],
      "routes": [{"network": 1234567, "destination": "0.0.0.0/0"}]
    }
  }
}
```

`tokenFile` holds a Hetzner Cloud API token with read and write access. Unknown keys are
rejected, so a typo cannot silently drop an address from the failover.

```
hcloud-vrrp-failover [-config path] [-timeout 2m] [-server-id id] <vrrp-group>
```

## Development

```sh
make check    # format, vet, golangci-lint, tests
make help
```

## License

The repository is licensed under the GNU General Public License v3.0, see [LICENSE](LICENSE).
`scripts/iso-to-oci` is from [vyos-build](https://github.com/vyos/vyos-build) and VyOS itself is
distributed under its own licenses.
