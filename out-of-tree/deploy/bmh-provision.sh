#!/usr/bin/env bash

# Patch the installer ISO onto an available host and watch the install.
# Run deploy/bmh-create.sh first, this refuses any other state.

# Lint with shellcheck -x deploy/bmh-provision.sh
# (one line, "shellcheck" starting a comment makes it a directive)

set -o errexit -o nounset -o pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/bmh-env.sh"

state="$(bmh_field '{.status.provisioning.state}')"

[[ -n "${state}" ]] || die "no BareMetalHost ${BMH_NAME} in ${NS}, run deploy/bmh-create.sh"
[[ "${state}" == "available" ]] || die "${BMH_NAME} is ${state}, not available, run deploy/bmh-create.sh"

# BMO wants spec.online true and no image in status. Either one wrong takes the
# patch and then does nothing at all.
[[ "$(bmh_field '{.spec.online}')" == "true" ]] || die "${BMH_NAME} is offline, an image patch would be ignored"
[[ -z "$(bmh_field '{.status.provisioning.image.url}')" ]] || die "${BMH_NAME} already records a provisioned image"

# A boot MAC is immutable once set, so a stale one has to be caught here rather
# than as a webhook rejection halfway through.
current_mac="$(bmh_field '{.spec.bootMACAddress}')"
[[ -z "${current_mac}" || "${current_mac}" == "${BOOT_MAC}" ]] \
    || die "${BMH_NAME} declares boot MAC ${current_mac}, which cannot be changed"

# Inspection recorded every NIC, so a boot MAC the machine does not have is a
# typo worth naming before anything boots.
if [[ "$(bmh_field '{.status.hardwareDetails.nic[*].mac}')" != *"${BOOT_MAC}"* ]]; then
    echo "warning: ${BOOT_MAC} is not among the NICs inspection found" >&2
fi

# Fail before the machine is touched rather than let anaconda find no ISO.
curl -fsI --max-time 10 "${ISO_URL}" > /dev/null \
    || die "${ISO_URL} is not fetchable, has the rocky-iso-builder Job finished?"

# What bmh-create.sh left out so the host would inspect. Cleaning goes on here
# too, so only a host that was provisioned gets its disks erased on the way out.
echo "patching boot MAC ${BOOT_MAC}, root device ${ROOT_DEVICE} and cleaning mode"
kubectl patch "${bmh}" -n "${NS}" --type=merge \
    -p "{\"spec\":{\"bootMACAddress\":\"${BOOT_MAC}\",\"automatedCleaningMode\":\"metadata\",\"rootDeviceHints\":{\"deviceName\":\"${ROOT_DEVICE}\"}}}"

# The route resolves per request and needs the boot MAC patched above. Ask the
# way anaconda will, so a mismatch surfaces now and not mid install.
ks_url="${BASE_URL}/ks/${KS_ID}"
ks_marker="--hostname=${BMH_NAME}"
ks_body=""

for _ in $(seq 1 15); do
    ks_body="$(curl -fsS --max-time 10 -H "X-RHN-Provisioning-MAC-0: eth0 ${BOOT_MAC}" "${ks_url}" || true)"

    if [[ "${ks_body}" == *"${ks_marker}"* ]]; then
        break
    fi

    sleep 2
done

# A warning, not a failure. The fallback powers the machine off instead of
# installing, which is safe but baffling if nobody was told.
if [[ "${ks_body}" != *"${ks_marker}"* ]]; then
    echo "warning: ${ks_url} served no kickstart for ${BOOT_MAC}, anaconda would get the fallback" >&2
fi

# live-iso needs no checksum, and it is booted once so the reboot ending the
# install lands on disk.
kubectl patch "${bmh}" -n "${NS}" --type=merge \
    -p "{\"spec\":{\"image\":{\"url\":\"${ISO_URL}\",\"format\":\"live-iso\"}}}"

while true; do
    clear
    kubectl get "${bmh}" -n "${NS}" -o custom-columns='STATE:.status.provisioning.state,POWERED:.status.poweredOn,IMAGE:.status.provisioning.image.url,ERROR:.status.errorMessage'
    # The host leaves provisioning only once anaconda reports in.
    echo "install report: $(bmh_field '{.metadata.annotations.anaconda\.metal3\.io/install-result}')"
    echo "--- ${NS}/${BMH_NAME} --- $(date) --- (Ctrl+C to stop)"
    sleep 3
done
