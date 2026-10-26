// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package reconciler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/cilium/hive/job"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ciliumhive "github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/hive/health"
	healthtypes "github.com/cilium/cilium/pkg/hive/health/types"
	k8sTables "github.com/cilium/cilium/pkg/k8s/tables"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/ztunnel/table"
)

func newTestK8sNamespace(name string, labels map[string]string) k8sTables.Namespace {
	return k8sTables.Namespace{
		Name:   name,
		Labels: labels,
	}
}

// mockSpireClient implements SpireClient for testing.
type mockSpireClient struct {
	upsertBatchFunc func(ctx context.Context, ids []string) error
	deleteBatchFunc func(ctx context.Context, ids []string) error
	upsertFunc      func(ctx context.Context, id string) error
	deleteFunc      func(ctx context.Context, id string) error
	initializedFunc func() <-chan struct{}
	initialized     chan struct{}
}

func newMockSpireClient() *mockSpireClient {
	ch := make(chan struct{})
	close(ch)
	return &mockSpireClient{
		initialized: ch,
	}
}

func (m *mockSpireClient) UpsertBatch(ctx context.Context, ids []string) error {
	if m.upsertBatchFunc != nil {
		return m.upsertBatchFunc(ctx, ids)
	}
	return nil
}

func (m *mockSpireClient) DeleteBatch(ctx context.Context, ids []string) error {
	if m.deleteBatchFunc != nil {
		return m.deleteBatchFunc(ctx, ids)
	}
	return nil
}

func (m *mockSpireClient) Upsert(ctx context.Context, id string) error {
	if m.upsertFunc != nil {
		return m.upsertFunc(ctx, id)
	}
	return nil
}

func (m *mockSpireClient) Delete(ctx context.Context, id string) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, id)
	}
	return nil
}

func (m *mockSpireClient) Initialized() <-chan struct{} {
	if m.initializedFunc != nil {
		return m.initializedFunc()
	}
	return m.initialized
}

func setupTest(t *testing.T) (*statedb.DB, statedb.RWTable[*table.EnrolledNamespace], statedb.RWTable[ServiceAccount], *mockSpireClient, *EnrollmentReconciler) {
	db := statedb.New()

	enrolledTbl, err := table.NewEnrolledNamespacesTable(db)
	require.NoError(t, err)

	saTbl, err := statedb.NewTable(db, "serviceaccounts",
		ServiceAccountNamespacedNameIndex,
		ServiceAccountNamespaceIndex,
	)
	require.NoError(t, err)

	mock := newMockSpireClient()
	logger := hivetest.Logger(t)
	health, _ := cell.NewSimpleHealth()

	ops := &EnrollmentReconciler{
		db:                     db,
		logger:                 logger,
		spireClient:            mock,
		serviceAccountTable:    saTbl,
		enrolledNamespaceTable: enrolledTbl,
		metrics:                NewMetrics(),
		mu:                     semaphore.NewWeighted(1),
		initializationHealth:   health,
	}

	return db, enrolledTbl, saTbl, mock, ops
}

func insertServiceAccounts(_ *testing.T, db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], namespace string, names ...string) {
	txn := db.WriteTxn(saTbl)
	for _, name := range names {
		saTbl.Insert(txn, ServiceAccount{
			ServiceAccount: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
			},
		})
	}
	txn.Commit()
}

// getMetric reads the current value of a counter with the given label values.
func getMetric(m *Metrics, method, outcome string) float64 {
	return m.EnrollmentOps.WithLabelValues(method, outcome).Get()
}

func TestEnrollmentReconciler_Update(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		saNames       []string
		upsertErr     error
		wantErr       bool
		wantUpsertIDs []string
	}{
		{
			name:          "upserts service accounts in namespace",
			namespace:     "test-ns",
			saNames:       []string{"sa1", "sa2"},
			wantErr:       false,
			wantUpsertIDs: []string{"test-ns/sa1", "test-ns/sa2"},
		},
		{
			name:      "upsert batch error",
			namespace: "test-ns",
			saNames:   []string{"sa1"},
			upsertErr: fmt.Errorf("spire server unavailable"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			enrollNamespace(db, enrolledTbl, tt.namespace)

			if len(tt.saNames) > 0 {
				insertServiceAccounts(t, db, saTbl, tt.namespace, tt.saNames...)
			}

			var gotIDs []string
			mock.upsertBatchFunc = func(ctx context.Context, ids []string) error {
				gotIDs = ids
				return tt.upsertErr
			}

			ns := &table.EnrolledNamespace{
				Name:   tt.namespace,
				Status: reconciler.StatusPending(),
			}
			err := ops.Update(t.Context(), db.ReadTxn(), 0, ns)

			if tt.wantErr {
				require.Error(t, err)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeFail))
			} else {
				require.NoError(t, err)
				require.ElementsMatch(t, tt.wantUpsertIDs, gotIDs)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeSuccess))
			}
		})
	}
}

