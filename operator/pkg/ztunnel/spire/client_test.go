// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"testing"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"github.com/stretchr/testify/require"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	"github.com/cilium/cilium/pkg/k8s/client"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client/testutils"
)

func TestClientBackendWiring(t *testing.T) {
	for _, tt := range []struct {
		name    string
		caType  ztunnel.CAType
		ztunnel bool
		crd     bool
		wantErr bool
	}{
		{name: "disabled", caType: ztunnel.CATypeInternal},
		{name: "disabled SPIRE", caType: ztunnel.CATypeSpire},
		{name: "internal ztunnel", caType: ztunnel.CATypeInternal, ztunnel: true},
		{name: "direct SPIRE ztunnel", caType: ztunnel.CATypeSpire, ztunnel: true},
		{name: "CRD SPIRE ztunnel", caType: ztunnel.CATypeSpire, ztunnel: true, crd: true},
		{name: "CRD requires ztunnel", caType: ztunnel.CATypeSpire, crd: true, wantErr: true},
		{name: "CRD requires SPIRE CA", caType: ztunnel.CATypeInternal, ztunnel: true, crd: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			log := hivetest.Logger(t)
			_, cs := k8sClient.NewFakeClientset(log)
			var registration SpireClient
			h := hive.New(
				Cell,
				cell.Config(ztunnel.DefaultConfig),
				cell.Provide(func() client.Clientset { return cs }),
				cell.Invoke(func(c SpireClient) {
					registration = c
				}),
			)
			h.Viper().Set("ztunnel-ca-type", string(tt.caType))
			h.Viper().Set("enable-ztunnel", tt.ztunnel)
			h.Viper().Set("enable-ztunnel-spiffeid-crd", tt.crd)
			h.Viper().Set("mesh-auth-spiffe-trust-domain", "test.cilium")
			err := h.Populate(log)
			if tt.wantErr {
				require.ErrorContains(t, err, "requires enable-ztunnel and ztunnel-ca-type=spire")
				return
			}
			require.NoError(t, err)
			if !tt.ztunnel || tt.caType != ztunnel.CATypeSpire || tt.crd {
				// In CRD mode, a ClusterSPIFFEID registers ztunnel workloads.
				require.IsType(t, nil, registration, "disabled registration must be a nil interface")
				return
			}
			direct, ok := registration.(*EntryClient)
			require.True(t, ok)
			require.NotNil(t, registration.Initialized())
			select {
			case <-registration.Initialized():
				t.Fatal("client initialized before lifecycle startup")
			default:
			}
			require.Same(t, cs, direct.k8sClient)
			require.Nil(t, direct.entry)
			wantCfg := defaultClientConfig
			wantCfg.SpiffeTrustDomain = "test.cilium"
			require.Equal(t, wantCfg, direct.cfg)
			require.Equal(t, DefaultSpireEntryConfig().entry("test.cilium", "shop/checkout"), direct.entryCfg.entry("test.cilium", "shop/checkout"))
		})
	}
}

func TestSpireEntryConfig(t *testing.T) {
	cfg := DefaultSpireEntryConfig()
	require.Equal(t, &types.Entry{
		SpiffeId: &types.SPIFFEID{TrustDomain: "test.cilium", Path: "/ns/shop/sa/checkout"},
		ParentId: &types.SPIFFEID{TrustDomain: "test.cilium", Path: "/ztunnel"},
		Selectors: []*types.Selector{
			{Type: "k8s", Value: "ns:shop"},
			{Type: "k8s", Value: "sa:checkout"},
		},
	}, cfg.entry("test.cilium", "shop/checkout"))
}
