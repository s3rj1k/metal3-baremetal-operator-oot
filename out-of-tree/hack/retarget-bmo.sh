#!/usr/bin/env bash

# Configure the module to build the anaconda plugin against a BMO git ref.
# It fetches BMO at the ref and pins go.mod, the Dockerfile, the Makefile and
# the GitRepository in every deploy manifest.

set -o errexit -o nounset -o pipefail

ref="${1:?usage: retarget-bmo.sh <bmo-git-ref> [bmo-repo-url] [bmo-src-dir]}"
repo="${2:-https://github.com/metal3-io/baremetal-operator.git}"
src="${3:-}"

# The operator binary is no longer built here, it comes from the published
# release image, so a ref without one cannot be built at all.
image_repo="${BMO_IMAGE_REPO:-quay.io/metal3-io/baremetal-operator}"

# Print the published image pinned by digest, or fail when there is none.
# Pinning the digest keeps the base immutable the way BUILD_IMAGE is.
resolve_bmo_image() {
    local tag="$1"
    local host="${image_repo%%/*}"
    local path="${image_repo#*/}"
    local url="https://${host}/v2/${path}/manifests/${tag}"
    local accept='Accept: application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json'
    local challenge realm service token response status digest
    local auth=()

    # Take the token realm from the registry's own challenge rather than
    # hardcoding quay, so a mirror or a different registry still resolves.
    challenge="$(curl -sI --max-time 20 -H "${accept}" "${url}" | tr -d '\r' \
        | sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: Bearer //p')"

    if [[ -n "${challenge}" ]]; then
        realm="$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<< "${challenge}")"
        service="$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<< "${challenge}")"

        if [[ -n "${realm}" ]]; then
            token="$(curl -s --max-time 20 "${realm}?service=${service}&scope=repository:${path}:pull" \
                | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
            [[ -n "${token}" ]] && auth=(-H "Authorization: Bearer ${token}")
        fi
    fi

    response="$(curl -sI --max-time 20 "${auth[@]}" -H "${accept}" "${url}" | tr -d '\r')"
    status="$(awk 'NR == 1 { print $2 }' <<< "${response}")"
    digest="$(sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: //p' <<< "${response}")"

    # A 404 is a ref with no release image, anything else means the registry
    # never answered, and sending the user to change a good ref would be wrong.
    [[ -n "${status}" ]] || return 2
    [[ "${status}" != "404" ]] || return 1
    [[ "${status}" == "200" && -n "${digest}" ]] || return 2

    printf '%s:%s@%s\n' "${image_repo}" "${tag}" "${digest}"
}

# Resolve before the clone, so a ref with no image fails in seconds rather than
# after a full checkout. A source dir is the image build, base already fixed.
bmo_image=""
if [[ -z "${src}" ]]; then
    rc=0
    bmo_image="$(resolve_bmo_image "${ref}")" || rc=$?

    case "${rc}" in
        0) ;;
        1)
            echo "error: no published image ${image_repo}:${ref}, retarget to a release tag" >&2
            exit 1
            ;;
        *)
            echo "error: could not reach ${image_repo} to resolve ${ref}, the ref may be fine" >&2
            exit 1
            ;;
    esac
fi

module_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${module_dir}"

cloned=false
if [[ -z "${src}" ]]; then
    cloned=true
    src="${module_dir}/.bmo-src"
    rm -rf "${src}"
    echo "cloning ${repo} at ${ref} into ${src}"
    if ! git clone --depth 1 --branch "${ref}" "${repo}" "${src}" 2>/dev/null; then
        git clone "${repo}" "${src}"
        git -C "${src}" checkout "${ref}"
    fi
fi

if [[ ! -f "${src}/go.mod" ]]; then
    echo "error: ${src} is not a BMO checkout (no go.mod)" >&2
    exit 1
fi

bmo="github.com/metal3-io/baremetal-operator"
mods=("${bmo}" "${bmo}/apis" "${bmo}/pkg/hardwareutils")

echo "pinning go.mod to BMO ${ref} via ${src}"

if [[ "${cloned}" == true ]]; then
    # Cloned here, so record what go.mod has to name on its own. A replace to a
    # path inside this checkout would leave a go.mod nobody else can build.
    version="${ref}"
    if [[ ! "${ref}" =~ ^v[0-9] ]]; then
        version="$(git -C "${src}" rev-parse HEAD)"
    fi

    for mod in "${mods[@]}"; do
        go get "${mod}@${version}"
    done
else
    # The image build copies BMO in, so the replace is what the plugin compiles
    # against and the require version only has to parse.
    if [[ "${ref}" =~ ^v[0-9] ]]; then
        for mod in "${mods[@]}"; do
            go mod edit -require "${mod}@${ref}"
        done
    fi

    go mod edit \
        -replace "${bmo}=${src}" \
        -replace "${bmo}/apis=${src}/apis" \
        -replace "${bmo}/pkg/hardwareutils=${src}/pkg/hardwareutils"