func TestEnrollmentReconciler_Delete(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		saNames       []string
		deleteErr     error
		wantErr       bool
		wantDeleteIDs []string
	}{
		{
			name:      "empty namespace does not send a synthetic account ID",
			namespace: "empty-ns",
		},
		{
			name:          "deletes service accounts in namespace",
			namespace:     "test-ns",
			saNames:       []string{"sa1", "sa2"},
			wantErr:       false,
			wantDeleteIDs: []string{"test-ns/sa1", "test-ns/sa2"},
		},
		{
			name:      "delete batch error",
			namespace: "test-ns",
			saNames:   []string{"sa1"},
			deleteErr: fmt.Errorf("spire server unavailable"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _, saTbl, mock, ops := setupTest(t)

			if len(tt.saNames) > 0 {
				insertServiceAccounts(t, db, saTbl, tt.namespace, tt.saNames...)
			}

			var gotIDs []string
			mock.deleteBatchFunc = func(ctx context.Context, ids []string) error {
				gotIDs = ids
				return tt.deleteErr
			}

			ns := &table.EnrolledNamespace{
				Name:   tt.namespace,
				Status: reconciler.StatusPending(),
			}
			err := ops.Delete(t.Context(), db.ReadTxn(), 0, ns)

			if tt.wantErr {
				require.Error(t, err)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeFail))
			} else {
				require.NoError(t, err)
				require.ElementsMatch(t, tt.wantDeleteIDs, gotIDs)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeSuccess))
			}
		})
	}
}

func TestEnrollmentReconciler_GuardsStaleEvents(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "shop")
	insertServiceAccounts(t, db, saTbl, "shop", "recreated")
	staleTxn := db.ReadTxn()
	ns := &table.EnrolledNamespace{Name: "shop"}
	upserts := []string{}
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserts = append(upserts, id)
		return nil
	}
	mock.deleteFunc = func(context.Context, string) error {
		t.Error("stale event deleted the recreated ServiceAccount")
		return nil
	}
	mock.deleteBatchFunc = func(context.Context, []string) error {
		t.Error("stale event deleted the reenrolled namespace")
		return nil
	}
	ops.reconcileServiceAccount(t.Context(), ServiceAccount{&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "recreated", UID: "old-uid"},
	}})
	require.Equal(t, []string{"shop/recreated"}, upserts)
	require.NoError(t, ops.Delete(t.Context(), staleTxn, 0, ns))

	txn := db.WriteTxn(enrolledTbl)
	enrolledTbl.Delete(txn, ns)
	txn.Commit()
	mock.upsertBatchFunc = func(context.Context, []string) error {
		t.Error("stale update recreated an unenrolled namespace registration")
		return nil
	}
	require.NoError(t, ops.Update(t.Context(), staleTxn, 0, ns))
}

func TestEnrollmentReconciler_WaitsForInventoriesAndBackend(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			txn := db.WriteTxn(saTbl, enrolledTbl)
			saReady := saTbl.RegisterInitializer(txn, "serviceaccounts")
			nsReady := enrolledTbl.RegisterInitializer(txn, "namespaces")
			txn.Commit()
			mock.initialized = make(chan struct{})
			if operation == "update" {
				enrollNamespace(db, enrolledTbl, "shop")
			}
			insertServiceAccounts(t, db, saTbl, "shop", "checkout")
			called := make(chan struct{}, 1)
			mock.upsertBatchFunc = func(context.Context, []string) error {
				called <- struct{}{}
				return nil
			}
			mock.deleteBatchFunc = mock.upsertBatchFunc
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				ns := &table.EnrolledNamespace{Name: "shop"}
				switch operation {
				case "update":
					done <- ops.Update(ctx, db.ReadTxn(), 0, ns)
				case "delete":
					done <- ops.Delete(ctx, db.ReadTxn(), 0, ns)
				}
			}()
			assertBlocked := func() {
				t.Helper()
				select {
				case <-called:
					t.Fatal("registration ran before all inventories and the backend initialized")
				case <-time.After(20 * time.Millisecond):
				}
			}
			assertBlocked()
			txn = db.WriteTxn(saTbl)
			saReady(txn)
			txn.Commit()
			assertBlocked()
			txn = db.WriteTxn(enrolledTbl)
			nsReady(txn)
			txn.Commit()
			assertBlocked()
			close(mock.initialized)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("registration did not run after initialization")
			}
			require.Len(t, called, 1)
		})
	}
}

