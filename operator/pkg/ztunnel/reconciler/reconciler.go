// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package reconciler

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"sync"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	"golang.org/x/sync/semaphore"

	"github.com/cilium/cilium/operator/pkg/ztunnel/spire"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/ztunnel/table"
)

// requeueNamespace marks an enrolled namespace as pending so the reconciler
// will re-reconcile it on transient SPIRE failures.
func requeueNamespace(db *statedb.DB, enrolledTbl statedb.RWTable[*table.EnrolledNamespace], namespace string) {
	txn := db.WriteTxn(enrolledTbl)
	ns, _, found := enrolledTbl.Get(txn, table.EnrolledNamespaceByName(namespace))
	if found {
		clone := ns.Clone()
		clone.Status = reconciler.StatusPending()
		enrolledTbl.Insert(txn, clone)
	}
	txn.Commit()
}

// SpireClient is the interface for interacting with SPIRE for enrollment purposes.
type SpireClient = spire.SpireClient

type params struct {
	cell.In

	DB                     *statedb.DB
	ServiceAccountTable    statedb.Table[ServiceAccount]
	EnrolledNamespaceTable statedb.RWTable[*table.EnrolledNamespace]
	SpireClient            spire.SpireClient
	Logger                 *slog.Logger
	Lifecycle              cell.Lifecycle
	Metrics                *Metrics
	Health                 cell.Health
}

type EnrollmentReconciler struct {
	db                     *statedb.DB
	logger                 *slog.Logger
	spireClient            SpireClient
	serviceAccountTable    statedb.Table[ServiceAccount]
	enrolledNamespaceTable statedb.RWTable[*table.EnrolledNamespace]
	metrics                *Metrics
	cancel                 context.CancelFunc
	workers                sync.WaitGroup
	mu                     *semaphore.Weighted
	initializationHealth   cell.Health
}

func NewEnrollmentReconciler(cfg params) reconciler.Operations[*table.EnrolledNamespace] {
	ops := &EnrollmentReconciler{
		logger:                 cfg.Logger,
		spireClient:            cfg.SpireClient,
		db:                     cfg.DB,
		serviceAccountTable:    cfg.ServiceAccountTable,
		enrolledNamespaceTable: cfg.EnrolledNamespaceTable,
		metrics:                cfg.Metrics,
		mu:                     semaphore.NewWeighted(1),
	}
	if cfg.SpireClient != nil {
		ops.initializationHealth = cfg.Health.NewScope("spire-initialization")
		cfg.Lifecycle.Append(ops)
	}
	return ops
}

func (ops *EnrollmentReconciler) Delete(ctx context.Context, _ statedb.ReadTxn, rev statedb.Revision, ns *table.EnrolledNamespace) error {
	if err := ops.waitForInitialization(ctx); err != nil {
		return err
	}
	if err := ops.mu.Acquire(ctx, 1); err != nil {
		return err
	}
	defer ops.mu.Release(1)
	txn := ops.db.ReadTxn()
	if _, _, enrolled := ops.enrolledNamespaceTable.Get(txn, table.EnrolledNamespaceByName(ns.Name)); enrolled {
		// An old deletion may be retried after this namespace was reenrolled.
		return nil
	}
	// Namespace was enrolled, remove all service accounts in the namespace from the CA.
	sas := ops.serviceAccountTable.List(txn, ServiceAccountNamespaceIndex.Query(ns.Name))
	ids := []string{}
	for sa := range sas {
		ids = append(ids, fmt.Sprintf("%s/%s", sa.Namespace, sa.Name))
	}
	if len(ids) == 0 {
		ops.logger.Info("No service accounts found in deleted enrolled namespace", logfields.K8sNamespace, ns.Name)
		return nil
	}
	err := ops.spireClient.DeleteBatch(ctx, ids)
	if err != nil {
		ops.logger.Error("failed to delete CA entries for deleted enrolled namespace",
			logfields.K8sNamespace, ns.Name,
			logfields.Error, err.Error())
		ops.metrics.EnrollmentOps.WithLabelValues(LabelValueMethodDelete, LabelValueOutcomeFail).Add(float64(len(ids)))
		return err
	}
	ops.logger.Info("Deleted CA entries for deleted enrolled namespace",
		logfields.K8sNamespace, ns.Name,
		logfields.Count, len(ids))
	ops.metrics.EnrollmentOps.WithLabelValues(LabelValueMethodDelete, LabelValueOutcomeSuccess).Add(float64(len(ids)))
	return nil
}

// Prune unexpected entries.
func (ops *EnrollmentReconciler) Prune(ctx context.Context, txn statedb.ReadTxn, objects iter.Seq2[*table.EnrolledNamespace, statedb.Revision]) error {
	return nil
}

