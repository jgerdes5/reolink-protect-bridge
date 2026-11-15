#!/bin/sh
# Reolink Protect Bridge installer for Linux with systemd (incl. Proxmox LXC).
#
# Usage (as root):
#   sh install.sh                        # install/upgrade latest release binary
#   sh install.sh --version v0.1.0       # install a specific release
#   sh install.sh --from-source <path>   # build from a local checkout
#                                        # (<path>: the checkout's directory, the one
#                                        # with go.mod, e.g. /root/reolink-protect-bridge)
#   sh install.sh --uninstall            # remove service + binary, keep data
#   sh install.sh --uninstall --purge    # also remove user rpb and all data
#
# Environment:
#   GITHUB_TOKEN  personal access token (fine-grained: "Contents: read" on
#                 jgerdes5/reolink-protect-bridge) - needed while the repo
#                 is private. Example:
#                   GITHUB_TOKEN=github_pat_xxx sh install.sh
#   RPB_LISTEN    web page address for a new installation (default :8080),
#                 e.g. RPB_LISTEN=:8081 if port 8080 is taken. Only used
#                 when the configuration is created; later change it on the
#                 web page under Options > Advanced.
#
# Re-running the script is safe; it upgrades the binary and restarts the
# service, leaving the data directory untouched.
set -eu

REPO="jgerdes5/reolink-protect-bridge"
NAME="reolink-protect-bridge"
BIN="/usr/local/bin/$NAME"
UNIT="/etc/systemd/system/$NAME.service"
DATA="/var/lib/$NAME"
USER_NAME="rpb"
LISTEN="${RPB_LISTEN:-}"
DROPIN="/etc/systemd/system/$NAME.service.d/listen.conf"

MODE=install
SRC=""
VERSION="latest"
PURGE=0

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The help text is here, not read from "$0": under "curl ... | sh" there is no
# script file.
usage() {
	cat <<'USAGE'
Reolink Protect Bridge installer for Linux with systemd (incl. Proxmox LXC).
On Debian/Ubuntu it installs ffmpeg itself; elsewhere install ffmpeg and curl first.

Usage (as root):
  sh install.sh                        install/upgrade latest release binary
  sh install.sh --version v0.1.0       install a specific release
  sh install.sh --from-source <path>   build from a local checkout
                                       (<path>: directory with go.mod,
                                       e.g. /root/reolink-protect-bridge)
  sh install.sh --uninstall            remove service + binary, keep data
  sh install.sh --uninstall --purge    also remove user rpb and all data

Environment:
  GITHUB_TOKEN  GitHub token with read access to the repository, needed
                while it is private
  RPB_LISTEN    web page address for a new installation (default :8080)
USAGE
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--from-source)
		[ $# -ge 2 ] || die "--from-source needs a path"
		MODE=source; SRC="$2"; shift 2 ;;
	--version)
		[ $# -ge 2 ] || die "--version needs a tag (e.g. v0.1.0)"
		VERSION="$2"; shift 2 ;;
	--uninstall) MODE=uninstall; shift ;;
	--purge) PURGE=1; shift ;;
	-h|--help) usage 0 ;;
	*) printf 'unknown option: %s\n\n' "$1" >&2; usage 1 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (e.g. sudo sh install.sh)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

# ---------------------------------------------------------------- uninstall --
if [ "$MODE" = uninstall ]; then
	log "stopping and removing service"
	systemctl disable --now "$NAME.service" 2>/dev/null || true
	rm -f "$UNIT" "$DROPIN"
	rmdir "$(dirname "$DROPIN")" 2>/dev/null || true
	systemctl daemon-reload
	rm -f "$BIN"
	if [ "$PURGE" -eq 1 ]; then
		log "purging data $DATA and user $USER_NAME"
		rm -rf "$DATA"
		if id "$USER_NAME" >/dev/null 2>&1; then
			userdel "$USER_NAME" 2>/dev/null || true
		fi
	else
		log "data kept in $DATA (use --purge to delete)"
	fi
	log "uninstalled"
	exit 0
