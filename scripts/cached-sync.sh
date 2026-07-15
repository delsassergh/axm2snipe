#!/usr/bin/env bash

# Refresh ABM data and reconcile Snipe-IT from the last known-good cache.
# A failed refresh is reported, but does not prevent a cached sync when a
# usable devices.json already exists.

set -uo pipefail

usage() {
    cat <<'EOF'
Usage: cached-sync.sh [--applecare-full]

Environment overrides:
  AXM2SNIPE_BIN                  axm2snipe binary path
  AXM2SNIPE_CONFIG               settings.yaml path
  AXM2SNIPE_CACHE_DIR            cache directory
  AXM2SNIPE_LOG_FILE             append-only job log
  AXM2SNIPE_REFRESH_LOCK_FILE    prevents overlapping ABM refresh jobs
  SNIPE_WRITE_LOCK_FILE          shared with other Snipe-IT writers
  SNIPE_WRITE_LOCK_WAIT_SECONDS  wait for another Snipe writer (default 1800)
  AXM2SNIPE_MAX_CACHE_AGE_HOURS  stale-cache alert threshold (default 48)
EOF
}

applecare_full=false
case "${1:-}" in
    "") ;;
    --applecare-full) applecare_full=true ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
esac
if (( $# > 1 )); then
    usage >&2
    exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/.." && pwd)

binary=${AXM2SNIPE_BIN:-$repo_dir/axm2snipe}
config=${AXM2SNIPE_CONFIG:-$repo_dir/settings.yaml}
cache_dir=${AXM2SNIPE_CACHE_DIR:-$repo_dir/.cache}
log_file=${AXM2SNIPE_LOG_FILE:-$repo_dir/cron-sync.log}
refresh_lock=${AXM2SNIPE_REFRESH_LOCK_FILE:-/tmp/axm2snipe-refresh.lock}
snipe_lock=${SNIPE_WRITE_LOCK_FILE:-/tmp/snipe-write.lock}
snipe_lock_wait=${SNIPE_WRITE_LOCK_WAIT_SECONDS:-1800}
max_cache_age_hours=${AXM2SNIPE_MAX_CACHE_AGE_HOURS:-48}
device_cache=$cache_dir/devices.json

mkdir -p -- "$cache_dir" "$(dirname -- "$log_file")"

timestamp() {
    date '+%Y-%m-%dT%H:%M:%S%z'
}

log() {
    printf '%s %s\n' "$(timestamp)" "$*" >>"$log_file"
}

alert() {
    log "ERROR: $*"
    printf 'axm2snipe scheduled job: %s\nSee %s\n' "$*" "$log_file" >&2
}

if [[ ! -x "$binary" ]]; then
    alert "binary is missing or not executable: $binary"
    exit 2
fi
if [[ ! -r "$config" ]]; then
    alert "configuration is missing or unreadable: $config"
    exit 2
fi
if ! [[ "$snipe_lock_wait" =~ ^[0-9]+$ && "$max_cache_age_hours" =~ ^[0-9]+$ ]]; then
    alert "lock wait and cache age settings must be non-negative integers"
    exit 2
fi

# Hold this lock for the complete refresh workflow so nightly and weekly ABM
# jobs cannot overlap. Kandji should use snipe_lock instead, which is held only
# during the final Snipe write phase.
exec 9>"$refresh_lock"
if ! flock -n 9; then
    log "SKIP: another ABM refresh job already holds $refresh_lock"
    exit 0
fi

failures=()
run_phase() {
    local label=$1
    shift
    log "START: $label"
    if "$@" >>"$log_file" 2>&1; then
        log "OK: $label"
        return 0
    else
        local rc=$?
        failures+=("$label (exit $rc)")
        alert "$label failed with exit $rc"
        return "$rc"
    fi
}

log "BEGIN: cached sync workflow (applecare_full=$applecare_full)"

run_phase "ABM device refresh" \
    "$binary" --config "$config" download --devices --cache-dir "$cache_dir" -v || true

applecare_args=(--config "$config" download --applecare --cache-dir "$cache_dir" -v)
if [[ "$applecare_full" == true ]]; then
    applecare_args+=(--applecare-full)
fi
run_phase "AppleCare refresh" "$binary" "${applecare_args[@]}" || true

cache_usable=true
if [[ ! -s "$device_cache" ]]; then
    cache_usable=false
    failures+=("device cache unavailable")
    alert "no usable device cache exists at $device_cache; cached sync skipped"
else
    cache_mtime=$(stat -c %Y -- "$device_cache" 2>/dev/null || printf '0')
    now=$(date +%s)
    cache_age_hours=$(( (now - cache_mtime) / 3600 ))
    if (( cache_age_hours > max_cache_age_hours )); then
        failures+=("device cache stale (${cache_age_hours}h)")
        alert "device cache is ${cache_age_hours} hours old (threshold ${max_cache_age_hours}); syncing last known-good data"
    else
        log "CACHE: devices.json age is ${cache_age_hours}h (threshold ${max_cache_age_hours}h)"
    fi
fi

if [[ "$cache_usable" == true ]]; then
    # Kandji should acquire this same lock around its Snipe write operation.
    # Only the write phase is serialized; long AppleCare downloads do not
    # unnecessarily block hourly Kandji inventory refreshes.
    run_phase "Snipe cached sync" \
        flock -w "$snipe_lock_wait" "$snipe_lock" \
        "$binary" --config "$config" sync --use-cache --cache-dir "$cache_dir" -v || true
fi

if (( ${#failures[@]} > 0 )); then
    summary=$(IFS=', '; printf '%s' "${failures[*]}")
    alert "workflow completed with problems: $summary"
    exit 1
fi

log "COMPLETE: cached sync workflow succeeded"
