package getters

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/ipfs/go-datastore"
	ds_sync "github.com/ipfs/go-datastore/sync"
	routinghelpers "github.com/libp2p/go-libp2p-routing-helpers"
	"github.com/libp2p/go-libp2p/core/host"
	routingdisc "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/net/conngater"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/stretchr/testify/require"

	"github.com/celestiaorg/celestia-app/pkg/da"
	"github.com/celestiaorg/celestia-app/pkg/wrapper"
	libhead "github.com/celestiaorg/go-header"
	"github.com/celestiaorg/nmt"
	"github.com/celestiaorg/rsmt2d"

	"github.com/celestiaorg/celestia-node/header"
	"github.com/celestiaorg/celestia-node/header/headertest"
	"github.com/celestiaorg/celestia-node/share"
	"github.com/celestiaorg/celestia-node/share/eds"
	"github.com/celestiaorg/celestia-node/share/eds/edstest"
	"github.com/celestiaorg/celestia-node/share/p2p/discovery"
	"github.com/celestiaorg/celestia-node/share/p2p/peers"
	"github.com/celestiaorg/celestia-node/share/p2p/shrexeds"
	"github.com/celestiaorg/celestia-node/share/p2p/shrexnd"
	"github.com/celestiaorg/celestia-node/share/p2p/shrexsub"
	"github.com/celestiaorg/celestia-node/share/sharetest"
)

func TestShrexGetter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	t.Cleanup(cancel)

	// create test net
	net, err := mocknet.FullMeshConnected(2)
	require.NoError(t, err)
	clHost, srvHost := net.Hosts()[0], net.Hosts()[1]

	// launch eds store and put test data into it
	edsStore, err := newStore(t)
	require.NoError(t, err)
	err = edsStore.Start(ctx)
	require.NoError(t, err)

	ndClient, _ := newNDClientServer(ctx, t, edsStore, srvHost, clHost)
	edsClient, _ := newEDSClientServer(ctx, t, edsStore, srvHost, clHost)

	// create shrex Getter
	sub := new(headertest.Subscriber)
	peerManager, err := testManager(ctx, clHost, sub)
	require.NoError(t, err)
	getter := NewShrexGetter(edsClient, ndClient, peerManager)
	require.NoError(t, getter.Start(ctx))

	t.Run("ND_Available, total data size > 1mb", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second*10)
		t.Cleanup(cancel)

		// generate test data
		namespace := sharetest.RandV0Namespace()
		randEDS, dah := singleNamespaceEds(t, namespace, 64)
		require.NoError(t, edsStore.Put(ctx, dah.Hash(), randEDS))
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		got, err := getter.GetSharesByNamespace(ctx, &dah, namespace)
		require.NoError(t, err)
		require.NoError(t, got.Verify(&dah, namespace))
	})

	t.Run("ND_err_not_found", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)

		// generate test data
		_, dah, namespace := generateTestEDS(t)
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		_, err := getter.GetSharesByNamespace(ctx, &dah, namespace)
		require.ErrorIs(t, err, share.ErrNotFound)
	})

	t.Run("ND_namespace_not_included", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)

		// generate test data
		eds, dah, maxNamespace := generateTestEDS(t)
		require.NoError(t, edsStore.Put(ctx, dah.Hash(), eds))
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		namespace, err := addToNamespace(maxNamespace, -1)
		require.NoError(t, err)
		// check for namespace to be between max and min namespace in root
		require.Len(t, filterRootsByNamespace(&dah, namespace), 1)

		emptyShares, err := getter.GetSharesByNamespace(ctx, &dah, namespace)
		require.NoError(t, err)
		// no shares should be returned
		require.Empty(t, emptyShares.Flatten())
		require.Nil(t, emptyShares.Verify(&dah, namespace))
	})

	t.Run("ND_namespace_not_in_dah", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)

		// generate test data
		eds, dah, maxNamesapce := generateTestEDS(t)
		require.NoError(t, edsStore.Put(ctx, dah.Hash(), eds))
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		namespace, err := addToNamespace(maxNamesapce, 1)
		require.NoError(t, err)
		// check for namespace to be not in root
		require.Len(t, filterRootsByNamespace(&dah, namespace), 0)

		emptyShares, err := getter.GetSharesByNamespace(ctx, &dah, namespace)
		require.NoError(t, err)
		// no shares should be returned
		require.Empty(t, emptyShares.Flatten())
		require.Nil(t, emptyShares.Verify(&dah, namespace))
	})

	t.Run("EDS_Available", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)

		// generate test data
		randEDS, dah, _ := generateTestEDS(t)
		require.NoError(t, edsStore.Put(ctx, dah.Hash(), randEDS))
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		got, err := getter.GetEDS(ctx, &dah)
		require.NoError(t, err)
		require.Equal(t, randEDS.Flattened(), got.Flattened())
	})

	t.Run("EDS_ctx_deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)

		// generate test data
		_, dah, _ := generateTestEDS(t)
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		cancel()
		_, err := getter.GetEDS(ctx, &dah)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("EDS_err_not_found", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		t.Cleanup(cancel)

		// generate test data
		_, dah, _ := generateTestEDS(t)
		peerManager.Validate(ctx, srvHost.ID(), shrexsub.Notification{
			DataHash: dah.Hash(),
			Height:   1,
		})

		_, err := getter.GetEDS(ctx, &dah)
		require.ErrorIs(t, err, share.ErrNotFound)
	})
}

