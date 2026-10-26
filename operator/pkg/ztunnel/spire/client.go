// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/spf13/pflag"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
)

const parentID = "/ztunnel"

// DefaultSpireEntryConfig returns the ztunnel SPIRE entry configuration.
func DefaultSpireEntryConfig() SpireEntryConfig {
	return SpireEntryConfig{
		ParentID:      parentID,
		PathFunc:      ztunnel.SpiffeIDPathFunc,
		SelectorsFunc: ztunnel.SpiffeIDSelectorsFunc,
	}
}

// Cell is the cell for the SPIRE client.
var Cell = cell.Module(
	"spire-client",
	"Spire Server API Client",
	cell.Config(defaultClientConfig),
	cell.Provide(DefaultSpireEntryConfig),
	cell.Provide(NewClient),
)

// ClientConfig contains the configuration for the SPIRE client.
type ClientConfig struct {
	SpireAgentSocketPath         string        `mapstructure:"mesh-auth-spire-agent-socket"`
	SpireServerAddress           string        `mapstructure:"mesh-auth-spire-server-address"`
	SpireServerConnectionTimeout time.Duration `mapstructure:"mesh-auth-spire-server-connection-timeout"`
	SpiffeTrustDomain            string        `mapstructure:"mesh-auth-spiffe-trust-domain"`
	// UseCRD leaves ztunnel workload registration to a ClusterSPIFFEID
	// reconciled by spire-controller-manager, so the operator does not
	// register workloads through the SPIRE Entry API.
	UseCRD bool `mapstructure:"enable-ztunnel-spiffeid-crd"`
}

var defaultClientConfig = ClientConfig{
	SpireAgentSocketPath:         "/run/spire/sockets/agent/agent.sock",
	SpireServerAddress:           "spire-server.spire.svc:8081",
	SpireServerConnectionTimeout: 10 * time.Second,
	SpiffeTrustDomain:            "spiffe.cilium",
}

// Flags adds the flags used by ClientConfig.
func (cfg ClientConfig) Flags(flags *pflag.FlagSet) {
	flags.String("mesh-auth-spire-agent-socket",
		cfg.SpireAgentSocketPath,
		"The path for the SPIRE admin agent Unix socket.")
	flags.String("mesh-auth-spire-server-address",
		cfg.SpireServerAddress,
		"SPIRE server endpoint.")
	flags.Duration("mesh-auth-spire-server-connection-timeout",
		cfg.SpireServerConnectionTimeout,
		"SPIRE server connection timeout.")
	flags.String("mesh-auth-spiffe-trust-domain",
		cfg.SpiffeTrustDomain,
		"The trust domain for the SPIFFE identity.")
	flags.Bool("enable-ztunnel-spiffeid-crd",
		cfg.UseCRD,
		"Leave ztunnel workload registration to a ClusterSPIFFEID reconciled by spire-controller-manager instead of the SPIRE Entry API (requires ztunnel with the SPIRE CA).")
}

type params struct {
	cell.In

	Logger        *slog.Logger
	K8sClient     k8sClient.Clientset
	Lifecycle     cell.Lifecycle
	ClientConfig  ClientConfig
	EntryConfig   SpireEntryConfig
	ZtunnelConfig ztunnel.Config
}

// SpireEntryConfig contains the configuration for ztunnel SPIRE entry generation.
type SpireEntryConfig struct {
	ParentID      string
	PathFunc      func(string) string
	SelectorsFunc func(string) []*types.Selector
}

func (cfg SpireEntryConfig) entry(trustDomain, id string) *types.Entry {
	return &types.Entry{
		SpiffeId: &types.SPIFFEID{
			TrustDomain: trustDomain,
			Path:        cfg.PathFunc(id),
		},
		ParentId: &types.SPIFFEID{
			TrustDomain: trustDomain,
			Path:        cfg.ParentID,
		},
		Selectors: cfg.SelectorsFunc(id),
	}
}

// SpireClient registers individual identities with the SPIRE server.
// Initialized reports client initialization only.
//
// List is intentionally excluded: SPIRE entry IDs are not interchangeable with
// logical ServiceAccount identities.
type SpireClient interface {
	Upsert(context.Context, string) error
	UpsertBatch(context.Context, []string) error
	Delete(context.Context, string) error
	DeleteBatch(context.Context, []string) error
	Initialized() <-chan struct{}
}

// NewClient creates the SPIRE Entry API client for ztunnel namespace
// enrollment. It returns nil when the operator does not register ztunnel
// workloads: without the SPIRE CA, or when a ClusterSPIFFEID reconciled by
// spire-controller-manager registers them.
func NewClient(params params) (SpireClient, error) {
	if params.ClientConfig.UseCRD && !params.ZtunnelConfig.UseSpireCA() {
		return nil, fmt.Errorf("enable-ztunnel-spiffeid-crd requires enable-ztunnel and ztunnel-ca-type=spire")
	}
	if !params.ZtunnelConfig.UseSpireCA() || params.ClientConfig.UseCRD {
		return nil, nil
	}
	return newEntryClient(params), nil
}
