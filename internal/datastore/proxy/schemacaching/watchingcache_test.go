package schemacaching

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"golang.org/x/exp/slices"

	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/options"
	corev1 "github.com/authzed/spicedb/pkg/proto/core/v1"
)

var goleakIgnores = []goleak.Option{
	goleak.IgnoreTopFunction("github.com/golang/glog.(*loggingT).flushDaemon"),
	goleak.IgnoreTopFunction("github.com/outcaste-io/ristretto.(*lfuPolicy).processItems"),
	goleak.IgnoreTopFunction("github.com/outcaste-io/ristretto.(*Cache).processItems"),
	goleak.IgnoreCurrent(),
}

func TestWatchingCacheBasicOperation(t *testing.T) {
	defer goleak.VerifyNone(t, goleakIgnores...)

	fakeDS := &fakeDatastore{
		headRevision: rev("0"),
		namespaces:   map[string][]fakeEntry[datastore.RevisionedNamespace, *corev1.NamespaceDefinition]{},
		caveats:      map[string][]fakeEntry[datastore.RevisionedCaveat, *corev1.CaveatDefinition]{},
		schemaChan:   make(chan *datastore.SchemaState, 1),
		errChan:      make(chan error, 1),
	}

	cache := createWatchingCacheProxy(fakeDS, 1*time.Hour)
	require.NoError(t, cache.Start(context.Background()))

	// Ensure no namespaces are found.
	_, _, err := cache.SnapshotReader(rev("1")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{})
	require.False(t, cache.namespaceCache.inFallbackMode)

	// Ensure a re-read also returns not found, even before a checkpoint is received.
	_, _, err = cache.SnapshotReader(rev("1")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{})

	// Send a checkpoint for revision 1.
	fakeDS.sendCheckpoint(rev("1"))

	// Write a namespace update at revision 2.
	fakeDS.updateNamespace("somenamespace", &corev1.NamespaceDefinition{Name: "somenamespace"}, rev("2"))

	// Ensure that reading at rev 2 returns found.
	nsDef, _, err := cache.SnapshotReader(rev("2")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.NoError(t, err)
	require.Equal(t, "somenamespace", nsDef.Name)

	// Disable reads.
	fakeDS.disableReads()

	// Ensure that reading at rev 3 returns an error, as with reads disabled the cache should not be hit.
	_, _, err = cache.SnapshotReader(rev("3")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.Error(t, err)
	require.ErrorContains(t, err, "reads are disabled")

	// Re-enable reads.
	fakeDS.enableReads()

	// Ensure that reading at rev 3 returns found, even though the cache should not yet be there. This will
	// require a datastore fallback read because the cache is not yet checkedpointed to that revision.
	nsDef, _, err = cache.SnapshotReader(rev("3")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.NoError(t, err)
	require.Equal(t, "somenamespace", nsDef.Name)

	// Checkpoint to rev 4.
	fakeDS.sendCheckpoint(rev("4"))
	require.False(t, cache.namespaceCache.inFallbackMode)

	// Disable reads.
	fakeDS.disableReads()

	// Read again, which should now be via the cache.
	nsDef, _, err = cache.SnapshotReader(rev("3.5")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.NoError(t, err)
	require.Equal(t, "somenamespace", nsDef.Name)

	// Read via a lookup.
	nsDefs, err := cache.SnapshotReader(rev("3.5")).LookupNamespacesWithNames(context.Background(), []string{"somenamespace"})
	require.NoError(t, err)
	require.Equal(t, "somenamespace", nsDefs[0].Definition.Name)

	// Delete the namespace at revision 5.
	fakeDS.updateNamespace("somenamespace", nil, rev("5"))

	// Re-read at an earlier revision.
	nsDef, _, err = cache.SnapshotReader(rev("3.5")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.NoError(t, err)
	require.Equal(t, "somenamespace", nsDef.Name)

	// Read at revision 5.
	_, _, err = cache.SnapshotReader(rev("5")).ReadNamespaceByName(context.Background(), "somenamespace")
	require.Error(t, err)
	require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{}, "missing not found in: %v", err)

	// Lookup at revision 5.
	nsDefs, err = cache.SnapshotReader(rev("5")).LookupNamespacesWithNames(context.Background(), []string{"somenamespace"})
	require.NoError(t, err)
	require.Empty(t, nsDefs)

	// Update a caveat.
	fakeDS.updateCaveat("somecaveat", &corev1.CaveatDefinition{Name: "somecaveat"}, rev("6"))

	// Read at revision 6.
	caveatDef, _, err := cache.SnapshotReader(rev("6")).ReadCaveatByName(context.Background(), "somecaveat")
	require.NoError(t, err)
	require.Equal(t, "somecaveat", caveatDef.Name)

	// Attempt to read at revision 1, which should require a read.
	_, _, err = cache.SnapshotReader(rev("1")).ReadCaveatByName(context.Background(), "somecaveat")
	require.ErrorContains(t, err, "reads are disabled")

	// Close the proxy and ensure the background goroutines are terminated.
	cache.Close()
	time.Sleep(10 * time.Millisecond)
}

func TestWatchingCacheParallelOperations(t *testing.T) {
	defer goleak.VerifyNone(t, goleakIgnores...)

	fakeDS := &fakeDatastore{
		headRevision: rev("0"),
		namespaces:   map[string][]fakeEntry[datastore.RevisionedNamespace, *corev1.NamespaceDefinition]{},
		caveats:      map[string][]fakeEntry[datastore.RevisionedCaveat, *corev1.CaveatDefinition]{},
		schemaChan:   make(chan *datastore.SchemaState, 1),
		errChan:      make(chan error, 1),
	}

	cache := createWatchingCacheProxy(fakeDS, 1*time.Hour)
	require.NoError(t, cache.Start(context.Background()))

	// Run some operations in parallel.
	var wg sync.WaitGroup
	wg.Add(2)

	go (func() {
		// Read somenamespace (which should not be found)
		_, _, err := cache.SnapshotReader(rev("1")).ReadNamespaceByName(context.Background(), "somenamespace")
		require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{})
		require.False(t, cache.namespaceCache.inFallbackMode)

		// Write somenamespace.
		fakeDS.updateNamespace("somenamespace", &corev1.NamespaceDefinition{Name: "somenamespace"}, rev("2"))

		// Read again (which should be found now)
		nsDef, _, err := cache.SnapshotReader(rev("2")).ReadNamespaceByName(context.Background(), "somenamespace")
		require.NoError(t, err)
		require.Equal(t, "somenamespace", nsDef.Name)

		wg.Done()
	})()

	go (func() {
		// Read anothernamespace (which should not be found)
		_, _, err := cache.SnapshotReader(rev("1")).ReadNamespaceByName(context.Background(), "anothernamespace")
		require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{})
		require.False(t, cache.namespaceCache.inFallbackMode)

		// Read again (which should still not be found)
		_, _, err = cache.SnapshotReader(rev("3")).ReadNamespaceByName(context.Background(), "anothernamespace")
		require.ErrorAs(t, err, &datastore.ErrNamespaceNotFound{})
		require.False(t, cache.namespaceCache.inFallbackMode)

		wg.Done()
	})()

	wg.Wait()

	// Close the proxy and ensure the background goroutines are terminated.
	cache.Close()
	time.Sleep(10 * time.Millisecond)
}

func TestWatchingCacheParallelReaderWriter(t *testing.T) {
	defer goleak.VerifyNone(t, goleakIgnores...)

	fakeDS := &fakeDatastore{
		headRevision: rev("0"),
		namespaces:   map[string][]fakeEntry[datastore.RevisionedNamespace, *corev1.NamespaceDefinition]{},
		caveats:      map[string][]fakeEntry[datastore.RevisionedCaveat, *corev1.CaveatDefinition]{},
		schemaChan:   make(chan *datastore.SchemaState, 1),
		errChan:      make(chan error, 1),
	}

	cache := createWatchingCacheProxy(fakeDS, 1*time.Hour)
	require.NoError(t, cache.Start(context.Background()))

	// Write somenamespace.
	fakeDS.updateNamespace("somenamespace", &corev1.NamespaceDefinition{Name: "somenamespace"}, rev("0"))

	// Run some operations in parallel.
	var wg sync.WaitGroup
	wg.Add(2)

	go (func() {
		// Start a loop to write a namespace a bunch of times.
		for i := 0; i < 1000; i++ {
			// Write somenamespace.
			fakeDS.updateNamespace("somenamespace", &corev1.NamespaceDefinition{Name: "somenamespace"}, rev(fmt.Sprintf("%d", i+1)))
		}

		wg.Done()
	})()

	go (func() {
		// Start a loop to read a namespace a bunch of times.
		for i := 0; i < 1000; i++ {
			headRevision, err := fakeDS.HeadRevision(context.Background())
			require.NoError(t, err)

			nsDef, _, err := cache.SnapshotReader(headRevision).ReadNamespaceByName(context.Background(), "somenamespace")
			require.NoError(t, err)
			require.Equal(t, "somenamespace", nsDef.Name)
		}

		wg.Done()
	})()

	wg.Wait()

	// Close the proxy and ensure the background goroutines are terminated.
	cache.Close()
	time.Sleep(10 * time.Millisecond)
}

type fakeDatastore struct {
	headRevision datastore.Revision

	namespaces map[string][]fakeEntry[datastore.RevisionedNamespace, *corev1.NamespaceDefinition]
	caveats    map[string][]fakeEntry[datastore.RevisionedCaveat, *corev1.CaveatDefinition]

	schemaChan chan *datastore.SchemaState
	errChan    chan error

	readsDisabled bool

	lock sync.RWMutex
}

func (fds *fakeDatastore) updateNamespace(name string, def *corev1.NamespaceDefinition, revision datastore.Revision) {
	fds.lock.Lock()
	defer fds.lock.Unlock()

	updateDef(fds.namespaces, name, def, def == nil, revision, fds.schemaChan)
	fds.headRevision = revision
}

func (fds *fakeDatastore) updateCaveat(name string, def *corev1.CaveatDefinition, revision datastore.Revision) {
	fds.lock.Lock()
	defer fds.lock.Unlock()

	updateDef(fds.caveats, name, def, def == nil, revision, fds.schemaChan)
	fds.headRevision = revision
}

func (fds *fakeDatastore) sendCheckpoint(revision datastore.Revision) {
	fds.schemaChan <- &datastore.SchemaState{
		Revision:     revision,
		IsCheckpoint: true,
	}
	time.Sleep(1 * time.Millisecond)
}

type fakeEntry[T datastore.RevisionedDefinition[Q], Q datastore.SchemaDefinition] struct {
	value      T
	wasDeleted bool
}

type revisionGetter[T datastore.SchemaDefinition] interface {
	datastore.RevisionedDefinition[T]
	GetLastWrittenRevision() datastore.Revision
}

func updateDef[T datastore.SchemaDefinition](
	defs map[string][]fakeEntry[datastore.RevisionedDefinition[T], T],
	name string,
	def T,
	isDelete bool,
	revision datastore.Revision,
	schemaChan chan *datastore.SchemaState,
) {
	slice, ok := defs[name]
	if !ok {
		slice = []fakeEntry[datastore.RevisionedDefinition[T], T]{}
	}

	slice = append(slice, fakeEntry[datastore.RevisionedDefinition[T], T]{
		value: datastore.RevisionedDefinition[T]{
			Definition:          def,
			LastWrittenRevision: revision,
		},
		wasDeleted: isDelete,
	})
	defs[name] = slice

	if isDelete {
		schemaChan <- &datastore.SchemaState{
			Revision:          revision,
			DeletedNamespaces: []string{name},
		}
	} else {
		schemaChan <- &datastore.SchemaState{
			Revision:           revision,
			ChangedDefinitions: []datastore.SchemaDefinition{def},
		}
	}
	time.Sleep(1 * time.Millisecond)
}

func readDefs[T datastore.SchemaDefinition, Q revisionGetter[T]](defs map[string][]fakeEntry[Q, T], names []string, revision datastore.Revision) []Q {
	results := make([]Q, 0, len(names))
	for _, name := range names {
		revisionedDefs, ok := defs[name]
		if !ok {
			continue
		}

		revisioned := []fakeEntry[Q, T]{}
		for _, revisionedEntry := range revisionedDefs {
			if revisionedEntry.value.GetLastWrittenRevision().LessThan(revision) || revisionedEntry.value.GetLastWrittenRevision().Equal(revision) {
				revisioned = append(revisioned, revisionedEntry)
			}
		}

		if len(revisioned) == 0 {
			continue
		}

		slices.SortFunc(revisioned, func(a fakeEntry[Q, T], b fakeEntry[Q, T]) int {
			if a.value.GetLastWrittenRevision().Equal(b.value.GetLastWrittenRevision()) {
				return 0
			}

			if a.value.GetLastWrittenRevision().LessThan(b.value.GetLastWrittenRevision()) {
				return -1
			}

			return 1
		})

		entry := revisioned[len(revisioned)-1]
		if !entry.wasDeleted {
			results = append(results, entry.value)
		}
	}

	return results
}

func (fds *fakeDatastore) readNamespaces(names []string, revision datastore.Revision) ([]datastore.RevisionedNamespace, error) {
	fds.lock.RLock()
	defer fds.lock.RUnlock()

	if fds.readsDisabled {
		return nil, fmt.Errorf("reads are disabled")
	}

	return readDefs(fds.namespaces, names, revision), nil
}

func (fds *fakeDatastore) readCaveats(names []string, revision datastore.Revision) ([]datastore.RevisionedCaveat, error) {
	fds.lock.RLock()
	defer fds.lock.RUnlock()

	if fds.readsDisabled {
		return nil, fmt.Errorf("reads are disabled")
	}

	return readDefs(fds.caveats, names, revision), nil
}

func (fds *fakeDatastore) disableReads() {
	fds.lock.Lock()
	defer fds.lock.Unlock()

	fds.readsDisabled = true
}

func (fds *fakeDatastore) enableReads() {
	fds.lock.Lock()
	defer fds.lock.Unlock()

	fds.readsDisabled = false
}

func (fds *fakeDatastore) SnapshotReader(rev datastore.Revision) datastore.Reader {
	return &fakeSnapshotReader{fds, rev}
}

func (fds *fakeDatastore) WatchSchema(context.Context, datastore.Revision) (<-chan *datastore.SchemaState, <-chan error) {
	return fds.schemaChan, fds.errChan
}

func (fds *fakeDatastore) HeadRevision(context.Context) (datastore.Revision, error) {
	fds.lock.RLock()
	defer fds.lock.RUnlock()

	return fds.headRevision, nil
}

func (*fakeDatastore) ReadWriteTx(context.Context, datastore.TxUserFunc, ...options.RWTOptionsOption) (datastore.Revision, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeDatastore) CheckRevision(context.Context, datastore.Revision) error {
	return nil
}

func (*fakeDatastore) Close() error {
	return nil
}

func (*fakeDatastore) Features(context.Context) (*datastore.Features, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeDatastore) OptimizedRevision(context.Context) (datastore.Revision, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeDatastore) ReadyState(context.Context) (datastore.ReadyState, error) {
	return datastore.ReadyState{}, fmt.Errorf("not implemented")
}

func (*fakeDatastore) RevisionFromString(string) (datastore.Revision, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeDatastore) Statistics(context.Context) (datastore.Stats, error) {
	return datastore.Stats{}, fmt.Errorf("not implemented")
}

func (*fakeDatastore) Watch(context.Context, datastore.Revision) (<-chan *datastore.RevisionChanges, <-chan error) {
	return nil, nil
}

type fakeSnapshotReader struct {
	fds *fakeDatastore
	rev datastore.Revision
}

func (fsr *fakeSnapshotReader) LookupNamespacesWithNames(_ context.Context, nsNames []string) ([]datastore.RevisionedDefinition[*corev1.NamespaceDefinition], error) {
	return fsr.fds.readNamespaces(nsNames, fsr.rev)
}

func (fsr *fakeSnapshotReader) ReadNamespaceByName(_ context.Context, nsName string) (ns *corev1.NamespaceDefinition, lastWritten datastore.Revision, err error) {
	namespaces, err := fsr.fds.readNamespaces([]string{nsName}, fsr.rev)
	if err != nil {
		return nil, nil, err
	}

	if len(namespaces) == 0 {
		return nil, nil, datastore.NewNamespaceNotFoundErr(nsName)
	}
	return namespaces[0].Definition, namespaces[0].LastWrittenRevision, nil
}

func (fsr *fakeSnapshotReader) LookupCaveatsWithNames(_ context.Context, names []string) ([]datastore.RevisionedDefinition[*corev1.CaveatDefinition], error) {
	return fsr.fds.readCaveats(names, fsr.rev)
}

func (fsr *fakeSnapshotReader) ReadCaveatByName(_ context.Context, name string) (caveat *corev1.CaveatDefinition, lastWritten datastore.Revision, err error) {
	caveats, err := fsr.fds.readCaveats([]string{name}, fsr.rev)
	if err != nil {
		return nil, nil, err
	}

	if len(caveats) == 0 {
		return nil, nil, datastore.NewCaveatNameNotFoundErr(name)
	}
	return caveats[0].Definition, caveats[0].LastWrittenRevision, nil
}

func (*fakeSnapshotReader) ListAllCaveats(context.Context) ([]datastore.RevisionedDefinition[*corev1.CaveatDefinition], error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeSnapshotReader) ListAllNamespaces(context.Context) ([]datastore.RevisionedDefinition[*corev1.NamespaceDefinition], error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeSnapshotReader) QueryRelationships(context.Context, datastore.RelationshipsFilter, ...options.QueryOptionsOption) (datastore.RelationshipIterator, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*fakeSnapshotReader) ReverseQueryRelationships(context.Context, datastore.SubjectsFilter, ...options.ReverseQueryOptionsOption) (datastore.RelationshipIterator, error) {
	return nil, fmt.Errorf("not implemented")
}
