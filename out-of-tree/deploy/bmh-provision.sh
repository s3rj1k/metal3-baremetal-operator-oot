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

# Fail before the machine is touched rather than let anaconda find no ISO.
curl -fsI --max-time 10 "${ISO_URL}" > /dev/null \
    || die "${ISO_URL} is not fetchable, has the rocky-iso-builder Job finished?"

# What bmh-create.sh withholds so nothing installs before an operator asks.
# The provisioner refuses until both of these are set.
echo "patching boot MAC ${BOOT_MAC} and root device ${ROOT_DEVICE}"
kubectl patch "${bmh}" -n "${NS}" --type=merge \
    -p "{\"spec\":{\"bootMACAddress\":\"${BOOT_MAC}\",\"rootDeviceHints\":{\"deviceName\":\"${ROOT_DEVICE}\"}}}"

# The route resolves per request against the boot MAC. Ask the way anaconda
# will, so a mismatch surfaces now and not mid install.
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

# Read once, so the screen names the budget the operator actually configured
# rather than a number that drifts out of the manifest.
budget="$(kubectl get configmap anaconda-provisioner -n "${NS}" \
    -o jsonpath='{.data.ANACONDA_INSTALL_TIMEOUT}' 2> /dev/null || true)"

# elapsed renders how long the install has been running, empty until the
# provisioner stamps a start on the host.
elapsed() {
    local started="$1" secs
    [[ -n "${started}" ]] || return 0

    secs=$(($(date +%s) - $(date -d "${started}" +%s)))

    printf '%dm%02ds elapsed of %s' "$((secs / 60))" "$((secs % 60))" "${budget:-unset}"
}

while true; do
    # One read per pass, so the screen is a single moment rather than seven. The
    # separator cannot be whitespace, bash folds runs of that into one field.
    snapshot="$(bmh_field '{.status.provisioning.state}{"|"}{.status.poweredOn}{"|"}{.status.operationalStatus}{"|"}{.metadata.annotations.anaconda\.metal3\.io/install-result}{"|"}{.metadata.annotations.anaconda\.metal3\.io/install-started}{"|"}{.status.errorMessage}{"|"}{.metadata.annotations.anaconda\.metal3\.io/install-message}')"

    # Free text last, so a message carrying the separator garbles only itself.
    IFS='|' read -r state powered opstatus result started errmsg message <<< "${snapshot}"

    clear
    printf '%s/%s   %s\n\n' "${NS}" "${BMH_NAME}" "$(date +%T)"
    printf '  state     %s\n' "${state:-gone from ${NS}}"
    printf '  powered   %s\n' "${powered:--}"
    printf '  opstatus  %s\n' "${opstatus:--}"
    printf '  image     %s\n' "${ISO_URL##*/}"
    printf '  install   %s\n' "$(elapsed "${started}")"
    printf '  report    %s\n' "${result:-waiting for the callback}"
    [[ -z "${message}" ]] || printf '  reason    %s\n' "${message}"
    [[ -z "${errmsg}" ]] || printf '  error     %s\n' "${errmsg}"

    # Where the provisioner narrates itself, so a stall says which step it hung on.
    printf '\nevents\n'
    kubectl get events -n "${NS}" --field-selector "involvedObject.name=${BMH_NAME}" \
        --sort-by=.lastTimestamp --no-headers \
        -o custom-columns='TIME:.lastTimestamp,REASON:.reason,MESSAGE:.message' 2> /dev/null \
        | tail -6 | sed 's/^/  /' || true

    printf '\n(Ctrl+C to stop)\n'
    sleep 3
done
