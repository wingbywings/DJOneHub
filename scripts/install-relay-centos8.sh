#!/usr/bin/env bash
# CentOS/RHEL 8 deployment behind an EXISTING nginx HTTPS virtual host.
set -euo pipefail
umask 077
# Probe the bundled compiler, not a toolchain automatically selected by go.mod.
# Keep selection, version output and the actual build in the same environment.
export GOTOOLCHAIN=local
unset GOROOT
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
domain='' public_ip='' bind_ip='' cert='' key='' turn_bin=''
usage() {
  cat <<'HELP'
Usage: sudo bash scripts/install-relay-centos8.sh \
  --domain YOUR_EXISTING_HTTPS_HOST --public-ip SERVER_PUBLIC_IPV4 \
  --cert /absolute/fullchain.pem --key /absolute/privkey.pem \
  [--bind-ip SERVER_INTERFACE_IPV4] [--turnserver-bin /absolute/turnserver]

Installs dependencies, a suitable Go toolchain if needed, Relay, coturn and
systemd units. Keeps SQLite and an existing compatible TURN secret. Rerunning
restarts the two DJOneHub services and interrupts calls. Does not rewrite nginx,
change repositories, firewall or SELinux. Prints the nginx include to add.
HELP
}
die() { echo "ERROR: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --domain|--public-ip|--bind-ip|--cert|--key|--turnserver-bin)
      [[ $# -ge 2 && -n $2 ]] || die "Missing value for $1"
      case "$1" in
        --domain) domain=$2 ;; --public-ip) public_ip=$2 ;; --bind-ip) bind_ip=$2 ;;
        --cert) cert=$2 ;; --key) key=$2 ;; --turnserver-bin) turn_bin=$2 ;;
      esac
      shift 2 ;;
    *) die "Unknown argument: $1" ;;
  esac
