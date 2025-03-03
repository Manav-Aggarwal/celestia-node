package eds

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/dagstore"
	"github.com/filecoin-project/dagstore/index"
	"github.com/filecoin-project/dagstore/mount"
	"github.com/filecoin-project/dagstore/shard"
	bstore "github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/go-datastore"
	carv1 "github.com/ipld/go-car"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/celestiaorg/rsmt2d"

	"github.com/celestiaorg/celestia-node/header"
	"github.com/celestiaorg/celestia-node/libs/utils"
	"github.com/celestiaorg/celestia-node/share"
	"github.com/celestiaorg/celestia-node/share/ipld"
)

const (
	blocksPath     = "/blocks/"
	indexPath      = "/index/"
	transientsPath = "/transients/"

	// GC performs DAG store garbage collection by reclaiming transient files of
	// shards that are currently available but inactive, or errored.
	// We don't use transient files right now, so GC is turned off by default.
	defaultGCInterval = 0
)

var ErrNotFound = errors.New("eds not found in store")

// Store maintains (via DAGStore) a top-level index enabling granular and efficient random access to
// every share and/or Merkle proof over every registered CARv1 file. The EDSStore provides a custom
// blockstore interface implementation to achieve access. The main use-case is randomized sampling
// over the whole chain of EDS block data and getting data by namespace.
type Store struct {
	cancel context.CancelFunc

	dgstr  *dagstore.DAGStore
	mounts *mount.Registry

	cache *blockstoreCache
	bs    bstore.Blockstore

	carIdx      index.FullIndexRepo
	invertedIdx *simpleInvertedIndex

	basepath   string
	gcInterval time.Duration
	// lastGCResult is only stored on the store for testing purposes.
	lastGCResult atomic.Pointer[dagstore.GCResult]

	metrics *metrics
}

// NewStore creates a new EDS Store under the given basepath and datastore.
func NewStore(basepath string, ds datastore.Batching) (*Store, error) {
	err := setupPath(basepath)
	if err != nil {
		return nil, fmt.Errorf("failed to setup eds.Store directories: %w", err)
	}

	r := mount.NewRegistry()
	err = r.Register("fs", &inMemoryOnceMount{})
	if err != nil {
		return nil, fmt.Errorf("failed to register memory mount on the registry: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to register FS mount on the registry: %w", err)
	}

	fsRepo, err := index.NewFSRepo(basepath + indexPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create index repository: %w", err)
	}

	invertedIdx, err := newSimpleInvertedIndex(basepath)
	if err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}
	dagStore, err := dagstore.NewDAGStore(
		dagstore.Config{
			TransientsDir: basepath + transientsPath,
			IndexRepo:     fsRepo,
			Datastore:     ds,
			MountRegistry: r,
			TopLevelIndex: invertedIdx,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DAGStore: %w", err)
	}

	cache, err := newBlockstoreCache(defaultCacheSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create blockstore cache: %w", err)
	}

	store := &Store{
		basepath:    basepath,
		dgstr:       dagStore,
		carIdx:      fsRepo,
		invertedIdx: invertedIdx,
		gcInterval:  defaultGCInterval,
		mounts:      r,
		cache:       cache,
	}
	store.bs = newBlockstore(store, cache, ds)
	return store, nil
}

func (s *Store) Start(ctx context.Context) error {
	err := s.dgstr.Start(ctx)
	if err != nil {
		return err
	}
	// start Store only if DagStore succeeds
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	// initialize empty gc result to avoid panic on access
	s.lastGCResult.Store(&dagstore.GCResult{
		Shards: make(map[shard.Key]error),
	})

	if s.gcInterval != 0 {
		go s.gc(runCtx)
	}
	return nil
}

// Stop stops the underlying DAGStore.
func (s *Store) Stop(context.Context) error {
	defer s.cancel()
	if err := s.invertedIdx.close(); err != nil {
		return err
	}
	return s.dgstr.Close()
}

