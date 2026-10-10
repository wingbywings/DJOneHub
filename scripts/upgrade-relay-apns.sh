#!/usr/bin/env bash
# Upgrade an existing managed CentOS 8 Relay; never run the first-time installer.
set -euo pipefail
umask 077
export GOTOOLCHAIN=local
unset GOROOT
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
apns_source='' go_bin=''
usage() {
  cat <<'HELP'
Usage: sudo bash scripts/upgrade-relay-apns.sh --apns-config /absolute/apns.sandbox.json [--go-bin /absolute/go]
For the existing djonehub user, loopback 18443 and nginx deployment only.
Builds and validates first, then stops Relay, backs up SQLite, upgrades and restarts.
Disconnects active calls. Preserves pairing/TURN/nginx. Does not send a test push.
On post-stop failure, leaves Relay stopped and prints the private recovery directory.
HELP
}
die() { echo "ERROR: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --apns-config|--go-bin)
      [[ $# -ge 2 ]] || die "Missing option value"
      if [[ $1 == --apns-config ]]; then apns_source=$2; else go_bin=$2; fi
      shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die 'Unknown argument' ;;
  esac
done
[[ $EUID == 0 && $(uname -s) == Linux ]] || die 'Run as root on the Relay Linux host.'
[[ $apns_source == /* && -f $apns_source && ! -L $apns_source ]] || die 'Provide an absolute private APNs JSON file.'
for command in systemctl python3 runuser curl install flock; do command -v "$command" >/dev/null || die "Missing $command"; done
exec 9>/run/lock/djonehub-apns-upgrade.lock
flock -n 9 || die 'Another APNs upgrade is running.'
[[ $(systemctl show djonehub-relay -p User --value) == djonehub ]] || die 'Unexpected service user.'
systemctl is-active --quiet djonehub-relay || die 'Relay must be active before starting this upgrade.'
for target in /usr/local/bin/djonehub-relay /var/lib/djonehub-relay /var/lib/djonehub-relay/identities.sqlite /etc/djonehub /etc/djonehub/apns.json /etc/djonehub/apns.json.new /usr/local/bin/djonehub-relay.new /etc/systemd/system/djonehub-relay.service.d /etc/systemd/system/djonehub-relay.service.d/30-apns.conf; do
  [[ ! -L $target ]] || die "Refusing symlink: $target"
done
if [[ -e /etc/systemd/system/djonehub-relay.service.d/30-apns.conf ]]; then
  grep -qx '# Managed by DJOneHub APNs upgrade.' /etc/systemd/system/djonehub-relay.service.d/30-apns.conf || die 'Existing APNs override is unmanaged.'
fi
[[ -x /usr/local/bin/djonehub-relay && -f /var/lib/djonehub-relay/identities.sqlite ]] || die 'Existing Relay binary/database missing.'
work=$(mktemp -d /var/tmp/djonehub-apns-build.XXXXXX)
backup='' stopped=false
cleanup() {
  result=$?
  if [[ $result != 0 && $stopped == true ]]; then
    systemctl stop djonehub-relay || true
    echo "Upgrade incomplete. Relay left stopped. Private recovery directory: $backup" >&2
    echo 'Use the recovery instructions; do not start an old binary on a migrated database.' >&2
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT
systemctl show djonehub-relay -p ExecStart --value | python3 "$root_dir/deploy/remote-call/upgrade_apns.py" unit > "$work/30-apns.conf"
if [[ -z $go_bin ]]; then
  for candidate in /opt/djonehub/go/bin/go /usr/local/go/bin/go "$(command -v go || true)"; do
    if [[ -n $candidate && -x $candidate ]]; then go_bin=$candidate; break; fi
  done
fi
[[ $go_bin == /* && -x $go_bin ]] || die 'Pass --go-bin pointing to Go 1.26.3+.'
echo '[1/5] Building new Relay before stopping the existing service...'
(cd "$root_dir" && CGO_ENABLED=1 "$go_bin" build -trimpath -o "$work/djonehub-relay" ./cmd/djonehub-relay)
"$work/djonehub-relay" check-apns --apns-config "$apns_source"
echo '[2/5] Stopping Relay and creating a consistent private backup...'
backup=$(mktemp -d /var/lib/djonehub-apns-backup.XXXXXX)
cp -p /usr/local/bin/djonehub-relay "$backup/djonehub-relay"
systemctl cat djonehub-relay > "$backup/service-before.txt"
for name in /etc/djonehub/apns.json /etc/systemd/system/djonehub-relay.service.d/30-apns.conf; do
  if [[ -e $name ]]; then cp -p "$name" "$backup/$(basename "$name")"; fi
 done
stopped=true
systemctl stop djonehub-relay
[[ $(systemctl show djonehub-relay -p MainPID --value) == 0 ]] || die 'Relay process has not stopped.'
python3 "$root_dir/deploy/remote-call/upgrade_apns.py" backup /var/lib/djonehub-relay/identities.sqlite "$backup/identities.sqlite"
echo '[3/5] Installing the provider configuration and APNs service override...'
install -o djonehub -g djonehub -m 600 "$apns_source" /etc/djonehub/apns.json.new
mv /etc/djonehub/apns.json.new /etc/djonehub/apns.json
install -m 755 "$work/djonehub-relay" /usr/local/bin/djonehub-relay.new
mv /usr/local/bin/djonehub-relay.new /usr/local/bin/djonehub-relay
runuser -u djonehub -- /usr/local/bin/djonehub-relay check-apns --apns-config /etc/djonehub/apns.json
install -d -m 755 /etc/systemd/system/djonehub-relay.service.d
install -m 644 "$work/30-apns.conf" /etc/systemd/system/djonehub-relay.service.d/30-apns.conf
systemctl daemon-reload
echo '[4/5] Starting Relay and checking local capabilities...'
systemctl start djonehub-relay
healthy=false
for attempt in $(seq 1 10); do
  if curl -fsS --max-time 2 http://127.0.0.1:18443/healthz > "$work/health.json" && python3 "$root_dir/deploy/remote-call/upgrade_apns.py" health < "$work/health.json"; then
    healthy=true; break
  fi
  sleep 1
done
[[ $healthy == true ]] || die 'Updated Relay health check failed.'
systemctl is-active --quiet djonehub-relay || die 'Relay is not active.'
echo '[5/5] Upgrade complete. Local Relay advertises voip_push=true.'
echo "Private pre-upgrade backup: $backup"
echo 'Next check public /healthz, enable background calls in the iPhone App, then test a real incoming call.'
stopped=false
