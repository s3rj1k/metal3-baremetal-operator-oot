# Anaconda provisioner plugin (out-of-tree)

A BMO `-buildmode=plugin` `.so`. Boots an anaconda ISO as virtual media over
Redfish, serves the kickstart on `/ks/`, waits for `/callback/`. No Ironic.

Unsupported: disk images, cleaning, RAID, firmware settings, custom deploy.

```sh
make image                              # operator image with the plugin layered in
make compile-check                      # compile only, not loadable
kubectl apply -f deploy/anaconda.yaml   # operator, kickstart, caddy, ISO builder
deploy/bmh-create.sh                    # create one host and let it inspect
deploy/bmh-provision.sh                 # patch in MAC, disk and image, then watch
deploy/bmh-delete.sh                    # remove the host and everything with it
hack/retarget-bmo.sh <ref> [repo-url]   # repin BMO, undo with git checkout
```

## BareMetalHost

| Field | Value |
|---|---|
| `spec.bmc.address` | `redfish-virtualmedia://host[/redfish/v1/Systems/<id>]`, `+http` or `+https` to fix the scheme |
| `spec.bootMACAddress` | Required, the only thing `/ks/` matches on |
| `spec.image.format` | `live-iso` |
| `spec.preprovisioningNetworkDataName` | Secret holding the kickstart under key `value` |
| `spec.rootDeviceHints` | Install disk, `deviceName`/`wwn`/`serialNumber` only, else `ANACONDA_INSTALL_DISK` |

Inspection prefers the `HardwareData` CR, then skips the BMC entirely when the
boot MAC and root device hints are both already set, else collects a Redfish
inventory. Re-inspect by deleting the CR *and* annotating
`inspect.metal3.io: refresh`.

## Kickstart

Rendered with `missingkey=error`, vars `.Name`, `.Namespace`, `.UID`,
`.BootMAC`, `.CallbackURL`, `.InstallDisk`. Anything unresolvable gets a
compiled in fallback that powers the machine off.

POST `.CallbackURL` empty or `{"status":"installed"}` to finish, which ejects
the media (`ok`, `success` and `complete` also pass). Any other status fails the
host with its `message`, silence fails it after `ANACONDA_INSTALL_TIMEOUT`. The
verdict lands on `anaconda.metal3.io/install-result` and `…/install-message`.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ANACONDA_LISTEN_ADDR` | unset | Starts the listener, unset disables `/ks` and `/callback` |
| `ANACONDA_BASE_URL` | unset | Externally reachable base for `.CallbackURL` |
| `ANACONDA_INSTALL_TIMEOUT` | `60m` | Wait for the callback before failing the host |
| `ANACONDA_INSTALL_DISK` | unset | Fleet wide install disk, for hosts whose hints name none |

## Security

The provisioning network is the trust boundary: plain HTTP listener, spoofable
`X-RHN-Provisioning-MAC-*` headers, unauthenticated callback guarded only by the
host UID, BMC certificates unverified, and a BMC error echoing the password back
would be logged.