// gc periodically removes all inactive or errored shards.
func (s *Store) gc(ctx context.Context) {
	ticker := time.NewTicker(s.gcInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tnow := time.Now()
			res, err := s.dgstr.GC(ctx)
			s.metrics.observeGCtime(ctx, time.Since(tnow), err != nil)
			if err != nil {
				log.Errorf("garbage collecting dagstore: %v", err)
				return
			}
			s.lastGCResult.Store(res)
		}

	}
}

// Put stores the given data square with DataRoot's hash as a key.
//
// The square is verified on the Exchange level, and Put only stores the square, trusting it.
// The resulting file stores all the shares and NMT Merkle Proofs of the EDS.
// Additionally, the file gets indexed s.t. store.Blockstore can access them.
func (s *Store) Put(ctx context.Context, root share.DataHash, square *rsmt2d.ExtendedDataSquare) error {
	ctx, span := tracer.Start(ctx, "store/put", trace.WithAttributes(
		attribute.Int("width", int(square.Width())),
	))

	tnow := time.Now()
	err := s.put(ctx, root, square)
	result := putOK
	switch {
	case errors.Is(err, dagstore.ErrShardExists):
		result = putExists
	case err != nil:
		result = putFailed
	}
	utils.SetStatusAndEnd(span, err)
	s.metrics.observePut(ctx, time.Since(tnow), result, square.Width())
	return err
}