func newStore(t *testing.T) (*eds.Store, error) {
	t.Helper()

	tmpDir := t.TempDir()
	ds := ds_sync.MutexWrap(datastore.NewMapDatastore())
	return eds.NewStore(tmpDir, ds)
}

func generateTestEDS(t *testing.T) (*rsmt2d.ExtendedDataSquare, da.DataAvailabilityHeader, share.Namespace) {
	eds := edstest.RandEDS(t, 4)
	dah, err := da.NewDataAvailabilityHeader(eds)
	require.NoError(t, err)
	max := nmt.MaxNamespace(dah.RowRoots[(len(dah.RowRoots))/2-1], share.NamespaceSize)
	return eds, dah, max
}

func testManager(
	ctx context.Context, host host.Host, headerSub libhead.Subscriber[*header.ExtendedHeader],
) (*peers.Manager, error) {
	shrexSub, err := shrexsub.NewPubSub(ctx, host, "test")
	if err != nil {
		return nil, err
	}

	disc := discovery.NewDiscovery(nil,
		routingdisc.NewRoutingDiscovery(routinghelpers.Null{}),
		discovery.WithPeersLimit(10),
		discovery.WithAdvertiseInterval(time.Second),
	)
	connGater, err := conngater.NewBasicConnectionGater(ds_sync.MutexWrap(datastore.NewMapDatastore()))
	if err != nil {
		return nil, err
	}
	manager, err := peers.NewManager(
		peers.DefaultParameters(),
		headerSub,
		shrexSub,
		disc,
		host,
		connGater,
	)
	return manager, err
}

func newNDClientServer(
	ctx context.Context, t *testing.T, edsStore *eds.Store, srvHost, clHost host.Host,
) (*shrexnd.Client, *shrexnd.Server) {
	params := shrexnd.DefaultParameters()

	// create server and register handler
	server, err := shrexnd.NewServer(params, srvHost, edsStore, NewStoreGetter(edsStore))
	require.NoError(t, err)
	require.NoError(t, server.Start(ctx))

	t.Cleanup(func() {
		_ = server.Stop(ctx)
	})

	// create client and connect it to server
	client, err := shrexnd.NewClient(params, clHost)
	require.NoError(t, err)
	return client, server
}

