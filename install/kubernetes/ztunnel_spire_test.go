// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kubernetes_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	ztunnelSpireValues = "encryption.enabled=true,encryption.type=ztunnel,encryption.ztunnel.ca.type=spire,authentication.mutual.spire.enabled=true,authentication.mutual.spire.install.enabled=false"
	ztunnelSpireCRD    = "encryption.ztunnel.spire.useCRD=true"
	ztunnelSpireFlag   = "--enable-ztunnel-spiffeid-crd"
)

func renderZtunnelSpire(t *testing.T, overrides ...string) (map[string]string, error) {
	t.Helper()
	return renderSpireChart(t, spireRenderOptions{}, overrides...)
}

func TestZtunnelSpireCRDRendering(t *testing.T) {
	tests := []struct {
		name       string
		values     []string
		wantCRD    bool
		noOperator bool
		noRole     bool
		wantSPIRE  bool
		extraArgs  []string
	}{
		{name: "defaults"},
		{
			name:   "internal CA",
			values: []string{"encryption.enabled=true,encryption.type=ztunnel"},
		},
		{
			name:   "Entry API external SPIRE default",
			values: []string{ztunnelSpireValues},
		},
		{
			name:   "Entry API external SPIRE explicit",
			values: []string{ztunnelSpireValues, "encryption.ztunnel.spire.useCRD=false"},
		},
		{
			name:      "Entry API chart-managed SPIRE",
			values:    []string{ztunnelSpireValues, "authentication.mutual.spire.install.enabled=true"},
			wantSPIRE: true,
		},
		{
			name:    "CRD external SPIRE",
			values:  []string{ztunnelSpireValues, ztunnelSpireCRD},
			wantCRD: true,
		},
		{
			name:      "CRD chart-managed SPIRE",
			values:    []string{ztunnelSpireValues, ztunnelSpireCRD, "authentication.mutual.spire.install.enabled=true"},
			wantCRD:   true,
			wantSPIRE: true,
		},
		{
			name:       "operator disabled",
			values:     []string{ztunnelSpireValues, ztunnelSpireCRD, "operator.enabled=false"},
			wantCRD:    true,
			noOperator: true,
			noRole:     true,
		},
		{
			name:    "externally managed RBAC",
			values:  []string{ztunnelSpireValues, ztunnelSpireCRD, "rbac.create=false"},
			wantCRD: true,
			noRole:  true,
		},
		{
			name:    "externally managed operator ServiceAccount",
			values:  []string{ztunnelSpireValues, ztunnelSpireCRD, "serviceAccounts.operator.create=false"},
			wantCRD: true,
			noRole:  true,
		},
		{
			name:      "unrelated Entry API arguments",
			values:    []string{ztunnelSpireValues},
			extraArgs: []string{"--log-level=debug", "--leader-election-lease-duration=30s"},
		},
		{
			name:      "unrelated CRD arguments",
			values:    []string{ztunnelSpireValues, ztunnelSpireCRD},
			wantCRD:   true,
			extraArgs: []string{"--log-level=debug", "--leader-election-lease-duration=30s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := slices.Clone(tt.values)
			for i, arg := range tt.extraArgs {
				values = append(values, fmt.Sprintf("operator.extraArgs[%d]=%s", i, arg))
			}
			rendered, err := renderZtunnelSpire(t, values...)
			require.NoError(t, err)

			deploymentYAML := strings.TrimSpace(rendered["cilium/templates/cilium-operator/deployment.yaml"])
			if tt.noOperator {
				require.Empty(t, deploymentYAML)
			} else {
				var deployment appsv1.Deployment
				require.NoError(t, yaml.Unmarshal([]byte(deploymentYAML), &deployment))
				require.Equal(t, "cilium-operator", deployment.Name)
				require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
				args := []string{"--config-dir=/tmp/cilium/config-map", "--debug=$(CILIUM_DEBUG)"}
				if tt.wantCRD {
					args = append(args, ztunnelSpireFlag+"=true")
				}
				require.Equal(t, append(args, tt.extraArgs...), deployment.Spec.Template.Spec.Containers[0].Args)
			}

			roleYAML := strings.TrimSpace(rendered["cilium/templates/cilium-operator/clusterrole.yaml"])
			if tt.noRole {
				require.Empty(t, roleYAML)
			} else {
				var role rbacv1.ClusterRole
				require.NoError(t, yaml.Unmarshal([]byte(roleYAML), &role))
				require.Equal(t, "cilium-operator", role.Name)
				// spire-controller-manager, not the operator, reconciles the
				// SPIRE registration CRDs.
				for _, rule := range role.Rules {
					require.NotContains(t, rule.APIGroups, "spire.spiffe.io")
				}
			}
			require.Equal(t, tt.wantCRD, strings.TrimSpace(rendered[ztunnelCSIDPath]) != "")

			hasSPIRE := false
			for name, manifest := range rendered {
				if strings.HasPrefix(name, "cilium/templates/spire/") && strings.TrimSpace(manifest) != "" {
					hasSPIRE = true
				}
			}
			require.Equal(t, tt.wantSPIRE, hasSPIRE)
		})
	}
}

func TestZtunnelSpireCRDValidation(t *testing.T) {
	for _, override := range []string{
		"encryption.enabled=false",
		"encryption.type=ipsec",
		"encryption.type=wireguard",
		"encryption.ztunnel.ca.type=internal",
		"authentication.mutual.spire.enabled=false",
	} {
		t.Run(override, func(t *testing.T) {
			_, err := renderZtunnelSpire(t, ztunnelSpireValues, ztunnelSpireCRD, override)
			require.ErrorContains(t, err, "encryption.ztunnel.spire.useCRD=true requires encryption.enabled=true, encryption.type=ztunnel, encryption.ztunnel.ca.type=spire and authentication.mutual.spire.enabled=true")
		})
	}
	for _, value := range []string{"enabled", "1"} {
		t.Run("non-boolean "+value, func(t *testing.T) {
			_, err := renderZtunnelSpire(t, ztunnelSpireValues, "encryption.ztunnel.spire.useCRD="+value)
			require.ErrorContains(t, err, "useCRD")
			require.ErrorContains(t, err, "boolean")
		})
	}
}

func TestZtunnelSpireCRDExtraArgs(t *testing.T) {
	for _, useCRD := range []bool{false, true} {
		for _, arg := range []string{ztunnelSpireFlag, ztunnelSpireFlag + "=true", ztunnelSpireFlag + "=false", ztunnelSpireFlag + "=1"} {
			t.Run(fmt.Sprintf("useCRD=%t/%s", useCRD, arg), func(t *testing.T) {
				_, err := renderZtunnelSpire(t,
					ztunnelSpireValues,
					fmt.Sprintf("encryption.ztunnel.spire.useCRD=%t", useCRD),
					"operator.extraArgs[0]=--log-level=debug",
					"operator.extraArgs[1]="+arg,
				)
				require.ErrorContains(t, err, "Remove --enable-ztunnel-spiffeid-crd from operator.extraArgs and use encryption.ztunnel.spire.useCRD instead")
			})
		}
	}
	t.Run("raw flag without SPIRE configuration", func(t *testing.T) {
		_, err := renderZtunnelSpire(t, "operator.extraArgs[0]="+ztunnelSpireFlag)
		require.ErrorContains(t, err, "Remove --enable-ztunnel-spiffeid-crd from operator.extraArgs and use encryption.ztunnel.spire.useCRD instead")
	})
}