func TestEnrollmentReconciler_Update_OnlyIncludesMatchingNamespace(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Insert SAs in two different namespaces.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1", "sa2")
	insertServiceAccounts(t, db, saTbl, "other-ns", "sa3")

	var gotIDs []string
	mock.upsertBatchFunc = func(ctx context.Context, ids []string) error {
		gotIDs = ids
		return nil
	}

	// Update should only include SAs from "enrolled-ns".
	ns := &table.EnrolledNamespace{
		Name:   "enrolled-ns",
		Status: reconciler.StatusPending(),
	}
	err := ops.Update(t.Context(), db.ReadTxn(), 0, ns)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"enrolled-ns/sa1", "enrolled-ns/sa2"}, gotIDs)
}

// initializeTables marks both SA and enrolled namespace tables as initialized
// so that Start() can proceed past its initialization checks.
func initializeTables(db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], enrolledTbl statedb.RWTable[*table.EnrolledNamespace]) {
	txn := db.WriteTxn(saTbl, enrolledTbl)
	completeSA := saTbl.RegisterInitializer(txn, "test")
	completeNS := enrolledTbl.RegisterInitializer(txn, "test")
	completeSA(txn)
	completeNS(txn)
	txn.Commit()
}

func enrollNamespace(db *statedb.DB, enrolledTbl statedb.RWTable[*table.EnrolledNamespace], name string) {
	txn := db.WriteTxn(enrolledTbl)
	enrolledTbl.Insert(txn, &table.EnrolledNamespace{
		Name:   name,
		Status: reconciler.StatusPending(),
	})
	txn.Commit()
}

func deleteServiceAccount(db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], namespace, name string) {
	txn := db.WriteTxn(saTbl)
	saTbl.Delete(txn, ServiceAccount{
		ServiceAccount: &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		},
	})
	txn.Commit()
}

func TestEnrollmentReconciler_Start_UpsertOnSACreate(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	// Enroll a namespace before starting.
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a service account in the enrolled namespace.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sa1", id)
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeSuccess) == 1
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert")
	}
}

func TestEnrollmentReconciler_Start_SkipNonEnrolledNamespace(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	// Enroll "enrolled-ns" but NOT "other-ns".
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a service account in the non-enrolled namespace first.
	insertServiceAccounts(t, db, saTbl, "other-ns", "sa1")

	// Then add one in the enrolled namespace. When we see this upsert,
	// we know the watcher has processed past the non-enrolled SA.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sentinel")

	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sentinel", id, "only the enrolled namespace SA should be upserted")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sentinel upsert")
	}
}

func TestEnrollmentReconciler_Start_DeleteOnSARemove(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Pre-populate a SA so we can delete it.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	upserted := make(chan string, 10)
	deleted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}
	mock.deleteFunc = func(_ context.Context, id string) error {
		deleted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Wait for the initial SA creation to be processed.
	select {
	case <-upserted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial upsert")
	}

	// Now delete the SA.
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")

	select {
	case id := <-deleted:
		require.Equal(t, "enrolled-ns/sa1", id)
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeSuccess) == 1
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delete")
	}
}

func TestEnrollmentReconciler_Start_ErrorMetricsAndRequeue(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upsertCalled := make(chan struct{}, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upsertCalled <- struct{}{}
		return fmt.Errorf("spire unavailable")
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a SA — upsert should be called even though it errors.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	select {
	case <-upsertCalled:
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeFail) == 1
		}, 5*time.Second, 10*time.Millisecond)

		// Verify the namespace was requeued to pending for re-reconciliation.
		require.Eventually(t, func() bool {
			ns, _, found := enrolledTbl.Get(db.ReadTxn(), table.EnrolledNamespaceByName("enrolled-ns"))
			return found && ns.Status.Kind == reconciler.StatusKindPending
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert call")
	}
}

