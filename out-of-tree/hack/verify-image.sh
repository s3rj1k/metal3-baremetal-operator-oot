#!/usr/bin/env bash

# Prove the built image carries the published BMO release unmodified.
# Run after make image, through make verify-image.

# Lint with shellcheck -x hack/verify-image.sh

set -o errexit -o nounset -o pipefail

img="${1:?usage: verify-image.sh <image>}"
runtime="${CONTAINER_RUNTIME:-docker}"
arch="${ARCH:-amd64}"

module_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
    echo "error: $*" >&2
    exit 1
}

# The Dockerfile is the only record of which release this was built onto, so a
# hand edited ARG is caught here rather than shipping as an unknown base.
base="$(sed -n 's/^ARG BMO_IMAGE=//p' "${module_dir}/Dockerfile")"
[[ -n "${base}" ]] || die "no ARG BMO_IMAGE in ${module_dir}/Dockerfile"
[[ "${base}" == *@sha256:* ]] || die "ARG BMO_IMAGE ${base} is not pinned by digest"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

"${runtime}" pull --platform "linux/${arch}" -q "${base}" > /dev/null \
    || die "cannot pull ${base} for linux/${arch}, the release may publish amd64 only"

# The release image is distroless and has no shell, so files come out through
# a created container rather than by running anything inside it.
extract() {
    local image="$1" path="$2" out="$3" cid rc=0

    cid="$("${runtime}" create --platform "linux/${arch}" "${image}")"
    "${runtime}" cp "${cid}:${path}" "${out}" 2> /dev/null || rc=1
    # Removed before the status is returned, so a missing file leaves nothing behind.
    "${runtime}" rm -f "${cid}" > /dev/null

    return "${rc}"
}

# The template and the formatter each end with a newline, so the blank last line
# has to go or it counts as a layer and shifts every comparison below.
layers() {
    "${runtime}" image inspect --format '{{range .RootFS.Layers}}{{println .}}{{end}}' "$1" | grep .
}

# The whole point of layering onto the release. A rebuilt operator would defeat
# it silently, since it would still load the plugin and still run.
extract "${base}" /baremetal-operator "${work}/published" \
    || die "no /baremetal-operator in ${base}"
extract "${img}" /baremetal-operator "${work}/built" \
    || die "no /baremetal-operator in ${img}"

cmp -s "${work}/published" "${work}/built" \
    || die "the operator binary in ${img} is not the one shipped in ${base}"

echo "operator binary is byte identical to ${base}"

# BMO's own plugins have to survive too, a base swapped underneath would show up
# here even when the operator binary happened to match.
for name in ironic demo; do
    extract "${base}" "/plugins/${name}-provisioner.so" "${work}/${name}-published.so" \
        || die "no ${name} plugin in ${base}"
    extract "${img}" "/plugins/${name}-provisioner.so" "${work}/${name}-built.so" \
        || die "the ${name} plugin ${base} ships is missing from ${img}"

    cmp -s "${work}/${name}-published.so" "${work}/${name}-built.so" \
        || die "the ${name} plugin in ${img} differs from ${base}"
done

echo "the ironic and demo plugins are unchanged"

# Ours is the only thing that may be added, and it has to actually be there.
extract "${img}" /plugins/anaconda-provisioner.so "${work}/anaconda.so" \
    || die "no anaconda plugin in ${img}"
[[ -s "${work}/anaconda.so" ]] || die "the anaconda plugin in ${img} is empty"

echo "anaconda plugin is present, $(wc -c < "${work}/anaconda.so") bytes"

# The one check nothing else can stand in for. A load tester built here agrees
# with a plugin built here, this is the binary that actually ships.
log="$(timeout 120 "${runtime}" run --rm --platform "linux/${arch}" "${img}" \
    --provisioner=anaconda 2>&1 || true)"

# It opens the plugin before any cluster IO, so no kubeconfig is needed and the
# failure that follows is the expected one.
if ! grep -q "loaded provisioner plugin" <<< "${log}"; then
    die "the released operator in ${img} did not load the plugin, $(grep -om1 'plugin was built with[^"\\]*' <<< "${log}")"
fi

echo "the released operator loads the plugin"

# Layer counts say whether anything else was added. Comparing the digests in
# order also catches a base rebuilt from the same source at a different time.
layers "${base}" > "${work}/base-layers"
layers "${img}" > "${work}/img-layers"

base_count="$(wc -l < "${work}/base-layers")"
img_count="$(wc -l < "${work}/img-layers")"

[[ "${img_count}" -eq "$((base_count + 1))" ]] \
    || die "${img} has ${img_count} layers, expected $((base_count + 1)) from ${base} plus the plugin"

head -n "${base_count}" "${work}/img-layers" | diff -q - "${work}/base-layers" > /dev/null \
    || die "${img} does not sit on the layers of ${base}"

echo "image is ${base_count} layers of ${base} plus one, ${img} verified"
