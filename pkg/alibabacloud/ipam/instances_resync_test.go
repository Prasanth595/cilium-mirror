// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipam

import (
	"context"
	"net/netip"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"

	apimock "github.com/cilium/cilium/pkg/alibabacloud/api/mock"
	"github.com/cilium/cilium/pkg/alibabacloud/types"
	"github.com/cilium/cilium/pkg/ipam/resynctest"
	ipamTypes "github.com/cilium/cilium/pkg/ipam/types"
)

// hookedAPI calls afterGetInstances while a full resync holds its ECS snapshot.
type hookedAPI struct {
	AlibabaCloudAPI
	afterGetInstances func()
}

func (h *hookedAPI) GetInstances(ctx context.Context, vpcs ipamTypes.VirtualNetworkMap, subnets ipamTypes.SubnetMap) (*ipamTypes.InstanceMap, error) {
	instances, err := h.AlibabaCloudAPI.GetInstances(ctx, vpcs, subnets)
	if h.afterGetInstances != nil {
		h.afterGetInstances()
	}
	return instances, err
}

func (h *hookedAPI) setHook(hook func()) {
	h.afterGetInstances = hook
}

func newHookedInstancesManager(t *testing.T) (*apimock.API, *hookedAPI, *InstancesManager) {
	api := apimock.NewAPI(subnets, vpcs, securityGroups)
	api.UpdateENIs(primaryENIs)
	hooked := &hookedAPI{AlibabaCloudAPI: api}
	mngr := NewInstancesManager(hivetest.Logger(t), hooked)
	_, err := mngr.Resync(t.Context())
	require.NoError(t, err)
	return api, hooked, mngr
}

func TestFullResync(t *testing.T) {
	const instanceID = "i-1"

	t.Run("IPRelease", func(t *testing.T) {
		t.Parallel()
		api, hooked, mngr := newHookedInstancesManager(t)
		eniID, eni, err := api.CreateNetworkInterface(t.Context(), 2, "vsw-1", []string{"sg-1"}, nil)
		require.NoError(t, err)
		require.NoError(t, api.AttachNetworkInterface(t.Context(), instanceID, eniID))
		_, err = mngr.Resync(t.Context())
		require.NoError(t, err)
		require.Len(t, eni.PrivateIPSets, 3)
		released := eni.PrivateIPSets[1].PrivateIpAddress

		resynctest.RunDuringFullResync(t, mngr, hooked.setHook,
			func() error {
				return api.UnassignPrivateIPAddresses(t.Context(), eniID, []string{released.String()})
			},
			func() {
				mngr.RemoveIPsFromENI(instanceID, eniID, []netip.Prefix{netip.PrefixFrom(released.Addr, 32)})
			})

		var addresses []types.PrivateIPSet
		mngr.ForeachInstance(instanceID, func(_, interfaceID string, iface ipamTypes.Interface) error {
			if interfaceID == eniID {
				addresses = iface.(*types.ENI).PrivateIPSets
			}
			return nil
		})
		require.ElementsMatch(t, []types.PrivateIPSet{eni.PrivateIPSets[0], eni.PrivateIPSets[2]}, addresses)
	})

	t.Run("ENIUpdate", func(t *testing.T) {
		t.Parallel()
		api, hooked, mngr := newHookedInstancesManager(t)
		resynctest.ENIUpdate(t, mngr, hooked.setHook, instanceID, func() (string, *types.ENI, error) {
			eniID, eni, err := api.CreateNetworkInterface(context.Background(), 1, "vsw-1", []string{"sg-1"}, nil)
			if err != nil {
				return "", nil, err
			}
			return eniID, eni, api.AttachNetworkInterface(context.Background(), instanceID, eniID)
		})
	})

	t.Run("InstanceDelete", func(t *testing.T) {
		t.Parallel()
		_, hooked, mngr := newHookedInstancesManager(t)
		resynctest.InstanceDelete(t, mngr, hooked.setHook, instanceID)
	})
}