func TestEnrollmentReconciler_Start_WaitsForSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, _, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Create a mock with uninitialized SPIRE client.
	spireInitCh := make(chan struct{})
	mock := &mockSpireClient{
		initialized: spireInitCh,
	}
	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}
	ops.spireClient = mock

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a SA before SPIRE is initialized.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	// Verify that no upsert happens while SPIRE is not initialized.
	select {
	case id := <-upserted:
		t.Fatalf("unexpected upsert before SPIRE init: %s", id)
	case <-time.After(200 * time.Millisecond):
		// Expected.
	}

	// Now initialize SPIRE.
	close(spireInitCh)

	// The SA change should be processed now.
	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sa1", id)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert after SPIRE init")
	}
}

func TestEnrollmentReconciler_Start_DeletionDuringSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	mock.initialized = make(chan struct{})
	waiting := make(chan struct{})
	signalWaiting := sync.OnceFunc(func() { close(waiting) })
	release := sync.OnceFunc(func() { close(mock.initialized) })
	mock.initializedFunc = func() <-chan struct{} {
		signalWaiting()
		return mock.initialized
	}
	var registered lock.Map[string, struct{}]
	registered.Store("enrolled-ns/sa1", struct{}{})
	deleted := make(chan string, 10)
	mock.deleteFunc = func(_ context.Context, id string) error {
		registered.Delete(id)
		deleted <- id
		return nil
	}
	require.NoError(t, ops.Start(t.Context()))
	t.Cleanup(func() {
		release()
		require.NoError(t, ops.Stop(nil))
	})

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend readiness wait")
	}
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")
	select {
	case id := <-deleted:
		t.Fatalf("unexpected deletion before SPIRE initialization: %s", id)
	default:
	}
	release()

	select {
	case id := <-deleted:
		require.Equal(t, "enrolled-ns/sa1", id)
		_, remains := registered.Load(id)
		require.False(t, remains)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion during SPIRE initialization was lost")
	}
}

func TestEnrollmentReconciler_Start_CancelDuringSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")
	mock.initialized = make(chan struct{})
	waiting := make(chan struct{})
	signalWaiting := sync.OnceFunc(func() { close(waiting) })
	release := sync.OnceFunc(func() { close(mock.initialized) })
	mock.initializedFunc = func() <-chan struct{} {
		signalWaiting()
		return mock.initialized
	}
	mutations := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		mutations <- id
		return nil
	}
	mock.deleteFunc = mock.upsertFunc
	require.NoError(t, ops.Start(t.Context()))
	t.Cleanup(func() {
		release()
		require.NoError(t, ops.Stop(nil))
	})

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend readiness wait")
	}
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")
	stopped := make(chan error, 1)
	go func() { stopped <- ops.Stop(nil) }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop while SPIRE initialization was pending")
	}
	release()
	select {
	case id := <-mutations:
		t.Fatalf("unexpected mutation after cancellation: %s", id)
	default:
	}
}

type readyForSerializationContext struct {
	context.Context
	checks atomic.Int32
	ready  chan struct{}
}

func (ctx *readyForSerializationContext) Err() error {
	err := ctx.Context.Err()
	// The inventory and backend waits each finish by checking Err. Capture the
	// second result before signaling so cancellation cannot bypass serialization
	// by returning from the readiness wait instead.
	if ctx.checks.Add(1) == 2 {
		close(ctx.ready)
	}
	return err
}