done
[[ -n $domain && -n $public_ip && -n $cert && -n $key ]] || { usage; exit 1; }
[[ $EUID == 0 ]] || die 'Run with sudo/root.'
[[ $(uname -s) == Linux ]] || die 'This installer runs on Linux only.'
source /etc/os-release
[[ ${VERSION_ID%%.*} == 8 ]] || die 'This installer targets EL8 only.'
command -v dnf >/dev/null || die 'dnf is required; fix package repositories first.'
command -v systemctl >/dev/null || die 'systemd is required.'
[[ $cert == /* && $key == /* && -r $cert && -r $key ]] || die 'Provide readable absolute certificate/key paths.'
[[ -f $root_dir/go.mod && -f $root_dir/cmd/djonehub-relay/main.go ]] || die 'Incomplete source bundle.'

# Never silently replace a previous manual deployment, including its credentials.
for target in /etc/systemd/system/djonehub-relay.service \
  /etc/systemd/system/djonehub-turn.service /etc/djonehub/turnserver.conf \
  /etc/djonehub/deployment.env /etc/nginx/djonehub/relay.locations.conf \
  /usr/local/sbin/djonehub-refresh-turn-cert; do
  [[ ! -L $target ]] || die "Refusing symlink: $target"
  if [[ -e $target ]] && ! grep -q '^# Managed by DJOneHub relay installer\.' "$target"; then
    die "Existing unmanaged file: $target. Preserve it and migrate explicitly."
  fi
done
if systemctl is-active --quiet coturn; then
  die 'A separate coturn.service is already active. Check its users before stopping or migrating it.'
fi
if [[ -e /etc/djonehub/turn-issuer.json && ! -f /etc/djonehub/deployment.env ]]; then
  die 'Existing manual TURN issuer found. Preserve its secret and migrate explicitly.'
fi
[[ ! -L /etc/djonehub/turn-issuer.json ]] || die 'TURN issuer cannot be a symlink.'

echo '[1/6] Installing compiler and runtime dependencies (using your configured repos)...'
dnf install -y gcc gcc-c++ make pkgconf-pkg-config ca-certificates openssl python3 curl jq iproute
if [[ -z $turn_bin ]]; then
  for candidate in "$(type -P turnserver || true)" /usr/bin/turnserver /usr/sbin/turnserver \
    /usr/local/bin/turnserver /usr/local/sbin/turnserver; do
    if [[ -n $candidate && -f $candidate && -x $candidate ]]; then
      turn_bin=$candidate
      break
    fi
  done
  if [[ -z $turn_bin ]]; then
    dnf install -y epel-release
    dnf install -y coturn
    turn_bin=$(type -P turnserver || true)
  fi
fi
[[ $turn_bin == /* && -f $turn_bin && -x $turn_bin ]] || die 'Cannot find an executable turnserver; pass --turnserver-bin /absolute/path.'
# /bin and /usr/bin may name the same file; use the resolved path in systemd too.
turn_bin=$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$turn_bin")
[[ $turn_bin =~ ^/[A-Za-z0-9_./+-]+$ ]] || die 'turnserver path contains unsupported systemd command characters.'
echo "Using TURN executable: $turn_bin"
if [[ -z $bind_ip ]]; then
  bind_ip=$(ip -4 route get "$public_ip" | awk '{for (i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')
fi
[[ -n $bind_ip ]] || die 'Cannot determine the interface IP; pass --bind-ip.'
work=$(mktemp -d /var/tmp/djonehub-install.XXXXXX)
trap 'rm -rf "$work"' EXIT

existing=()
[[ ! -e /etc/djonehub/turn-issuer.json ]] || existing=(--existing-issuer /etc/djonehub/turn-issuer.json)
"$turn_bin" --help > "$work/turnserver-help.txt" 2>&1 || die 'Cannot read turnserver options.'
python3 "$root_dir/deploy/remote-call/render_install.py" --domain "$domain" \
  --public-ip "$public_ip" --bind-ip "$bind_ip" --cert "$cert" --key "$key" \
  --turnserver-bin "$turn_bin" --turnserver-help-file "$work/turnserver-help.txt" \
  --output "$work/config" "${existing[@]}"
openssl verify -purpose sslserver -verify_hostname "$domain" -untrusted "$cert" "$cert"
openssl x509 -in "$cert" -noout -checkend 86400
openssl x509 -in "$cert" -pubkey -noout | openssl pkey -pubin -outform DER > "$work/cert.pub"
openssl pkey -in "$key" -passin pass: -pubout -outform DER > "$work/key.pub"
cmp -s "$work/cert.pub" "$work/key.pub" || die 'Certificate/key mismatch.'
if ss -Hltn 'sport = :18443' | grep -q . && ! systemctl is-active --quiet djonehub-relay; then
  die 'Loopback port 18443 is occupied by another service.'
fi
if { ss -Hltn '( sport = :3478 or sport = :5349 )'; ss -Hlun 'sport = :3478'; } | grep -q . &&
  ! systemctl is-active --quiet djonehub-turn; then
  die 'TURN ports are already occupied; check existing services before proceeding.'
fi

echo '[2/6] Selecting Go >= 1.26.3 (existing installation is preserved)...'
go_bin=''
for candidate in "$(command -v go || true)" /usr/local/go/bin/go /opt/djonehub/go/bin/go; do
  [[ -n $candidate && -x $candidate ]] || continue
  if "$candidate" version | python3 -c 'import re,sys; m=re.search(r"go(\d+)\.(\d+)\.(\d+)\b",sys.stdin.read()); sys.exit(0 if m and tuple(map(int,m.groups())) >= (1,26,3) else 1)'; then
    go_bin=$candidate
    break
  fi
done
if [[ -z $go_bin ]]; then
  case $(uname -m) in x86_64) go_arch=amd64 ;; aarch64) go_arch=arm64 ;; *) die 'Unsupported Go download architecture.' ;; esac
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 \
    'https://go.dev/dl/?mode=json&include=all' -o "$work/go-releases.json"
  # Select the newest stable patch from the 1.26 series and verify its official SHA-256.
  python3 - "$work/go-releases.json" "$go_arch" > "$work/go-download" <<'PY'
import json, re, sys
choices = []
for release in json.load(open(sys.argv[1])):
    match = re.fullmatch(r"go1\.26\.(\d+)", release["version"])
    if not match or not release.get("stable") or int(match.group(1)) < 3:
        continue
    for f in release["files"]:
        if f["os"] == "linux" and f["arch"] == sys.argv[2] and f["kind"] == "archive":
            expected = release["version"] + ".linux-" + sys.argv[2] + ".tar.gz"
            if f["filename"] == expected and re.fullmatch(r"[0-9a-f]{64}", f["sha256"]):
                choices.append((int(match.group(1)), f))
if not choices:
    raise SystemExit("No supported verified Go release found")
f = max(choices, key=lambda x: x[0])[1]
print(f["filename"], f["sha256"])
PY
  read -r go_archive go_sha < "$work/go-download"
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 "https://go.dev/dl/$go_archive" -o "$work/go.tar.gz"
  printf '%s  %s\n' "$go_sha" "$work/go.tar.gz" | sha256sum -c -
  tar -xzf "$work/go.tar.gz" -C "$work"
  install -d -m 755 /opt/djonehub
  [[ ! -e /opt/djonehub/go ]] || die 'Existing /opt/djonehub/go is unsuitable; move it aside explicitly before retrying.'
  mv "$work/go" /opt/djonehub/go
  go_bin=/opt/djonehub/go/bin/go
fi
"$go_bin" version

echo '[3/6] Building Relay with SQLite/CGO...'
(cd "$root_dir" && CGO_ENABLED=1 GOTOOLCHAIN=local "$go_bin" build -trimpath -o "$work/djonehub-relay" ./cmd/djonehub-relay)

echo '[4/6] Installing private configuration and service files...'
if ! id djonehub >/dev/null 2>&1; then
  useradd --system --user-group --home-dir /var/lib/djonehub-relay --shell /sbin/nologin djonehub
fi
getent group djonehub >/dev/null || die 'Existing djonehub account has no matching group.'
install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay /var/lib/djonehub-relay/transfer
install -d -o root -g djonehub -m 750 /etc/djonehub /etc/djonehub/tls
install -d -m 755 /etc/nginx/djonehub /usr/local/sbin /usr/local/bin
# A private backup supports manual rollback; SQLite and its WAL are never copied live or deleted.
backup=$(mktemp -d /etc/djonehub/backup.XXXXXX)
for name in turn-issuer.json turnserver.conf deployment.env; do
  [[ ! -e /etc/djonehub/$name ]] || cp -p "/etc/djonehub/$name" "$backup/$name"
done
for name in djonehub-relay.service djonehub-turn.service; do
  [[ ! -e /etc/systemd/system/$name ]] || cp -p "/etc/systemd/system/$name" "$backup/$name"
  install -m 644 "$work/config/$name" "/etc/systemd/system/$name"
done
if [[ -f /usr/local/bin/djonehub-relay ]]; then
  cp -p /usr/local/bin/djonehub-relay "$backup/djonehub-relay"
fi
install -m 755 "$work/djonehub-relay" /usr/local/bin/djonehub-relay.new
mv -f /usr/local/bin/djonehub-relay.new /usr/local/bin/djonehub-relay
install -o djonehub -g djonehub -m 600 "$work/config/turn-issuer.json" /etc/djonehub/turn-issuer.json
install -o djonehub -g djonehub -m 600 "$work/config/turnserver.conf" /etc/djonehub/turnserver.conf
install -o root -g root -m 600 "$work/config/deployment.env" /etc/djonehub/deployment.env
install -m 644 "$root_dir/deploy/remote-call/nginx-relay.locations.conf" /etc/nginx/djonehub/relay.locations.conf
install -m 700 "$root_dir/scripts/refresh-relay-turn-cert.sh" /usr/local/sbin/djonehub-refresh-turn-cert
/usr/local/sbin/djonehub-refresh-turn-cert

echo '[5/6] Starting DJOneHub services...'
systemctl daemon-reload
systemctl enable djonehub-turn djonehub-relay
systemctl restart djonehub-turn djonehub-relay
for attempt in {1..15}; do
  if curl -fsS http://127.0.0.1:18443/healthz -o "$work/health.json"; then break; fi
  sleep 1
done
for service in djonehub-turn djonehub-relay; do
  systemctl is-active --quiet "$service" || die "Service failed: $service; inspect journalctl."
done
jq -e '.status == "ok" and .capabilities.turn == true' "$work/health.json" >/dev/null || die 'Local Relay health check failed.'

echo '[6/6] Local services ready. One nginx integration step remains:'
echo 'Add this line INSIDE the existing HTTPS server { } for your domain:'
echo '    include /etc/nginx/djonehub/relay.locations.conf;'
echo 'Reserve /v1/ and /healthz for DJOneHub, then run nginx -t && nginx -s reload.'
echo "Public check: curl -fsS https://$domain/healthz"
echo 'Open TURN 3478/tcp, 3478/udp, 5349/tcp, 49160-49259/udp in the active firewall zone AND cloud security group.'
echo 'Do not expose port 18443. No existing nginx config, firewall or SELinux setting was changed.'
echo "Private rollback files: $backup"