func (s *Store) put(ctx context.Context, root share.DataHash, square *rsmt2d.ExtendedDataSquare) (err error) {
	// if root already exists, short-circuit
	if has, _ := s.Has(ctx, root); has {
		return dagstore.ErrShardExists
	}

	key := root.String()
	f, err := os.OpenFile(s.basepath+blocksPath+key, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// save encoded eds into buffer
	mount := &inMemoryOnceMount{
		// TODO: buffer could be pre-allocated with capacity calculated based on eds size.
		buf:       bytes.NewBuffer(nil),
		FileMount: mount.FileMount{Path: s.basepath + blocksPath + key},
	}
	err = WriteEDS(ctx, square, mount)
	if err != nil {
		return fmt.Errorf("failed to write EDS to file: %w", err)
	}

	// write whole buffered mount data in one go to optimize i/o
	if _, err = mount.WriteTo(f); err != nil {
		return fmt.Errorf("failed to write EDS to file: %w", err)
	}

	ch := make(chan dagstore.ShardResult, 1)
	err = s.dgstr.RegisterShard(ctx, shard.KeyFromString(key), mount, ch, dagstore.RegisterOpts{})
	if err != nil {
		return fmt.Errorf("failed to initiate shard registration: %w", err)
	}

	select {
	case <-ctx.Done():
		// if context finished before result was received, track result in separate goroutine
		go trackLateResult("put", ch, s.metrics, time.Minute*5)
		return ctx.Err()
	case result := <-ch:
		if result.Error != nil {
			return fmt.Errorf("failed to register shard: %w", result.Error)
		}
		return nil
	}
}

// waitForResult waits for a result from the res channel for a maximum duration specified by
// maxWait. If the result is not received within the specified duration, it logs an error
// indicating that the parent context has expired and the shard registration is stuck. If a result
// is received, it checks for any error and logs appropriate messages.
func trackLateResult(opName string, res <-chan dagstore.ShardResult, metrics *metrics, maxWait time.Duration) {
	tnow := time.Now()
	select {
	case <-time.After(maxWait):
		metrics.observeLongOp(context.Background(), opName, time.Since(tnow), longOpUnresolved)
		log.Errorf("parent context is expired, while register shard is stuck for more than %v sec", time.Since(tnow))
		return
	case result := <-res:
		// don't observe if result was received right after launch of the func
		if time.Since(tnow) < time.Second {
			return
		}
		if result.Error != nil {
			metrics.observeLongOp(context.Background(), opName, time.Since(tnow), longOpFailed)
			log.Errorf("failed to register shard after context expired: %v ago, err: %w", time.Since(tnow), result.Error)
			return
		}
		metrics.observeLongOp(context.Background(), opName, time.Since(tnow), longOpOK)
		log.Warnf("parent context expired, but register shard finished with no error,"+
			" after context expired: %v ago", time.Since(tnow))
		return
	}
}

// GetCAR takes a DataRoot and returns a buffered reader to the respective EDS serialized as a
// CARv1 file.
// The Reader strictly reads the CAR header and first quadrant (1/4) of the EDS, omitting all the
// NMT Merkle proofs. Integrity of the store data is not verified.
//
// The shard is cached in the Store, so subsequent calls to GetCAR with the same root will use the
// same reader. The cache is responsible for closing the underlying reader.
func (s *Store) GetCAR(ctx context.Context, root share.DataHash) (io.Reader, error) {
	ctx, span := tracer.Start(ctx, "store/get-car")
	tnow := time.Now()
	r, err := s.getCAR(ctx, root)
	s.metrics.observeGetCAR(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return r, err
}

func (s *Store) getCAR(ctx context.Context, root share.DataHash) (io.Reader, error) {
	key := root.String()
	accessor, err := s.getCachedAccessor(ctx, shard.KeyFromString(key))
	if err != nil {
		return nil, fmt.Errorf("failed to get accessor: %w", err)
	}
	return accessor.sa.Reader(), nil
}

// Blockstore returns an IPFS blockstore providing access to individual shares/nodes of all EDS
// registered on the Store. NOTE: The blockstore does not store whole Celestia Blocks but IPFS
// blocks. We represent `shares` and NMT Merkle proofs as IPFS blocks and IPLD nodes so Bitswap can
// access those.
func (s *Store) Blockstore() bstore.Blockstore {
	return s.bs
}

// CARBlockstore returns an IPFS Blockstore providing access to individual shares/nodes of a
// specific EDS identified by DataHash and registered on the Store. NOTE: The Blockstore does not
// store whole Celestia Blocks but IPFS blocks. We represent `shares` and NMT Merkle proofs as IPFS
// blocks and IPLD nodes so Bitswap can access those.
func (s *Store) CARBlockstore(
	ctx context.Context,
	root share.DataHash,
) (dagstore.ReadBlockstore, error) {
	ctx, span := tracer.Start(ctx, "store/car-blockstore")
	tnow := time.Now()
	r, err := s.carBlockstore(ctx, root)
	s.metrics.observeCARBlockstore(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return r, err
}

func (s *Store) carBlockstore(
	ctx context.Context,
	root share.DataHash,
) (dagstore.ReadBlockstore, error) {
	key := shard.KeyFromString(root.String())
	accessor, err := s.getCachedAccessor(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("eds/store: failed to get accessor: %w", err)
	}
	return accessor.bs, nil
}

// GetDAH returns the DataAvailabilityHeader for the EDS identified by DataHash.
func (s *Store) GetDAH(ctx context.Context, root share.DataHash) (*share.Root, error) {
	ctx, span := tracer.Start(ctx, "store/car-dah")
	tnow := time.Now()
	r, err := s.getDAH(ctx, root)
	s.metrics.observeGetDAH(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return r, err
}

func (s *Store) getDAH(ctx context.Context, root share.DataHash) (*share.Root, error) {
	key := shard.KeyFromString(root.String())
	accessor, err := s.getCachedAccessor(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("eds/store: failed to get accessor: %w", err)
	}

	carHeader, err := carv1.ReadHeader(bufio.NewReader(accessor.sa.Reader()))
	if err != nil {
		return nil, fmt.Errorf("eds/store: failed to read car header: %w", err)
	}

	dah := dahFromCARHeader(carHeader)
	if !bytes.Equal(dah.Hash(), root) {
		return nil, fmt.Errorf("eds/store: content integrity mismatch from CAR for root %x", root)
	}
	return dah, nil
}

// dahFromCARHeader returns the DataAvailabilityHeader stored in the CIDs of a CARv1 header.
func dahFromCARHeader(carHeader *carv1.CarHeader) *header.DataAvailabilityHeader {
	rootCount := len(carHeader.Roots)
	rootBytes := make([][]byte, 0, rootCount)
	for _, root := range carHeader.Roots {
		rootBytes = append(rootBytes, ipld.NamespacedSha256FromCID(root))
	}
	return &header.DataAvailabilityHeader{
		RowRoots:    rootBytes[:rootCount/2],
		ColumnRoots: rootBytes[rootCount/2:],
	}
}

func (s *Store) getAccessor(ctx context.Context, key shard.Key) (*dagstore.ShardAccessor, error) {
	ch := make(chan dagstore.ShardResult, 1)
	err := s.dgstr.AcquireShard(ctx, key, ch, dagstore.AcquireOpts{})
	if err != nil {
		if errors.Is(err, dagstore.ErrShardUnknown) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to initialize shard acquisition: %w", err)
	}

	select {
	case res := <-ch:
		if res.Error != nil {
			return nil, fmt.Errorf("failed to acquire shard: %w", res.Error)
		}
		return res.Accessor, nil
	case <-ctx.Done():
		go trackLateResult("get_shard", ch, s.metrics, time.Minute)
		return nil, ctx.Err()
	}
}

func (s *Store) getCachedAccessor(ctx context.Context, key shard.Key) (*accessorWithBlockstore, error) {
	lk := &s.cache.stripedLocks[shardKeyToStriped(key)]
	lk.Lock()
	defer lk.Unlock()

	tnow := time.Now()
	accessor, err := s.cache.unsafeGet(key)
	if err != nil && err != errCacheMiss {
		log.Errorf("unexpected error while reading key from bs cache %s: %s", key, err)
	}
	if accessor != nil {
		s.metrics.observeGetAccessor(ctx, time.Since(tnow), true, false)
		return accessor, nil
	}

	// wasn't found in cache, so acquire it and add to cache
	shardAccessor, err := s.getAccessor(ctx, key)
	if err != nil {
		s.metrics.observeGetAccessor(ctx, time.Since(tnow), false, err != nil)
		return nil, err
	}

	a, err := s.cache.unsafeAdd(key, shardAccessor)
	s.metrics.observeGetAccessor(ctx, time.Since(tnow), false, err != nil)
	return a, err
}

// Remove removes EDS from Store by the given share.Root hash and cleans up all
// the indexing.
func (s *Store) Remove(ctx context.Context, root share.DataHash) error {
	ctx, span := tracer.Start(ctx, "store/remove")
	tnow := time.Now()
	err := s.remove(ctx, root)
	s.metrics.observeRemove(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return err
}

func (s *Store) remove(ctx context.Context, root share.DataHash) (err error) {
	key := root.String()
	ch := make(chan dagstore.ShardResult, 1)
	err = s.dgstr.DestroyShard(ctx, shard.KeyFromString(key), ch, dagstore.DestroyOpts{})
	if err != nil {
		return fmt.Errorf("failed to initiate shard destruction: %w", err)
	}

	select {
	case result := <-ch:
		if result.Error != nil {
			return fmt.Errorf("failed to destroy shard: %w", result.Error)
		}
	case <-ctx.Done():
		go trackLateResult("remove", ch, s.metrics, time.Minute)
		return ctx.Err()
	}

	dropped, err := s.carIdx.DropFullIndex(shard.KeyFromString(key))
	if !dropped {
		log.Warnf("failed to drop index for %s", key)
	}
	if err != nil {
		return fmt.Errorf("failed to drop index for %s: %w", key, err)
	}

	err = os.Remove(s.basepath + blocksPath + key)
	if err != nil {
		return fmt.Errorf("failed to remove CAR file: %w", err)
	}
	return nil
}

// Get reads EDS out of Store by given DataRoot.
//
// It reads only one quadrant(1/4) of the EDS and verifies the integrity of the stored data by
// recomputing it.
func (s *Store) Get(ctx context.Context, root share.DataHash) (*rsmt2d.ExtendedDataSquare, error) {
	ctx, span := tracer.Start(ctx, "store/get")
	tnow := time.Now()
	eds, err := s.get(ctx, root)
	s.metrics.observeGet(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return eds, err
}

func (s *Store) get(ctx context.Context, root share.DataHash) (eds *rsmt2d.ExtendedDataSquare, err error) {
	ctx, span := tracer.Start(ctx, "store/get")
	defer func() {
		utils.SetStatusAndEnd(span, err)
	}()

	f, err := s.GetCAR(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("failed to get CAR file: %w", err)
	}
	eds, err = ReadEDS(ctx, f, root)
	if err != nil {
		return nil, fmt.Errorf("failed to read EDS from CAR file: %w", err)
	}
	return eds, nil
}

// Has checks if EDS exists by the given share.Root hash.
func (s *Store) Has(ctx context.Context, root share.DataHash) (has bool, err error) {
	ctx, span := tracer.Start(ctx, "store/has")
	tnow := time.Now()
	eds, err := s.has(ctx, root)
	s.metrics.observeHas(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return eds, err
}

func (s *Store) has(_ context.Context, root share.DataHash) (bool, error) {
	key := root.String()
	info, err := s.dgstr.GetShardInfo(shard.KeyFromString(key))
	switch err {
	case nil:
		return true, info.Error
	case dagstore.ErrShardUnknown:
		return false, info.Error
	default:
		return false, err
	}
}

// List lists all the registered EDSes.
func (s *Store) List() ([]share.DataHash, error) {
	ctx, span := tracer.Start(context.Background(), "store/list")
	tnow := time.Now()
	hashes, err := s.list()
	s.metrics.observeList(ctx, time.Since(tnow), err != nil)
	utils.SetStatusAndEnd(span, err)
	return hashes, err
}

func (s *Store) list() ([]share.DataHash, error) {
	shards := s.dgstr.AllShardsInfo()
	hashes := make([]share.DataHash, 0, len(shards))
	for shrd := range shards {
		hash, err := hex.DecodeString(shrd.String())
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	return hashes, nil
}

func setupPath(basepath string) error {
	err := os.MkdirAll(basepath+blocksPath, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create blocks directory: %w", err)
	}
	err = os.MkdirAll(basepath+transientsPath, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create transients directory: %w", err)
	}
	err = os.MkdirAll(basepath+indexPath, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create index directory: %w", err)
	}
	return nil
}

// inMemoryOnceMount is used to allow reading once from buffer before using main mount.Reader
type inMemoryOnceMount struct {
	buf *bytes.Buffer

	readOnce atomic.Bool
	mount.FileMount
}

func (m *inMemoryOnceMount) Fetch(ctx context.Context) (mount.Reader, error) {
	if m.buf != nil && !m.readOnce.Swap(true) {
		reader := &inMemoryReader{Reader: bytes.NewReader(m.buf.Bytes())}
		// release memory for gc, otherwise buffer will stick forever
		m.buf = nil
		return reader, nil
	}
	return m.FileMount.Fetch(ctx)
}

func (m *inMemoryOnceMount) Write(b []byte) (int, error) {
	return m.buf.Write(b)
}

func (m *inMemoryOnceMount) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, bytes.NewReader(m.buf.Bytes()))
}

// inMemoryReader extends bytes.Reader to implement mount.Reader interface
type inMemoryReader struct {
	*bytes.Reader
}

// Close allows inMemoryReader to satisfy mount.Reader interface
func (r *inMemoryReader) Close() error {
	return nil
}
