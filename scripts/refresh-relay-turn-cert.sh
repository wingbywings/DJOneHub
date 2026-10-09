#!/usr/bin/env bash
# Managed by DJOneHub relay installer.
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run as root.' >&2; exit 1; }
source /etc/djonehub/deployment.env
work=$(mktemp -d /etc/djonehub/tls/.refresh.XXXXXX)
trap 'rm -rf "$work"' EXIT
install -m 600 "$DJ_CERT_SOURCE" "$work/fullchain.pem"
install -m 600 "$DJ_KEY_SOURCE" "$work/privkey.pem"
openssl verify -purpose sslserver -verify_hostname "$DJ_DOMAIN" \
  -untrusted "$work/fullchain.pem" "$work/fullchain.pem"
openssl x509 -in "$work/fullchain.pem" -noout -checkend 86400
openssl x509 -in "$work/fullchain.pem" -pubkey -noout |
  openssl pkey -pubin -outform DER > "$work/cert.pub"
openssl pkey -in "$work/privkey.pem" -passin pass: -pubout -outform DER > "$work/key.pub"
cmp -s "$work/cert.pub" "$work/key.pub" || { echo 'Certificate/key mismatch.' >&2; exit 1; }
if cmp -s "$work/fullchain.pem" /etc/djonehub/tls/fullchain.pem &&
   cmp -s "$work/privkey.pem" /etc/djonehub/tls/privkey.pem; then
  echo 'TURN certificate already current.'
  exit 0
fi
chown djonehub:djonehub "$work/fullchain.pem" "$work/privkey.pem"
mv -f "$work/fullchain.pem" /etc/djonehub/tls/fullchain.pem
mv -f "$work/privkey.pem" /etc/djonehub/tls/privkey.pem
if systemctl is-active --quiet djonehub-turn; then
  systemctl restart djonehub-turn
fi
echo 'TURN certificate updated. An active TURN service was restarted; nginx was not changed.'
