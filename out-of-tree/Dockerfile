# Out of tree anaconda provisioner plugin image.
# Layers the plugin onto the published BMO release image.

ARG BUILD_IMAGE=docker.io/golang:1.26.5@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647
ARG BMO_IMAGE=quay.io/metal3-io/baremetal-operator:v0.14.0-rc.0@sha256:204e3258a0138aaaf23476ac9dbc70608a9dc07542ca07c71c9026648c1196e9
ARG BMO_VERSION=v0.14.0-rc.0
ARG BMO_REPO=https://github.com/metal3-io/baremetal-operator.git

# Fetch the pinned BMO source once. The operator itself is not built here, but
# the plugin has to compile against this tree to resolve the same module graph.
# /workspace is not a preference. Go bakes the source path into every package
# hash, and the release was built there, so any other directory fails to load.
FROM $BUILD_IMAGE AS bmo-src
ARG BMO_VERSION
ARG BMO_REPO
RUN git clone --depth 1 --branch ${BMO_VERSION} ${BMO_REPO} /workspace 2>/dev/null \
 || (git clone ${BMO_REPO} /workspace && git -C /workspace checkout ${BMO_VERSION})

# Build the plugin against the pinned BMO source. The replaces make it inherit
# BMO's exact module resolution and add only the gofish dependency.
FROM bmo-src AS plugin
ARG ARCH=amd64
ARG LDFLAGS=-s -w
ARG BMO_VERSION
ARG BMO_REPO
ENV CGO_ENABLED=1
ENV GO111MODULE=on
WORKDIR /plugin
COPY . .
RUN bash ./hack/retarget-bmo.sh "${BMO_VERSION}" "${BMO_REPO}" /workspace \
 && GOOS=linux GOARCH=${ARCH} \
    go build -buildmode=plugin -ldflags "${LDFLAGS}" -o /out/anaconda-provisioner.so ./plugin

# BMO's own load tester, built from the same checkout and toolchain so it holds
# the plugin to the module graph the release image was built with.
RUN cd /workspace && GOOS=linux GOARCH=${ARCH} \
    go build -a -ldflags "${LDFLAGS}" -o /out/plugin-load-test ./cmd/plugin-load-test

# Fail the build unless the .so loads. This is the contract and toolchain gate.
# The final image copies from here so it always runs.
FROM $BMO_IMAGE AS verify
COPY --from=plugin /out/plugin-load-test /plugin-load-test
COPY --from=plugin /out/anaconda-provisioner.so /plugins/anaconda-provisioner.so

# BMO's own plugin, built by their CI against the release binary. Our loader
# opening it is what proves this build agrees with the image it layers onto.
# Exec form, the release image is distroless and has no shell to parse a string.
RUN ["/plugin-load-test", "/plugins/demo-provisioner.so", "demo"]

# Only meaningful once the line above passes, otherwise both sides are ours and
# agree with each other while the release binary rejects them both.
RUN ["/plugin-load-test", "/plugins/anaconda-provisioner.so", "anaconda"]

# Runtime image. The published operator with the anaconda plugin added beside
# the ironic and demo plugins it already ships.
FROM $BMO_IMAGE
COPY --from=verify /plugins/anaconda-provisioner.so /plugins/anaconda-provisioner.so
# ENTRYPOINT, WORKDIR and USER come from the release image. Only the default
# provisioner is ours, and the Deployment passes the flag explicitly anyway.
CMD ["--provisioner", "anaconda"]
