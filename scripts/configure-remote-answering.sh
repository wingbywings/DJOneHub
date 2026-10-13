#!/usr/bin/env bash
# Validate actual module/installation permissions before changing the call route.
set -euo pipefail
umask 077
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
profile=home module='' installation='' check_only=0
base_url=http://127.0.0.1:7575
binary="$root_dir/dist/djonehub-macos"
die() { echo "ERROR: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check-only) check_only=1; shift ;;
    --profile|--module|--installation|--base-url|--binary)
      [[ $# -ge 2 && -n $2 ]] || die "Missing value for $1"
      case "$1" in
        --profile) profile=$2 ;; --module) module=$2 ;; --installation) installation=$2 ;;
        --base-url) base_url=$2 ;; --binary) binary=$2 ;;
      esac
      shift 2 ;;
    -h|--help)
      echo 'Usage: bash scripts/configure-remote-answering.sh --profile NAME [--module ACTUAL_ID] [--installation ACTUAL_ID] [--check-only]'
      echo 'Optional: --base-url http://127.0.0.1:PORT --binary /path/to/djonehub-macos'
      echo 'Only selects a unique ready module and a unique paired installation; never guesses aliases.'
      exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done
[[ $base_url =~ ^http://127\.0\.0\.1:[0-9]+$ ]] || die 'The Hub API must use literal loopback HTTP.'
for value in "$profile" "$module" "$installation"; do
  [[ -z $value || $value =~ ^[A-Za-z0-9_-]+$ ]] || die 'Invalid profile/module/installation ID.'
  [[ $value != *REPLACE* ]] || die 'Replace placeholder IDs with actual API IDs.'
done
[[ -x $binary ]] || die "Missing executable: $binary"
for command in curl jq; do command -v "$command" >/dev/null || die "Missing $command"; done
work=$(mktemp -d "${TMPDIR:-/tmp}/djonehub-route.XXXXXX")
trap 'rm -rf "$work"' EXIT
api() {
  local method=$1 path=$2 output=$3 code
  shift 3
  code=$(curl -sS --max-time 10 -X "$method" "$base_url$path" -o "$output" -w '%{http_code}' "$@") || die "Cannot reach Hub API: $path"
  if [[ ! $code =~ ^2[0-9][0-9]$ ]]; then
    echo "HTTP $code: $path" >&2
    jq -r '.error // .code // "API request failed"' "$output" >&2 || true
    exit 1
  fi
}
api GET /api/devices "$work/devices.json"
jq -e 'type == "array"' "$work/devices.json" >/dev/null || die 'Invalid devices response.'
"$binary" remote status --profile "$profile" > "$work/profile.json"
jq -e '.phase == "ready" and (.principal.modules | type == "array")' "$work/profile.json" >/dev/null || die 'Profile is not ready; resolve enrollment first.'
jq --slurpfile profile "$work/profile.json" \
  '[.[] | select(.state == "ready") | . as $device | select($profile[0].principal.modules | index($device.id))]' \
  "$work/devices.json" > "$work/eligible.json"
if [[ -z $module ]]; then
  if [[ $(jq 'length' "$work/eligible.json") != 1 ]]; then
    jq -r '.[] | [.id, .alias, .state] | @tsv' "$work/devices.json" >&2
    jq -c '.principal.modules' "$work/profile.json" >&2
    die 'Expected one authorized ready module. Use --module with an actual ID; aliases are not authorization IDs.'
  fi
  module=$(jq -r '.[0].id' "$work/eligible.json")
fi
jq -e --arg module "$module" 'any(.[]; .id == $module and .state == "ready")' "$work/devices.json" >/dev/null || die "Module $module is not a ready device in /api/devices."
if ! jq -e --arg module "$module" '.principal.modules | index($module) != null' "$work/profile.json" >/dev/null; then
  jq -c '.principal.modules' "$work/profile.json" >&2
  die "Profile $profile does not authorize $module. Re-enroll/re-pair using the actual module ID before configuring calls."
fi
"$binary" remote installations --profile "$profile" > "$work/installations.json"
jq --arg module "$module" '[.[] | select(.modules | index($module))]' "$work/installations.json" > "$work/phones.json"
if [[ -z $installation ]]; then
  [[ $(jq 'length' "$work/phones.json") == 1 ]] || {
    jq -c '.[]' "$work/phones.json" >&2
    die 'Expected one paired iPhone for this module. Pair it first, or specify --installation when there are several.'
  }
  installation=$(jq -r '.[0].id' "$work/phones.json")
fi
jq -e --arg installation "$installation" 'any(.[]; .id == $installation)' "$work/phones.json" >/dev/null || die 'The specified installation is not paired/authorized for this module.'
prefix="/api/devices/$module"
api GET "$prefix/calls/status" "$work/calls.json"
jq -e 'has("active") and .active == null' "$work/calls.json" >/dev/null || die 'End the current call/ringing before changing the route.'
api GET "$prefix/remote-calls/status" "$work/remote.json"
jq -e '.online_transport.configured == true and .capabilities.remote_media == true and .codec.available == true' "$work/remote.json" >/dev/null || die 'Start terminal A with the same profile, native control and an Opus build first.'
echo "Validated module=$module installation=$installation profile=$profile"
if [[ $check_only == 1 ]]; then
  echo 'Checks passed; no call configuration was changed.'
  exit 0
fi
# New Web releases require a local management session and verify the target
# again at commit. Detect that boundary BEFORE this legacy script disables AI.
web_management_code=$(curl -sS --max-time 5 "$base_url/api/remote-management/session/status" -o /dev/null -w '%{http_code}') || die 'Cannot verify management API before changing AI.'
[[ $web_management_code != 200 ]] || die 'This release manages routes through authenticated Web UI. Open /remote/, select the module and iPhone, then adjust AI in the module page if desired. No configuration was changed.'
api GET "$prefix/voice-agent/status" "$work/ai.json"
jq -e '.state | select(type == "object") | {
  enabled:false, provider, fallback_provider, model, voice, instructions,
  stt_provider, fallback_stt_provider, stt_model,
  tools_enabled, audit_enabled, redact_pii, auto_answer, auto_answer_delay_ms
} | with_entries(select(.value != null))' "$work/ai.json" > "$work/ai-disabled.json"
api PUT "$prefix/voice-agent/config" "$work/ai-result.json" \
  -H 'Content-Type: application/json' --data-binary "@$work/ai-disabled.json"
jq -n --arg installation "$installation" \
  '{enabled:true,target_installation_id:$installation,ring_timeout_seconds:60,caller_display:"module"}' > "$work/config.json"
api PUT "$prefix/remote-calls/config" "$work/config-result.json" \
  -H 'Content-Type: application/json' --data-binary "@$work/config.json"
api GET "$prefix/remote-calls/status" "$work/result.json"
jq . "$work/result.json"
jq -e --arg installation "$installation" '.effective_route == "remote" and .config.enabled == true and .config.target_installation_id == $installation' "$work/result.json" >/dev/null || die 'The resulting route is not the requested remote installation.'
echo 'Remote route configured. Wait for Hub online and iPhone foreground/online, then place a NEW incoming call.'