func TestEnrollmentReconciler_Stop_CancelsBlockedNamespaceJob(t *testing.T) {
	db, enrolledTbl, saTbl, mock, _ := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	enrollNamespace(db, enrolledTbl, "shop")
	insertServiceAccounts(t, db, saTbl, "shop", "held")

	entered := make(chan struct{})
	backendCanceled := make(chan struct{})
	releaseBackend := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseBackend) })
	mock.upsertFunc = func(ctx context.Context, _ string) error {
		close(entered)
		select {
		case <-ctx.Done():
			close(backendCanceled)
			return ctx.Err()
		case <-releaseBackend:
			return nil
		}
	}
	mutations := make(chan struct{}, 1)
	mock.upsertBatchFunc = func(context.Context, []string) error {
		mutations <- struct{}{}
		return nil
	}

	ready := make(chan struct{})
	jobResult := make(chan error, 1)
	log := hivetest.Logger(t)
	health, _ := cell.NewSimpleHealth()
	var ops *EnrollmentReconciler
	h := hive.New(
		job.Cell,
		cell.Invoke(func(lc cell.Lifecycle, registry job.Registry) {
			ops = NewEnrollmentReconciler(params{
				DB: db, Logger: log, Lifecycle: lc, Metrics: NewMetrics(), Health: health,
				SpireClient: mock, ServiceAccountTable: saTbl, EnrolledNamespaceTable: enrolledTbl,
			}).(*EnrollmentReconciler)
			// Static StateDB jobs are likewise registered after the watcher hook.
			registry.NewGroup(health).Add(job.OneShot("namespace-update", func(ctx context.Context, _ cell.Health) error {
				select {
				case <-entered:
				case <-ctx.Done():
					return ctx.Err()
				}
				err := ops.Update(&readyForSerializationContext{Context: ctx, ready: ready},
					db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "shop"})
				jobResult <- err
				return err
			}))
		}),
	)
	t.Cleanup(func() {
		release()
		if ops != nil && ops.cancel != nil {
			ops.cancel()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.Stop(log, ctx) }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Error("timed out cleaning up lifecycle jobs")
		}
	})
	require.NoError(t, h.Start(log, t.Context()))
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("namespace job did not reach serialization behind the watcher")
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, h.Stop(log, ctx), "namespace cancellation must not depend on the later watcher stop hook")
	require.ErrorIs(t, <-jobResult, context.Canceled)
	select {
	case <-backendCanceled:
	default:
		t.Fatal("watcher stop did not cancel its stalled backend request")
	}
	require.Empty(t, mutations, "cancelled namespace job must not mutate the backend")
}

func TestEnrollmentReconciler_InitializationHealth(t *testing.T) {
	for _, method := range []string{"update", "delete"} {
		for _, cancelWait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", method, cancelWait), func(t *testing.T) {
				db, enrolledTbl, saTbl, mock, ops := setupTest(t)
				initializeTables(db, saTbl, enrolledTbl)
				if method == "update" {
					enrollNamespace(db, enrolledTbl, "shop")
				}
				mock.initialized = make(chan struct{})
				health, status := cell.NewSimpleHealth()
				ops.initializationHealth = health
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					op := ops.Update
					if method == "delete" {
						op = ops.Delete
					}
					done <- op(ctx, db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "shop"})
				}()
				require.Eventually(t, func() bool {
					status.Lock()
					defer status.Unlock()
					return status.Level == cell.StatusDegraded
				}, time.Second, time.Millisecond)
				require.Error(t, status.Error)
				select {
				case err := <-done:
					t.Fatalf("initialization must still be pending: %v", err)
				default:
				}
				if cancelWait {
					cancel()
				} else {
					close(mock.initialized)
				}
				select {
				case err := <-done:
					if cancelWait {
						require.ErrorIs(t, err, context.Canceled)
						require.Equal(t, cell.StatusDegraded, status.Level)
					} else {
						require.NoError(t, err)
						require.Equal(t, cell.StatusOK, status.Level)
					}
				case <-time.After(time.Second):
					t.Fatal("initialization wait did not finish")
				}
			})
		}
	}
}