func newEDSClientServer(
	ctx context.Context, t *testing.T, edsStore *eds.Store, srvHost, clHost host.Host,
) (*shrexeds.Client, *shrexeds.Server) {
	params := shrexeds.DefaultParameters()

	// create server and register handler
	server, err := shrexeds.NewServer(params, srvHost, edsStore)
	require.NoError(t, err)
	require.NoError(t, server.Start(ctx))

	t.Cleanup(func() {
		_ = server.Stop(ctx)
	})

	// create client and connect it to server
	client, err := shrexeds.NewClient(params, clHost)
	require.NoError(t, err)
	return client, server
}

// addToNamespace adds arbitrary int value to namespace, treating namespace as big-endian
// implementation of int
func addToNamespace(namespace share.Namespace, val int) (share.Namespace, error) {
	if val == 0 {
		return namespace, nil
	}
	// Convert the input integer to a byte slice and add it to result slice
	result := make([]byte, len(namespace))
	if val > 0 {
		binary.BigEndian.PutUint64(result[len(namespace)-8:], uint64(val))
	} else {
		binary.BigEndian.PutUint64(result[len(namespace)-8:], uint64(-val))
	}

	// Perform addition byte by byte
	var carry int
	for i := len(namespace) - 1; i >= 0; i-- {
		sum := 0
		if val > 0 {
			sum = int(namespace[i]) + int(result[i]) + carry
		} else {
			sum = int(namespace[i]) - int(result[i]) + carry
		}

		switch {
		case sum > 255:
			carry = 1
			sum -= 256
		case sum < 0:
			carry = -1
			sum += 256
		default:
			carry = 0
		}

		result[i] = uint8(sum)
	}

	// Handle any remaining carry
	if carry != 0 {
		return nil, errors.New("namespace overflow")
	}

	return result, nil
}

func TestAddToNamespace(t *testing.T) {
	testCases := []struct {
		name          string
		value         int
		input         share.Namespace
		expected      share.Namespace
		expectedError error
	}{
		{
			name:          "Positive value addition",
			value:         42,
			input:         share.Namespace{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01},
			expected:      share.Namespace{0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x2b},
			expectedError: nil,
		},
		{
			name:          "Negative value addition",
			value:         -42,
			input:         share.Namespace{0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01},
			expected:      share.Namespace{0x1, 0x1, 0x1, 0x1, 0x1, 0x01, 0x1, 0x1, 0x1, 0x0, 0xd7},
			expectedError: nil,
		},
		{
			name:          "Overflow error",
			value:         1,
			input:         share.Namespace{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			expected:      nil,
			expectedError: errors.New("namespace overflow"),
		},
		{
			name:          "Overflow error negative",
			value:         -1,
			input:         share.Namespace{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0},
			expected:      nil,
			expectedError: errors.New("namespace overflow"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := addToNamespace(tc.input, tc.value)
			if tc.expectedError == nil {
				require.NoError(t, err)
				require.Equal(t, tc.expected, result)
				return
			}
			require.Error(t, err)
			if err.Error() != tc.expectedError.Error() {
				t.Errorf("Unexpected error message. Expected: %v, Got: %v", tc.expectedError, err)
			}
		})
	}
}

func singleNamespaceEds(
	t require.TestingT,
	namespace share.Namespace,
	size int,
) (*rsmt2d.ExtendedDataSquare, da.DataAvailabilityHeader) {
	shares := make([]share.Share, size*size)
	rnd := rand.New(rand.NewSource(time.Now().Unix()))
	for i := range shares {
		shr := make([]byte, share.Size)
		copy(share.GetNamespace(shr), namespace)
		_, err := rnd.Read(share.GetData(shr))
		require.NoError(t, err)
		shares[i] = shr
	}
	sort.Slice(shares, func(i, j int) bool { return bytes.Compare(shares[i], shares[j]) < 0 })
	eds, err := rsmt2d.ComputeExtendedDataSquare(shares, share.DefaultRSMT2DCodec(), wrapper.NewConstructor(uint64(size)))
	require.NoError(t, err, "failure to recompute the extended data square")
	dah, err := da.NewDataAvailabilityHeader(eds)
	require.NoError(t, err)
	return eds, dah
}