fi

# go list refuses to run against a go.mod the edits above left untidy, so this
# has to come first or the pinning below reads nothing and silently skips.
go mod tidy

# Pin each plugin owned direct dep to its go.mod version, never @latest, so retargets
# stay reproducible. Deps BMO already provides are skipped so they track the BMO ref.
deps="$(go list -m -f '{{if and (not .Main) (not .Indirect)}}{{.Path}} {{.Version}}{{end}}' all)"

while read -r path version; do
    [[ -z "${path}" || -z "${version}" ]] && continue
    case "${path}" in "${bmo}" | "${bmo}/"*) continue ;; esac
    if awk -v p="${path}" '$1 == p { found = 1 } END { exit !found }' "${src}/go.mod"; then
        continue
    fi
    go get "${path}@${version}"
done <<< "${deps}"

go mod tidy

# Drop the overlap pins a previous run wrote, self replaces rather than the BMO
# ones. Tidy never removes them, so a dropped dep would keep a forced version.
stale="$(awk '
    /^replace \(/ { block = 1; next }
    block && /^\)/ { block = 0; next }
    block && NF >= 4 && $1 == $3 { print $1; next }
    /^replace / && NF >= 5 && $2 == $4 { print $2 }
' go.mod)"

while read -r path; do
    [[ -n "${path}" ]] || continue
    go mod edit -dropreplace "${path}"
done <<< "${stale}"

go mod tidy

# Every module the operator also links has to resolve to the same version or
# plugin.Open rejects the .so, so BMO's graph wins wherever the two overlap.
# Captured first, a process substitution would hide a failed go list and leave
# the pinning silently skipped, which is the mismatch this exists to prevent.
graph_format='{{if not .Main}}{{.Path}} {{.Version}}{{end}}'
our_graph="$(go list -m -f "${graph_format}" all)"
bmo_graph="$(cd "${src}" && go list -m -f "${graph_format}" all)"

declare -A resolved

while read -r path version; do
    if [[ -n "${path}" && -n "${version}" ]]; then
        resolved["${path}"]="${version}"
    fi
done <<< "${our_graph}"

# A require would lose to MVS, which is what raised these above BMO in the first
# place, so each overlap is written as a replace that no upgrade can outvote.
while read -r path version; do
    [[ -z "${path}" || -z "${version}" ]] && continue
    case "${path}" in "${bmo}" | "${bmo}/"*) continue ;; esac
    [[ -n "${resolved["${path}"]:-}" && "${resolved["${path}"]}" != "${version}" ]] || continue

    echo "pinning ${path} to ${version}, the version BMO resolves"
    go mod edit -replace "${path}=${path}@${version}"
done <<< "${bmo_graph}"

go mod tidy

# Only a host retarget rewrites the build files. Inside the image build a source
# dir is passed and the Dockerfile already has its images fixed for that build.
if [[ "${cloned}" == true ]]; then
    dockerfile="${module_dir}/Dockerfile"
    makefile="${module_dir}/Makefile"

    # Match the toolchain the release used. A Go plugin only loads into a host
    # built by the identical Go version, so this cannot be allowed to drift.
    line="$(grep -m1 "^ARG BUILD_IMAGE=" "${src}/Dockerfile" || true)"
    if [[ -n "${line}" ]]; then
        sed -i "s|^ARG BUILD_IMAGE=.*|${line}|" "${dockerfile}"
    fi

    echo "pinning the runtime base to ${bmo_image}"
    sed -i "s|^ARG BMO_IMAGE=.*|ARG BMO_IMAGE=${bmo_image}|" "${dockerfile}"
    sed -i "s|^ARG BMO_VERSION=.*|ARG BMO_VERSION=${ref}|" "${dockerfile}"
    sed -i "s|^BMO_VERSION[[:space:]]*[?]=.*|BMO_VERSION ?= ${ref}|" "${makefile}"

    # Flux has to read config/base at the commit the plugin is compiled against,
    # and a branch or tag would drift from the module pin, so record the SHA.
    commit="$(git -C "${src}" rev-parse HEAD)"
    manifest_url="${repo%.git}"

    for manifest in "${module_dir}"/deploy/*.yaml; do
        [[ -f "${manifest}" ]] || continue
        grep -q '^kind: GitRepository$' "${manifest}" || continue

        # A manifest pinned by branch or tag has no line to rewrite, and silently
        # leaving it on the old ref is how a stale config/base reaches a cluster.
        if ! grep -q '^    commit: ' "${manifest}"; then
            echo "warning: $(basename "${manifest}") pins its GitRepository by something other than a commit, leaving it alone" >&2
            continue
        fi

        sed -i \
            -e "s|^  url: .*|  url: ${manifest_url}|" \
            -e "s|^    commit: .*|    commit: ${commit}|" \
            "${manifest}"

        echo "pinning $(basename "${manifest}") to BMO ${commit}"
    done
fi

echo "done. build with: make plugin  or  make image"
