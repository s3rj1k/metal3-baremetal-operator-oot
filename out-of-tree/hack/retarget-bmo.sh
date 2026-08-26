#!/usr/bin/env bash

# Configure the module to build the anaconda plugin against a BMO git ref.
# It fetches BMO at the ref and pins go.mod, the Dockerfile, and the Makefile.

set -o errexit -o nounset -o pipefail

ref="${1:?usage: retarget-bmo.sh <bmo-git-ref> [bmo-repo-url] [bmo-src-dir]}"
repo="${2:-https://github.com/metal3-io/baremetal-operator.git}"
src="${3:-}"

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

# Only a host retarget rewrites the build files. Inside the image build a source
# dir is passed and the Dockerfile already has its images fixed for that build.
if [[ "${cloned}" == true ]]; then
    dockerfile="${module_dir}/Dockerfile"
    makefile="${module_dir}/Makefile"

    # Match the toolchain and base images the release used. Go plugin loading
    # needs the exact same Go version to load into the official image.
    for arg in BUILD_IMAGE BASE_IMAGE; do
        line="$(grep -m1 "^ARG ${arg}=" "${src}/Dockerfile" || true)"
        if [[ -n "${line}" ]]; then
            sed -i "s|^ARG ${arg}=.*|${line}|" "${dockerfile}"
        fi
    done
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