func (ops *EnrollmentReconciler) Update(ctx context.Context, _ statedb.ReadTxn, rev statedb.Revision, ns *table.EnrolledNamespace) error {
	if err := ops.waitForInitialization(ctx); err != nil {
		return err
	}
	if err := ops.mu.Acquire(ctx, 1); err != nil {
		return err
	}
	defer ops.mu.Release(1)
	txn := ops.db.ReadTxn()
	if _, _, enrolled := ops.enrolledNamespaceTable.Get(txn, table.EnrolledNamespaceByName(ns.Name)); !enrolled {
		return nil
	}
	ops.logger.Debug("Reconciling namespace", logfields.K8sNamespace, ns.Name)

	// Namespace is enrolled, ensure all service accounts in the namespace are
	// present in the CA.
	sas := ops.serviceAccountTable.List(txn, ServiceAccountNamespaceIndex.Query(ns.Name))
	var ids []string
	for sa := range sas {
		ids = append(ids, fmt.Sprintf("%s/%s", sa.Namespace, sa.Name))
	}
	if len(ids) == 0 {
		ops.logger.Info("No service accounts found in enrolled namespace", logfields.K8sNamespace, ns.Name)
		return nil
	}
	err := ops.spireClient.UpsertBatch(ctx, ids)
	if err != nil {
		ops.logger.Error("failed to upsert CA entries for enrolled namespace",
			logfields.K8sNamespace, ns.Name,
			logfields.Error, err.Error())
		ops.metrics.EnrollmentOps.WithLabelValues(LabelValueMethodUpsert, LabelValueOutcomeFail).Add(float64(len(ids)))
		return err
	}
	ops.logger.Info("Upserted CA entries for enrolled namespace",
		logfields.K8sNamespace, ns.Name,
		logfields.Count, len(ids))
	ops.metrics.EnrollmentOps.WithLabelValues(LabelValueMethodUpsert, LabelValueOutcomeSuccess).Add(float64(len(ids)))
	return nil
}

var _ reconciler.Operations[*table.EnrolledNamespace] = &EnrollmentReconciler{}

func (ops *EnrollmentReconciler) waitForInitialization(ctx context.Context) error {
	if err := ops.waitForInventories(ctx); err != nil {
		return err
	}
	initialized := ops.spireClient.Initialized()
	select {
	case <-initialized:
		ops.initializationHealth.OK("SPIRE client initialized")
		return ctx.Err()
	default:
	}
	ops.initializationHealth.Degraded("Waiting for SPIRE client initialization", fmt.Errorf("SPIRE client is not initialized"))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-initialized:
		ops.initializationHealth.OK("SPIRE client initialized")
		return ctx.Err()
	}
}

func (ops *EnrollmentReconciler) waitForInventories(ctx context.Context) error {
	if ops.spireClient == nil {
		return fmt.Errorf("SPIRE registration client is disabled")
	}
	txn := ops.db.ReadTxn()
	_, saInit := ops.serviceAccountTable.Initialized(txn)
	_, nsInit := ops.enrolledNamespaceTable.Initialized(txn)
	for _, initialized := range []<-chan struct{}{saInit, nsInit} {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-initialized:
		}
	}
	return ctx.Err()
}

func (ops *EnrollmentReconciler) reconcileServiceAccount(ctx context.Context, sa ServiceAccount) {
	if err := ops.mu.Acquire(ctx, 1); err != nil {
		ops.logger.Debug("ServiceAccount reconciliation canceled", logfields.Error, err)
		return
	}
	defer ops.mu.Release(1)

	id := sa.Namespace + "/" + sa.Name
	txn := ops.db.ReadTxn()
	_, _, exists := ops.serviceAccountTable.Get(txn, ServiceAccountNamespacedNameIndex.Query(id))
	var err error
	method := LabelValueMethodDelete
	if exists {
		_, _, enrolled := ops.enrolledNamespaceTable.Get(txn, table.EnrolledNamespaceByName(sa.Namespace))
		if !enrolled {
			return
		}
		// Consult current state instead of replaying a stale deletion for a
		// same-name ServiceAccount that has since been recreated.
		method = LabelValueMethodUpsert
		err = ops.spireClient.Upsert(ctx, id)
	} else {
		err = ops.spireClient.Delete(ctx, id)
	}
	if err != nil {
		ops.logger.Error("Failed to reconcile ServiceAccount registration",
			logfields.Error, err,
			logfields.Identity, id)
		ops.metrics.EnrollmentOps.WithLabelValues(method, LabelValueOutcomeFail).Inc()
		requeueNamespace(ops.db, ops.enrolledNamespaceTable, sa.Namespace)
	} else {
		ops.metrics.EnrollmentOps.WithLabelValues(method, LabelValueOutcomeSuccess).Inc()
	}
}

func (ops *EnrollmentReconciler) Start(cell.HookContext) error {
	ctx, cancel := context.WithCancel(context.Background())
	ops.cancel = cancel
	ops.workers.Go(func() {
		ops.logger.Info("Starting mTLS enrollment reconciler")

		if err := ops.waitForInventories(ctx); err != nil {
			return
		}

		wtxn := ops.db.WriteTxn(ops.serviceAccountTable)
		changeIterator, err := ops.serviceAccountTable.Changes(wtxn)
		wtxn.Commit()
		if err != nil {
			ops.logger.Error("failed to create change iterator", logfields.Error, err.Error())
			return
		}

		// Retain deletions that happen while the SPIRE connection initializes.
		if err := ops.waitForInitialization(ctx); err != nil {
			return
		}

		for {
			changes, watch := changeIterator.Next(ops.db.ReadTxn())
			for change := range changes {
				ops.reconcileServiceAccount(ctx, change.Object)
			}
			select {
			case <-ctx.Done():
				return
			case <-watch:
			}
		}
	})
	return nil
}

func (ops *EnrollmentReconciler) Stop(cell.HookContext) error {
	if ops.cancel != nil {
		ops.cancel()
		ops.workers.Wait()
	}
	ops.logger.Info("Stopping reconciler")
	return nil
}

var _ cell.HookInterface = &EnrollmentReconciler{}
