#!/usr/bin/env bash

# Remove everything bmh-create.sh made, so the next run starts from nothing.

# Lint with shellcheck -x deploy/bmh-delete.sh
# (one line, "shellcheck" starting a comment makes it a directive)

set -o errexit -o nounset -o pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/bmh-env.sh"

# A merge patch clears the list even when the object carries none, where a json
# remove fails outright on a missing path.
strip_finalizers() {
    kubectl patch "$1" -n "${NS}" --type=merge \
        -p '{"metadata":{"finalizers":null}}' > /dev/null 2>&1 || true
}

echo "removing ${BMH_NAME} in ${NS}"

# The HardwareData CR outlives its host and inspection prefers it over the BMC,
# so leaving it behind serves the last run's inventory to the next host.
owned=(
    "dataimage.metal3.io/${BMH_NAME}"
    "hardwaredata.metal3.io/${BMH_NAME}"
    "${bmh}"
    "secret/${BMH_NAME}-bmc"
    "secret/${KS_SECRET}"
)

# Delete first so the object carries a deletion timestamp, then drop the
# finalizers and it goes at once rather than waiting on a controller.
for res in "${owned[@]}"; do
    kubectl delete "${res}" -n "${NS}" --ignore-not-found --wait=false
    strip_finalizers "${res}"
done

# Left over from the Ironic flow this replaces, harmless but confusing next to a
# host that reads none of them.
kubectl delete secret -n "${NS}" --ignore-not-found --wait=false \
    "${BMH_NAME}-network-data" "${BMH_NAME}-preprovisioning-network-data" \
    "${BMH_NAME}-userdata" "${BMH_NAME}-metadata"

# BMO goes on reconciling a host that is already gone, which wedges the next one
# on a stale UID, and only a restart clears that cache.
if [[ "${RESTART_BMO}" == "true" ]]; then
    kubectl rollout restart deployment/baremetal-operator-controller-manager -n "${NS}"
    kubectl rollout status deployment/baremetal-operator-controller-manager -n "${NS}" --timeout=300s
fi

echo "${BMH_NAME} is gone, recreate it with deploy/bmh-create.sh"
