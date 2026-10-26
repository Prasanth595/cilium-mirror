.. only:: not (epub or latex or html)

    WARNING: You are looking at unreleased Cilium documentation.
    Please use the official rendered version released here:
    https://docs.cilium.io

.. _encryption_ztunnel:

*************************************
Ztunnel Transparent Encryption (Beta)
*************************************

.. include:: /beta.rst

This guide explains how to configure Cilium to use ztunnel for transparent
encryption and mutual TLS (mTLS) authentication between Cilium-managed endpoints.
ztunnel is a purpose-built per-node proxy that provides transparent Layer 4 mTLS
encryption and authentication for pod-to-pod communication.

When ztunnel is enabled in Cilium, the agent running on each cluster node
establishes a control plane connection with the local ztunnel proxy. Cilium
enrolls pods into the mesh on a per-namespace basis, allowing fine-grained
control over which workloads participate in mTLS encryption. Enrolled pods have
their traffic transparently redirected to the ztunnel proxy using iptables rules
configured in their network namespace, where the traffic is encrypted and
authenticated using mutual TLS before being sent to the destination.


Generating secrets for authentication
=====================================

Cilium's ztunnel integration requires a set of private keys and accompanying
to certificates be present via Kubernetes secrets. This follows the same pattern
as IPsec key injections.

These keys can be generated with the following bash script prior to deploying
Cilium.

.. literalinclude:: ../../../examples/kubernetes-ztunnel/generate-secrets.sh
   :language: bash

The 'bootstrap' keys are used to secure the connection between ztunnel and
Cilium's xDS and certificate server implementation.

The 'ca' keys are used as the root certificate for creating in-memory and
ephemeral client certificates on ztunnel's request.

Enable ztunnel in Cilium
========================

Before you install Cilium with ztunnel enabled, ensure that:

* The necessary Kubernetes secrets are available.
* Cluster Mesh is not enabled (ztunnel is currently not compatible with Cluster Mesh).

.. tabs::

    .. group-tab:: Cilium CLI

       If you are deploying Cilium with the Cilium CLI, pass the following
       options:

       .. parsed-literal::

          cilium install --version |CHART_VERSION| \\
             --set encryption.enabled=true \\
             --set encryption.type=ztunnel

    .. group-tab:: Helm

       If you are deploying Cilium with Helm by following
       :ref:`k8s_install_helm`, pass the following options:

       .. parsed-literal::

           helm install cilium |CHART_RELEASE| \\
             --namespace kube-system \\
             --set encryption.enabled=true \\
             --set encryption.type=ztunnel


.. _ztunnel_spire_installation:

Chart-managed SPIRE
===================

Set ``encryption.ztunnel.ca.type=spire``,
``authentication.mutual.spire.enabled=true`` and
``encryption.ztunnel.spire.useCRD=true`` to use SPIRE for ztunnel workload
certificates. The chart installs SPIRE when
``authentication.mutual.spire.install.enabled=true``. The default installation
still uses a persistent SQLite datastore, the disk KeyManager, and the existing
bootstrap sidecar.

Without ``encryption.ztunnel.spire.useCRD=true``, the chart neither wires
ztunnel to SPIRE nor authorizes it as a SPIRE agent delegate, so ztunnel keeps
obtaining certificates from the Cilium agent's built-in CA unless it is wired
to an external SPIRE installation manually. The Cilium agent leaves the xDS
workload trust domain empty, so ztunnel uses its default trust domain, and the
operator registers workloads through the SPIRE Entry API.

.. warning::

   Enabling ``encryption.ztunnel.spire.useCRD=true`` moves ztunnel workload
   identities from ztunnel's default trust domain to
   ``authentication.mutual.spire.trustDomain``. Until all Cilium agent and
   ztunnel pods have rolled out, nodes can disagree on peer identities, and
   cross-node traffic between enrolled workloads can fail. Plan the switch as
   a maintenance operation.

For a new bundled installation, ``encryption.ztunnel.spire.useCRD=true`` is
the only feature control. CRD installation, the controller, and bootstrap
registration are derived from it. The existing SPIRE and ztunnel
prerequisites still apply.

