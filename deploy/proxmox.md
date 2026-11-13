# Reolink Protect Bridge in a Proxmox LXC

A small unprivileged Debian 13 container is enough (1 core, 512 MB RAM, 4 GB disk).
All commands in steps 1-3 run in a shell on the **Proxmox host** (web UI > node > Shell, or SSH).

## 1. Look up the placeholders

| Placeholder   | Meaning | How to find it |
|---------------|---------|----------------|
| `<CTID>`      | numeric ID of the new container | `pvesh get /cluster/nextid` (next free ID) |
| `<template>`  | Debian 13 template, e.g. `local:vztmpl/debian-13-standard_13.1-2_amd64.tar.zst` | `pveam update && pveam available --section system \| grep debian-13`, download with `pveam download local <name>`, list downloaded ones with `pveam list local` |
| `<tmplstore>` | storage holding templates (usually `local`) | `pvesm status --content vztmpl` |
| `<storage>`   | storage for the container disk, e.g. `local-lvm` | `pvesm status --content rootdir` |
| `<bridge>`    | Linux bridge on your LAN, usually `vmbr0` | `pct config <other-CTID>` of an existing container, look at `net0: ...bridge=vmbr0...`, or `ip -br link show type bridge` |
| `<ip>`        | free static IP for the bridge, e.g. `192.168.1.50` | pick one outside your DHCP range; the doorbell and the UniFi console must reach it |
| `<cidr>`      | prefix length of your LAN, usually `24` | same `net0:` line (`ip=192.168.1.20/24`) |
| `<gateway>`   | your router, e.g. `192.168.1.1` | `net0:` line (`gw=...`) of another container, or `ip route \| grep default` on the host |
| `<vlan>`      | optional VLAN tag | only if your cameras live in a VLAN; append `,tag=<vlan>` to `--net0` |

## 2. Create the container

```sh
pct create <CTID> <template> \
  --hostname reolink-protect-bridge \
  --unprivileged 1 --features nesting=1 \
  --cores 1 --memory 512 --swap 256 \
  --rootfs <storage>:4 \
  --net0 name=eth0,bridge=<bridge>,ip=<ip>/<cidr>,gw=<gateway>,firewall=0 \
  --nameserver <gateway> \
  --onboot 1 --start 1
```

Example: `pct create 120 local:vztmpl/debian-13-standard_13.1-2_amd64.tar.zst --hostname reolink-protect-bridge --unprivileged 1 --features nesting=1 --cores 1 --memory 512 --swap 256 --rootfs local-lvm:4 --net0 name=eth0,bridge=vmbr0,ip=192.168.1.50/24,gw=192.168.1.1,firewall=0 --nameserver 192.168.1.1 --onboot 1 --start 1`

Notes:
- `nesting=1` lets the systemd sandboxing in the service unit work inside an unprivileged container.
- Keep the IP static: UniFi Protect remembers the camera by this address and sends talkback audio to it (UDP 7004).
- If you enable the Proxmox firewall for the container, allow inbound TCP 8080 (web UI) and UDP 7004 from the console, plus UDP broadcast on the LAN.
- If port 8080 is taken, run the installer as `RPB_LISTEN=:8081 sh install.sh` on the first install, or change the port later on the web page under Options.

## 3. Open a shell in the container

```sh
pct enter <CTID>
```

## 4. Install the bridge (inside the container)

```sh
apt-get update && apt-get install -y curl
curl -fsSLO https://raw.githubusercontent.com/jgerdes5/reolink-protect-bridge/main/deploy/install.sh
sh install.sh
```

While the repository is private, the download needs a GitHub token
(fine-grained, read-only "Contents" permission on the repository):

```sh
export GITHUB_TOKEN=github_pat_xxx
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github.raw" \
  -o install.sh https://api.github.com/repos/jgerdes5/reolink-protect-bridge/contents/deploy/install.sh
sh install.sh
```

Alternatively copy a checkout into the container and build it there:
`sh deploy/install.sh --from-source /root/reolink-protect-bridge`
(uses Debian's Go; `GOTOOLCHAIN=auto` fetches the required newer Go automatically).

The script installs ffmpeg, creates the user `rpb`, installs
`/usr/local/bin/reolink-protect-bridge` and the systemd service, and starts it.
Running it again upgrades to the latest release.

## 5. Open the web UI

Browse to `http://<ip>:8080` (`<ip>` is the container's address from step 1,
e.g. `http://192.168.1.50:8080`; the script prints the exact URL) and follow
the setup: create the admin password, then enter the doorbell's address and
credentials and the UniFi console with a local user or an adoption token.
After saving, the camera shows up in UniFi Protect and adopts itself; there is
nothing to click in Protect.

Useful commands inside the container:

```sh
systemctl status reolink-protect-bridge
journalctl -u reolink-protect-bridge -f
sh install.sh --uninstall           # keeps /var/lib/reolink-protect-bridge
sh install.sh --uninstall --purge   # removes data and user too
```
