# Build the out of tree anaconda provisioner plugin.
# It targets a pinned upstream BMO ref.

# BMO_VERSION and the toolchain image live in the Dockerfile and stay in sync
# via the retarget script. Change them there, not by overriding this.
BMO_VERSION ?= v0.14.0-rc.0
BMO_REPO ?= https://github.com/metal3-io/baremetal-operator.git
ARCH ?= amd64
CONTAINER_RUNTIME ?= docker

REGISTRY ?= localhost
IMAGE_NAME ?= baremetal-operator-oot
IMG_TAG ?= latest
IMG ?= $(REGISTRY)/$(IMAGE_NAME)-$(ARCH):$(IMG_TAG)

PLUGIN_SO ?= bin/anaconda-provisioner.so

.PHONY: all
all: image

# Point go.mod at BMO_VERSION and build the .so. This mutates go.mod, run
# "git checkout go.mod go.sum" to restore it. Only the image build loads.
.PHONY: plugin
plugin:
	./hack/retarget-bmo.sh $(BMO_VERSION) $(BMO_REPO)
	CGO_ENABLED=1 go build -buildmode=plugin -o $(PLUGIN_SO) ./plugin

# Compile the plugin against the committed module tags without fetching source.
.PHONY: compile-check
compile-check:
	CGO_ENABLED=1 go build -buildmode=plugin -o $(PLUGIN_SO) ./plugin

# Vet and run the suite. The tests stub every BMC over httptest, so this needs
# no network beyond the module downloads and touches no cluster.
.PHONY: test
test:
	go vet ./...
	go test ./...

# Build the operator image with the layered plugin. The build load tests the .so.
# BMO_VERSION must match the Dockerfile so the toolchain image stays consistent.
.PHONY: image
image:
	@test "$(ARCH)" = "amd64" \
		|| { echo "the published BMO image is amd64 only, ARCH=$(ARCH) has no base to layer onto" >&2; exit 1; }
	@test "$(BMO_VERSION)" = "$$(sed -n 's/^ARG BMO_VERSION=//p' Dockerfile)" \
		|| { echo "BMO_VERSION $(BMO_VERSION) does not match the Dockerfile, run hack/retarget-bmo.sh $(BMO_VERSION)" >&2; exit 1; }
	@test "$(BMO_VERSION)" = "$$(sed -n 's/^ARG BMO_IMAGE=[^:]*:\([^@]*\)@.*/\1/p' Dockerfile)" \
		|| { echo "the BMO_IMAGE tag does not match BMO_VERSION $(BMO_VERSION), run hack/retarget-bmo.sh $(BMO_VERSION)" >&2; exit 1; }
	$(CONTAINER_RUNTIME) build --platform=linux/$(ARCH) \
		--build-arg ARCH=$(ARCH) \
		--build-arg BMO_VERSION=$(BMO_VERSION) \
		--build-arg BMO_REPO=$(BMO_REPO) \
		-f Dockerfile . -t $(IMG)

# Prove the image is the published release plus one plugin. The load test gate
# says the .so loads, this says nothing underneath it was rebuilt or swapped.
.PHONY: verify-image
verify-image:
	ARCH=$(ARCH) CONTAINER_RUNTIME=$(CONTAINER_RUNTIME) ./hack/verify-image.sh $(IMG)

.PHONY: push
push:
	$(CONTAINER_RUNTIME) push $(IMG)

.PHONY: clean
clean:
	rm -rf bin .bmo-src
