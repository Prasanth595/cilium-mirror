# spire-crds

ClusterSPIFFEID and ClusterStaticEntry CRDs for the Cilium SPIRE integration

## Source Code

* <https://github.com/spiffe/spire-controller-manager>

## Requirements

Kubernetes: `>= 1.21.0-0`

Helm installs these CRDs only on initial installation. Existing CRDs are not
adopted or upgraded, and uninstall retains the CRDs and their instances.