With CRD registration, the chart renders one ``ClusterSPIFFEID`` that selects
namespaces labeled ``io.cilium/mtls-enabled=true``, the label that enrolls a
namespace in ztunnel. ``spire-controller-manager`` registers every pod in those
namespaces with the SPIFFE ID
``spiffe://<trust domain>/ns/<namespace>/sa/<service account>``, parented to
the SPIRE agent on the pod's node. ztunnel requests each workload certificate
through the SPIRE agent's delegated identity API, which attests the pod by
process ID. The operator does not register workloads in this mode.

The SPIRE server pod runs a colocated ``spire-controller-manager`` that
reconciles the ``ClusterSPIFFEID`` and ``ClusterStaticEntry`` resources into
the server over a pod-local private API socket. The chart also supplies
bootstrap ``ClusterStaticEntry`` resources for the SPIRE agent node alias,
Cilium agent, Cilium operator, and ztunnel. The ztunnel entry provides the
identity that the SPIRE agent authorizes as a delegate. Keeping the Cilium
identities preserves other consumers of the SPIRE integration.

The controller watches pods, nodes, namespaces and endpoints in the whole
cluster and keeps one entry per selected pod in the server. It ignores the
``kube-system``, ``kube-public`` and ``spire-system`` namespaces, so do not
enroll those namespaces in CRD mode: their pods get no SPIRE entries, and
ztunnel cannot obtain certificates for them.

The controller does not require the ``ClusterFederatedTrustDomain`` CRD. Admission
webhooks are disabled with ``ENABLE_WEBHOOKS=false``; no webhook resources or
webhook RBAC are installed. Kubernetes still enforces the OpenAPI schema of
each bundled CRD. Controller logs and resource status report semantic
registration failures that schema validation cannot detect.

With chart-managed SPIRE and CRD registration, the chart wires ztunnel to the
local SPIRE delegated identity socket and the containerd CRI socket. Override
``encryption.ztunnel.spire.containerRuntimeSocketPath`` if containerd uses a
different path. A read-only socket mount still grants privileged access to the
runtime API; it is not a read-only API authorization boundary. The configured
SPIRE trust domain is used consistently in registration, xDS workload
identities, and ztunnel's SPIRE bundle lookup.
Do not duplicate the managed socket mount paths in
``encryption.ztunnel.extraVolumeMounts``, even under different volume names.

In CRD mode, the server keeps its SQLite database and disk KeyManager state on
a persistent volume by default. With ``server.dataStorage.enabled=false``, it
keeps them in pod-local ``emptyDir`` storage instead, so a replaced server pod
starts with a new trust root. The private API socket is pod-local ``emptyDir``
storage in either case. CRD-managed server pods default to UID/GID and
``fsGroup`` 1000; ``server.podSecurityContext`` overrides these defaults.
``server.securityContext`` applies to both containers so their socket
permissions remain compatible. Keep a suitable ``fsGroup`` when changing UIDs
so the data volume remains accessible.

CRD ownership and upgrades
--------------------------

The ``spire-crds`` subchart bundles the upstream controller-manager
v0.6.2 ``ClusterSPIFFEID`` and ``ClusterStaticEntry`` CRDs. These CRDs and
the controller image must remain compatible. SPIRE server and agent images
remain pinned to 1.15.5.

By default, the dependency follows ``encryption.ztunnel.spire.useCRD``.
``authentication.mutual.spire.install.crds.enabled`` is an optional boolean
override: ``false`` opts out for an externally managed schema, and ``true``
explicitly enables schema installation even without bundled SPIRE. Leave the
override absent for automatic behavior. This dependency is evaluated before
Helm renders templates, so it also applies when SPIRE itself is external.

Helm installs files under ``crds/`` only during an initial install. It skips
existing CRDs without adopting or updating them, and does not delete them on
uninstall. Enabling the dependency while upgrading an existing Cilium release
does **not** install missing CRDs. Install them first, for example from this
checkout:

.. code-block:: shell-session

    helm install cilium-spire-crds ./install/kubernetes/cilium/charts/spire-crds \
      --namespace kube-system

Then upgrade Cilium with ``authentication.mutual.spire.install.crds.enabled=false``
and CRD registration enabled. If another installation owns the CRDs, retain that
ownership and arrange compatible schema upgrades with its owner instead. Do
not delete a CRD to resolve an ownership or version mismatch: deletion removes
all instances, including workload registration intent.

