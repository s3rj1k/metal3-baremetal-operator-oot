# Shared knobs and helpers for the bmh-*.sh scripts. Sourced, never executed.

# shellcheck shell=bash

# Every knob is overridable from the environment.
: "${NS:=metal3-system}"
: "${BMH_NAME:=vm1}"
: "${BOOT_MAC:=52:54:00:a8:28:2f}"
: "${UUID:=cffcd0f5-e447-4de4-b28e-3aa313133dd9}"
# No scheme suffix, so BMO resolves this to https on 443.
: "${BMC_ADDR:=redfish-virtualmedia://bmc.s3rj1k.fyi/redfish/v1/Systems/${UUID}}"
: "${BMC_USER:=admin}"
: "${BMC_PASS:=password}"
# Must match ANACONDA_BASE_URL, where the ISO, kickstart and callback all live.
: "${BASE_URL:=http://172.17.1.10:8080}"
: "${ISO_URL:=${BASE_URL}/rocky-10.2-x86_64-ks1.iso}"
# Path segment baked into the ISO's inst.ks URL.
: "${KS_ID:=kickstart}"
# Secret holding this host's kickstart under the key "value".
: "${KS_SECRET:=${BMH_NAME}-ks}"
# The disk the kickstart wipes. Provisioning refuses rather than guess it.
: "${ROOT_DEVICE:=/dev/vda}"
: "${RESTART_BMO:=false}"

bmh="baremetalhost.metal3.io/${BMH_NAME}"

# bmh_field prints one jsonpath off the host, empty when the host is gone.
bmh_field() {
    kubectl get "${bmh}" -n "${NS}" -o jsonpath="$1" 2> /dev/null || true
}

# die appends what BMO recorded, the only explanation a bad state ever offers.
die() {
    local why
    why="$(bmh_field '{.status.errorMessage}')"

    echo "error: $*${why:+: ${why}}" >&2
    exit 1
}