func TestEnrollmentReconciler_InitializationProductionHealth(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	mock.initialized = make(chan struct{})
	var healthDB *statedb.DB
	var statuses statedb.Table[healthtypes.Status]
	h := ciliumhive.New(cell.Invoke(func(reporter cell.Health, db *statedb.DB, table statedb.Table[healthtypes.Status]) {
		ops.initializationHealth = reporter.NewScope("spire-initialization")
		healthDB, statuses = db, table
	}))
	require.NoError(t, h.Populate(hivetest.Logger(t)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mock.initializedFunc = func() <-chan struct{} {
		cancel()
		return mock.initialized
	}
	require.NotPanics(t, func() {
		require.ErrorIs(t, ops.waitForInitialization(ctx), context.Canceled)
	})
	degraded := statedb.Collect(statuses.List(healthDB.ReadTxn(), health.StatusByLevel(healthtypes.LevelDegraded)))
	require.Len(t, degraded, 1)
	require.NotEmpty(t, degraded[0].Error)
	mock.initializedFunc = nil
	close(mock.initialized)
	require.NoError(t, ops.waitForInitialization(t.Context()))
	require.Empty(t, statedb.Collect(statuses.List(healthDB.ReadTxn(), health.StatusByLevel(healthtypes.LevelDegraded))))
	healthy := statedb.Collect(statuses.List(healthDB.ReadTxn(), health.StatusByLevel(healthtypes.LevelOK)))
	require.Len(t, healthy, 1)
	require.Empty(t, healthy[0].Error)
}

func TestEnrollmentReconciler_SerializationCancellation(t *testing.T) {
	for _, operation := range []string{"update", "delete", "service-account"} {
		t.Run(operation, func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			initializeTables(db, saTbl, enrolledTbl)
			enrollNamespace(db, enrolledTbl, "shop")
			insertServiceAccounts(t, db, saTbl, "shop", "held", "pending")
			insertServiceAccounts(t, db, saTbl, "gone", "pending")
			account := func(name string) ServiceAccount {
				return ServiceAccount{&corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
				}}
			}

			entered := make(chan struct{})
			releaseBackend := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseBackend) })
			mutations := make(chan string, 10)
			mock.upsertFunc = func(ctx context.Context, id string) error {
				if id != "shop/held" {
					mutations <- id
					return nil
				}
				close(entered)
				select {
				case <-releaseBackend:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			mock.upsertBatchFunc = func(context.Context, []string) error {
				mutations <- "upsert-batch"
				return nil
			}
			mock.deleteBatchFunc = func(context.Context, []string) error {
				mutations <- "delete-batch"
				return nil
			}
			invoke := func(ctx context.Context) error {
				switch operation {
				case "update":
					return ops.Update(ctx, db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "shop"})
				case "delete":
					return ops.Delete(ctx, db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "gone"})
				default:
					ops.reconcileServiceAccount(ctx, account("pending"))
					return ctx.Err()
				}
			}

			holderCtx, cancelHolder := context.WithCancel(t.Context())
			ctx, cancel := context.WithCancel(t.Context())
			holderDone := make(chan struct{})
			var operationDone chan struct{}
			t.Cleanup(func() {
				cancel()
				release()
				cancelHolder()
				for _, done := range []chan struct{}{holderDone, operationDone} {
					if done == nil {
						continue
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("timed out cleaning up serialized operation")
					}
				}
			})
			go func() {
				defer close(holderDone)
				ops.reconcileServiceAccount(holderCtx, account("held"))
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("serialization holder did not enter the backend")
			}

			ready := make(chan struct{})
			operationDone = make(chan struct{})
			var operationErr error
			go func() {
				defer close(operationDone)
				if operation == "service-account" {
					close(ready)
					operationErr = invoke(ctx)
				} else {
					operationErr = invoke(&readyForSerializationContext{Context: ctx, ready: ready})
				}
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not reach serialization")
			}
			cancel()
			select {
			case <-operationDone:
				require.ErrorIs(t, operationErr, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("cancelled operation kept waiting for the stalled backend")
			}
			require.Empty(t, mutations, "cancelled waiter must not mutate the backend")

			release()
			select {
			case <-holderDone:
			case <-time.After(5 * time.Second):
				t.Fatal("backend request did not finish after release")
			}
			ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, invoke(ctx))
			require.Len(t, mutations, 1, "serialization must remain usable after a waiter is cancelled")
		})
	}
}

func TestK8sNamespaceToEnrolledNamespace_LabelMatching(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		deleted    bool
		wantResult statedb.DeriveResult
	}{
		{
			name:       "enrolled namespace",
			labels:     map[string]string{"io.cilium/mtls-enabled": "true"},
			wantResult: statedb.DeriveInsert,
		},
		{
			name:       "not enrolled - label missing",
			labels:     map[string]string{},
			wantResult: statedb.DeriveDelete,
		},
		{
			name:       "enrolled but deleted",
			labels:     map[string]string{"io.cilium/mtls-enabled": "true"},
			deleted:    true,
			wantResult: statedb.DeriveDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := newTestK8sNamespace(tt.name, tt.labels)
			_, result := table.K8sNamespaceToEnrolledNamespace(ns, tt.deleted)
			require.Equal(t, tt.wantResult, result)
		})
	}
}