Offline rendering against existing CRDs requires
``--api-versions spire.spiffe.io/v1alpha1/ClusterSPIFFEID`` and, with
chart-managed SPIRE, ``--api-versions spire.spiffe.io/v1alpha1/ClusterStaticEntry``.
This option only supplies rendering capabilities; it is not evidence that a
cluster has the CRDs.

The ``ClusterSPIFFEID`` and bootstrap resources belong to this Helm release and
use release-qualified names. They set ``spec.className`` to
``encryption.ztunnel.spire.className``, ``cilium-spire`` by default. The
chart's controller reconciles only resources of that class, or only resources
without a class when the value is empty, and manages only SPIRE entry IDs
prefixed with ``cilium.``. It does not adopt or garbage-collect existing
direct-API registrations. Use distinct class names for controllers of
unrelated trust domains in the same cluster.

When RBAC or ServiceAccounts are managed externally, supply the equivalent
permissions and accounts yourself. The server needs PSAT token reviews, pod
and node reads, and bundle ConfigMap access. With CRD registration, the
controller also needs get/list/watch on ``ClusterSPIFFEID`` and
``ClusterStaticEntry`` resources and on pods, nodes, namespaces and endpoints,
plus get/patch/update on the status of both resource types. The chart grants
these through a separate ``<server ServiceAccount>-controller-manager``
ClusterRole and ClusterRoleBinding that exist only with CRD registration,
``rbac.create=true`` and ``server.serviceAccount.create=true``. The operator
needs no SPIRE CRD permissions.
For compatibility with existing bundled installations, the bundle ConfigMap
RoleBinding remains chart-managed regardless of ``rbac.create`` or
``server.serviceAccount.create``. Its Role is chart-managed when
``server.serviceAccount.create=true``, independently of ``rbac.create``.
The server ClusterRoleBinding and bundle RoleBinding reference a ClusterRole
and Role named ``spire-server``, while the chart names those roles after
``server.serviceAccount.name``. Keep the default ``spire-server``
ServiceAccount name, or supply roles with the referenced names yourself.

Migration and verification
--------------------------

Treat migration from the legacy installation as a planned maintenance
operation:

* Preserve and back up the existing trust root and registration intent before
  changing producers or storage. Moving to an unrelated root requires a
  coordinated trust migration and certificate refresh.
* Install the CRDs before enabling CRD registration on an existing release.
  Explicitly migrate or retire the Entry API registrations that the operator
  created; the controller deliberately leaves unprefixed entries alone.
* Enabling CRD registration moves workload identities to the configured
  trust domain. Expect cross-node traffic between enrolled workloads to fail
  until every Cilium agent and ztunnel pod has rolled out.

Before relying on CRD registration, verify registration entries on the server,
agent attestation, and cross-node mTLS in an isolated test environment. Pod
readiness does not prove that registration has converged: controller v0.6.2's
readiness endpoint checks controller health, not successful reconciliation of
every entry.

External SPIRE
--------------

With ``authentication.mutual.spire.install.enabled=false``, the chart does not
install servers, agents, controllers, or bootstrap registrations.
``encryption.ztunnel.spire.useCRD=true`` still renders the ztunnel
``ClusterSPIFFEID``, sets the xDS workload trust domain, stops the operator's
Entry API registration, and enables the CRD dependency.
Explicitly set ``authentication.mutual.spire.install.crds.enabled=false`` when
CRD installation is also managed externally. Existing CRDs are never adopted,
updated, or deleted by Helm, regardless of this override.

The external installation must provide a ``spire-controller-manager`` that
reconciles ``ClusterSPIFFEID`` resources of class
``encryption.ztunnel.spire.className`` (set it to ``""`` for a controller
without a class name), PSAT and bootstrap registration, delegated identity
authorization for ztunnel, shared trust and appropriate routing. Wire ztunnel
to the external SPIRE agent with ``encryption.ztunnel.extraEnv``,
``extraVolumes`` and ``extraVolumeMounts``. When moving to chart-managed
SPIRE, remove manually supplied ``SPIRE_ENABLED``,
``SPIRE_ADMIN_ENDPOINT_SOCKET``, ``CONTAINER_RUNTIME_SOCK_PATH``,
``TRUST_DOMAIN`` and ``CLUSTER_DOMAIN`` variables and the reserved
``spire-agent-socket`` and ``container-runtime`` volumes. With CRD
registration, the chart supplies these from its SPIRE values and rejects
duplicates.

Enrolling Namespaces
====================