fi

# ------------------------------------------------------------- dependencies --
if command -v apt-get >/dev/null 2>&1; then
	export DEBIAN_FRONTEND=noninteractive
	PKGS="ffmpeg ca-certificates"
	if [ "$MODE" = install ]; then
		PKGS="$PKGS curl"
	else
		PKGS="$PKGS golang-go git"
	fi
	missing=""
	for p in $PKGS; do
		dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q "install ok installed" ||
			missing="$missing $p"
	done
	if [ -n "$missing" ]; then
		log "installing packages:$missing"
		apt-get update -q
		# shellcheck disable=SC2086
		apt-get install -y -q --no-install-recommends $missing
	fi
else
	# other distributions: the tools must already be installed
	CMDS="ffmpeg"
	if [ "$MODE" = install ]; then
		CMDS="$CMDS curl"
	else
		CMDS="$CMDS go git"
	fi
	missing=""
	for c in $CMDS; do
		command -v "$c" >/dev/null 2>&1 || missing="$missing $c"
	done
	[ -z "$missing" ] || die "missing:$missing (install with your package manager, then run this again)"
fi

# --------------------------------------------------------------------- user --
if ! id "$USER_NAME" >/dev/null 2>&1; then
	log "creating system user $USER_NAME"
	useradd --system --home-dir "$DATA" --no-create-home \
		--shell /usr/sbin/nologin "$USER_NAME"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT TERM

# ------------------------------------------------------------------- binary --
if [ "$MODE" = source ]; then
	[ -f "$SRC/go.mod" ] || die "$SRC does not look like a checkout (no go.mod)"
	SRC="$(cd "$SRC" && pwd)"
	ver="$(git -c safe.directory="$SRC" -C "$SRC" describe --tags --always --dirty 2>/dev/null || echo dev)"
	log "building $ver from $SRC (GOTOOLCHAIN=auto may download a newer Go)"
	(
		cd "$SRC"
		GOTOOLCHAIN=auto CGO_ENABLED=0 GOFLAGS=-buildvcs=false \
			go build -trimpath -ldflags "-s -w -X main.version=$ver" \
			-o "$TMP/$NAME" "./cmd/$NAME"
	)
else
	case "$(uname -m)" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	armv7l|armv7*|armhf) arch=armv7 ;;
	*) die "unsupported CPU architecture: $(uname -m) (try --from-source)" ;;
	esac
	asset="${NAME}_linux_${arch}"

	api="https://api.github.com/repos/$REPO/releases"
	if [ "$VERSION" = latest ]; then api="$api/latest"; else api="$api/tags/$VERSION"; fi

	# gh_curl <url> <out> [accept]
	gh_curl() {
		if [ -n "${GITHUB_TOKEN:-}" ]; then
			curl -fsSL --retry 3 -H "Authorization: Bearer $GITHUB_TOKEN" \
				-H "Accept: ${3:-application/vnd.github+json}" -o "$2" "$1"
		else
			curl -fsSL --retry 3 -H "Accept: ${3:-application/vnd.github+json}" -o "$2" "$1"
		fi
	}

	log "looking up release $VERSION of $REPO"
	gh_curl "$api" "$TMP/release.json" ||
		die "cannot fetch release info (private repo? set GITHUB_TOKEN)"
	tag="$(sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' "$TMP/release.json" | head -n1)"

	# Asset API URLs work for private repos (with Accept: octet-stream);
	# browser_download_url does not. Map asset name -> API url.
	asset_url() {
		tr -d '\n' <"$TMP/release.json" | sed 's/{/\n{/g' |
			grep "\"name\": *\"$1\"" | sed -n 's/.*"url": *"\([^"]*\/releases\/assets\/[0-9]*\)".*/\1/p' |
			head -n1
	}
	bin_url="$(asset_url "$asset")"
	sum_url="$(asset_url SHA256SUMS)"
	[ -n "$bin_url" ] || die "release $tag has no asset $asset"
	[ -n "$sum_url" ] || die "release $tag has no SHA256SUMS, refusing to install an unverified binary"

	log "downloading $asset ($tag)"
	gh_curl "$bin_url" "$TMP/$asset" application/octet-stream
	gh_curl "$sum_url" "$TMP/SHA256SUMS" application/octet-stream
	grep -q " \*\{0,1\}$asset\$" "$TMP/SHA256SUMS" || die "SHA256SUMS has no entry for $asset"
	(cd "$TMP" && grep " \*\{0,1\}$asset\$" SHA256SUMS | sha256sum -c -) >/dev/null ||
		die "checksum mismatch for $asset"
	log "checksum ok"
	mv "$TMP/$asset" "$TMP/$NAME"
