#!/usr/bin/env bash

# Create one BareMetalHost, writing nothing to disk. Apply deploy/anaconda.yaml
# first for the operator, then install with deploy/bmh-provision.sh.

# Lint with shellcheck -x deploy/bmh-create.sh
# (one line, "shellcheck" starting a comment makes it a directive)

set -o errexit -o nounset -o pipefail

# shellcheck source-path=SCRIPTDIR
source "$(dirname "${BASH_SOURCE[0]}")/bmh-env.sh"

# Applying over a host that reached provisioning is rejected, its boot MAC is
# immutable and this manifest declares none.
[[ -z "$(bmh_field '{.metadata.uid}')" ]] \
    || die "${BMH_NAME} already exists in ${NS}, run deploy/bmh-delete.sh first"

# A quoted heredoc, so the shell touches none of it. Unquoted, the backslash
# continuations in the callback curl would be eaten and the lines joined.
kubectl create secret generic "${KS_SECRET}" -n "${NS}" \
    --dry-run=client -o yaml --from-file=value=/dev/stdin << 'KSEOF' | kubectl apply -f -
text
eula --agreed
keyboard --vckeymap=us --xlayouts=us
lang en_US.UTF-8
timezone Etc/UTC --utc
network --bootproto=dhcp --activate --hostname={{ .Name }}
rootpw --plaintext toor
firewall --enabled --ssh
selinux --enforcing
firstboot --disable
ignoredisk --only-use={{ .InstallDisk }}
clearpart --all --initlabel --drives={{ .InstallDisk }}
autopart --type=lvm --nohome
# No reboot or poweroff, the provisioner takes the host down and boots the disk.

%packages
@^minimal-environment
openssh-server
curl
%end

%addon com_redhat_kdump --disable
%end

# Files that rpm scriptlets create are labeled by the installer's own policy,
# which is not always loaded, so the tree is labeled from the one just installed.
%post --nochroot --erroronfail --interpreter=/bin/bash
root=/mnt/sysroot
[[ -d ${root} ]] || root=/mnt/sysimage
setfiles -r "${root}" \
  "${root}/etc/selinux/targeted/contexts/files/file_contexts" "${root}"
%end

# Reporting in is what moves the host out of provisioning, so it has to be the
# last thing the install does. The provisioner takes the machine down on it.
%post --erroronfail --interpreter=/bin/bash
curl -fsS --retry 5 --retry-connrefused -X POST \
  -H "Content-Type: application/json" \
  -d '{"status":"installed","host":"{{ .Name }}"}' \
  "{{ .CallbackURL }}"
%end

%onerror --interpreter=/bin/bash
curl -fsS --retry 3 --retry-connrefused -X POST \
  -H "Content-Type: application/json" \
  -d '{"status":"failed","message":"anaconda hit a fatal installation error"}' \
  "{{ .CallbackURL }}"
%end
KSEOF

kubectl apply -f - << EOF
---
apiVersion: v1
kind: Secret
metadata:
  name: ${BMH_NAME}-bmc
  namespace: ${NS}
type: Opaque
stringData:
  username: ${BMC_USER}
  password: ${BMC_PASS}
---
apiVersion: metal3.io/v1alpha1
kind: BareMetalHost
metadata:
  name: ${BMH_NAME}
  namespace: ${NS}
spec:
  architecture: x86_64
  # Disabled until there is something on the disks worth erasing.
  # deploy/bmh-provision.sh switches it on when it patches the image.
  automatedCleaningMode: disabled
  bmc:
    address: "${BMC_ADDR}"
    credentialsName: ${BMH_NAME}-bmc
    disableCertificateVerification: true
  bootMode: UEFI
  online: true
  # BMO reads this Secret for networkData or value and fails registration when
  # it finds neither. The plugin reads the kickstart from the same value key.
  preprovisioningNetworkDataName: ${KS_SECRET}
  # No boot MAC, no root device hints and no image on purpose. Declaring either
  # of the first two skips inspection, and the image starts the install.
EOF

echo "install it with deploy/bmh-provision.sh once ${BMH_NAME} reaches available"