After enabling ztunnel in Cilium, you need to explicitly enroll namespaces to
enable mTLS encryption for their workloads. This is done by applying a label
to the namespace:

.. code-block:: shell-session

    kubectl label namespace <namespace-name> io.cilium/mtls-enabled=true

To verify that a namespace is enrolled:

.. code-block:: shell-session

    kubectl get namespace <namespace-name> --show-labels

When a namespace is enrolled:

* All existing pods in the namespace (except ztunnel pods themselves) are enrolled
* Iptables rules are configured in each pod's network namespace for traffic redirection
* Pod metadata is sent to the ztunnel proxy via the ZDS protocol
* Future pods created in the namespace are automatically enrolled

To disenroll a namespace:

.. code-block:: shell-session

    kubectl label namespace <namespace-name> io.cilium/mtls-enabled-

This will:

#. Disenroll all pods in the namespace from ztunnel
#. Remove the iptables rules from each pod's network namespace
#. Notify ztunnel to stop processing traffic for those workloads

When ``encryption.ztunnel.spire.useCRD=true``, the chart's ``ClusterSPIFFEID``
selects namespaces with this label. ``spire-controller-manager`` registers
each pod in an enrolled namespace and removes those entries when the label is
removed. Namespaces that the controller ignores, ``kube-system``,
``kube-public`` and ``spire-system``, cannot be enrolled in this mode.

Validate the Setup
==================

#. Check that ztunnel has been enabled:

   .. code-block:: shell-session

      kubectl -n kube-system describe cm cilium-config | grep enable-ztunnel -A2

   You should see output indicating that ztunnel encryption is enabled.

#. Check which namespaces are enrolled:

   .. code-block:: shell-session

      kubectl get namespaces -l io.cilium/mtls-enabled=true

   This shows all namespaces labeled for ztunnel enrollment.

   To verify that these namespaces are actually enrolled in the StateDB table:

   .. code-block:: shell-session

      kubectl exec -n kube-system ds/cilium -- cilium-dbg statedb dump | jq '.["mtls-enrolled-namespaces"]'

   The results of this query should show which namespaces have been successfully
   processed by the enrollment reconciler.

#. Run a ``bash`` shell in one of the Cilium pods hosting a mtls-enrolled pod with
   ``kubectl -n kube-system exec -ti pod/<cilium-pod-hosting-mtls-pod> -- bash``
   and execute the following commands:

   Install tcpdump

   .. code-block:: shell-session

       $ apt-get update
       $ apt-get -y install tcpdump

   Check that traffic is encrypted. In the example below, this can be verified
   by the fact that packets will have a destination port of 15008 (HBONE).
   In the example below, ``eth0`` is the interface used for pod-to-pod
   communication. Replace this interface with e.g. ``cilium_vxlan`` if
   tunneling is enabled.

   .. code-block:: shell-session

       tcpdump -i eth0 port 15008
       tcpdump: verbose output suppressed, use -v or -vv for full protocol decode
       listening on eth0, link-type EN10MB (Ethernet), snapshot length 262144 bytes
       13:00:06.982499 IP 10.244.1.95.15008 > 10.244.2.3.33446: ...
       13:00:06.982536 IP 10.244.2.3.33446 > 10.244.1.95.15008: ...
       13:00:06.982675 IP 10.244.2.3.33446 > 10.244.1.95.15008: ...


Limitations
===========

* Traffic between workloads is only supported when both the source and
  destination endpoints are enrolled in ztunnel. Communication between an
  enrolled workload and a non-enrolled workload is not supported.

* The ztunnel integration currently only supports enrollment via namespace
  labels. Pod-level enrollment is not supported.

* Only TCP traffic is currently supported for mTLS encryption. UDP and other
  protocols are not redirected to ztunnel.

* The integration requires iptables support in the kernel and cannot be used
  with environments that do not support iptables (such as some minimal container
  runtimes).

* Ztunnel interferes with Cilium network policy as traffic is encrypted before
  it leaves the pod, meaning L4 policies won't work except for directly
  targeting the ztunnel HBONE port (15008).

Known Issues
============

* Cluster Mesh is not currently supported when ztunnel is enabled. Attempting
  to enable both will result in a validation error.

* Pods without a network namespace path (such as host-networked pods) cannot
  be enrolled in ztunnel and will be skipped during enrollment.