fi

was_active=0
systemctl is-active --quiet "$NAME.service" 2>/dev/null && was_active=1

install -m 0755 "$TMP/$NAME" "$BIN.new"
mv -f "$BIN.new" "$BIN"
log "installed $BIN ($("$BIN" -version 2>/dev/null || echo "version unknown"))"

# --------------------------------------------------------------------- unit --
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$DATA"

# Same text as deploy/systemd/reolink-protect-bridge.service (CI checks that
# they match); kept here so the script also works on its own.
cat >"$TMP/unit" <<'UNIT'
[Unit]
Description=Reolink Protect Bridge (Reolink doorbell as UniFi Protect camera)
Documentation=https://github.com/jgerdes5/reolink-protect-bridge
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=rpb
Group=rpb
StateDirectory=reolink-protect-bridge
StateDirectoryMode=0750
WorkingDirectory=/var/lib/reolink-protect-bridge
Environment=RPB_DATA=/var/lib/reolink-protect-bridge
# The web page address comes from the configuration (Options > Advanced).
ExecStart=/usr/local/bin/reolink-protect-bridge
Restart=always
RestartSec=5

# Hardening (ffmpeg runs as a child process under the same rules)
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
RemoveIPC=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
UMask=0027

[Install]
WantedBy=multi-user.target
UNIT

if ! cmp -s "$TMP/unit" "$UNIT"; then
	log "writing $UNIT"
	install -m 0644 "$TMP/unit" "$UNIT"
fi
# RPB_LISTEN only seeds a new configuration, see the help text.
if [ -n "$LISTEN" ]; then
	if [ -f "$DATA/config.json" ]; then
		log "RPB_LISTEN ignored: the configuration exists, change the address on the web page"
	else
		install -d -m 0755 "$(dirname "$DROPIN")"
		printf '[Service]\nEnvironment=RPB_LISTEN=%s\n' "$LISTEN" >"$DROPIN"
	fi
fi
systemctl daemon-reload
systemctl --quiet enable "$NAME.service"
if [ "$was_active" -eq 1 ]; then
	log "restarting service"
	systemctl restart "$NAME.service"
else
	log "starting service"
	systemctl start "$NAME.service"
fi

sleep 2
if ! systemctl is-active --quiet "$NAME.service"; then
	journalctl -u "$NAME.service" -n 30 --no-pager >&2 || true
	die "service failed to start (see above)"
fi

# -------------------------------------------------------------------- done --
ip="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
[ -n "$ip" ] || ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$ip" ] || ip="<this-host-ip>"
listen="$(sed -n 's/^ *"listen": *"\([^"]*\)".*/\1/p' "$DATA/config.json" 2>/dev/null | head -n1)"
[ -n "$listen" ] || listen="${LISTEN:-:8080}"
port="${listen##*:}"
log "done. Open the web UI: http://$ip:$port"
log "logs: journalctl -u $NAME -f    data: $DATA"
