package p2p

import (
	"bytes"
	"context"
	"testing"
	"time"

	libhost "github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tmbytes "github.com/tendermint/tendermint/libs/bytes"

	"github.com/celestiaorg/go-libp2p-messenger/serde"

	"github.com/celestiaorg/celestia-node/header"
	p2p_pb "github.com/celestiaorg/celestia-node/header/p2p/pb"
)

var privateProtocolID = protocolID("private")

func TestExchange_RequestHead(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, store := createP2PExAndServer(t, hosts[0], hosts[1])
	// perform header request
	header, err := exchg.Head(context.Background())
	require.NoError(t, err)

	assert.Equal(t, store.headers[store.headHeight].Height, header.Height)
	assert.Equal(t, store.headers[store.headHeight].Hash(), header.Hash())
}

func TestExchange_RequestHeader(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, store := createP2PExAndServer(t, hosts[0], hosts[1])
	// perform expected request
	header, err := exchg.GetByHeight(context.Background(), 5)
	require.NoError(t, err)
	assert.Equal(t, store.headers[5].Height, header.Height)
	assert.Equal(t, store.headers[5].Hash(), header.Hash())
}

func TestExchange_RequestHeaders(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, store := createP2PExAndServer(t, hosts[0], hosts[1])
	// perform expected request
	gotHeaders, err := exchg.GetRangeByHeight(context.Background(), 1, 5)
	require.NoError(t, err)
	for _, got := range gotHeaders {
		assert.Equal(t, store.headers[got.Height].Height, got.Height)
		assert.Equal(t, store.headers[got.Height].Hash(), got.Hash())
	}
}

func TestExchange_RequestVerifiedHeaders(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, store := createP2PExAndServer(t, hosts[0], hosts[1])
	// perform expected request
	h := store.headers[1]
	_, err := exchg.GetVerifiedRange(context.Background(), h, 3)
	require.NoError(t, err)
}

func TestExchange_RequestVerifiedHeadersFails(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, store := createP2PExAndServer(t, hosts[0], hosts[1])
	store.headers[2] = store.headers[3]
	// perform expected request
	h := store.headers[1]
	_, err := exchg.GetVerifiedRange(context.Background(), h, 3)
	require.Error(t, err)
}

// TestExchange_RequestFullRangeHeaders requests max amount of headers
// to verify how session will parallelize all requests.
func TestExchange_RequestFullRangeHeaders(t *testing.T) {
	// create mocknet with 5 peers
	hosts := createMocknet(t, 5)
	totalAmount := 80
	store := createStore(t, totalAmount)
	protocolSuffix := "private"
	// create new exchange
	exchange, err := NewExchange(hosts[len(hosts)-1], []peer.ID{}, protocolSuffix)
	require.NoError(t, err)
	exchange.Params.MaxHeadersPerRequest = 10
	exchange.ctx, exchange.cancel = context.WithCancel(context.Background())
	t.Cleanup(exchange.cancel)
	servers := make([]*ExchangeServer, len(hosts)-1) // amount of servers is len(hosts)-1 because one peer acts as a client
	for index := range servers {
		servers[index], err = NewExchangeServer(hosts[index], store, protocolSuffix)
		require.NoError(t, err)
		servers[index].Start(context.Background()) //nolint:errcheck
		exchange.peerTracker.connectedPeers[hosts[index].ID()] = &peerStat{peerID: hosts[index].ID()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	// request headers from 1 to totalAmount(80)
	headers, err := exchange.GetRangeByHeight(ctx, 1, uint64(totalAmount))
	require.NoError(t, err)
	require.Len(t, headers, 80)
}

// TestExchange_RequestHeadersFails tests that the Exchange instance will return
// header.ErrNotFound if it will not have requested header.
func TestExchange_RequestHeadersFails(t *testing.T) {
	hosts := createMocknet(t, 2)
	exchg, _ := createP2PExAndServer(t, hosts[0], hosts[1])
	tt := []struct {
		amount      uint64
		expectedErr *error
	}{
		{
			amount:      10,
			expectedErr: &header.ErrNotFound,
		},
		{
			amount:      600,
			expectedErr: &header.ErrHeadersLimitExceeded,
		},
	}
	for _, test := range tt {
		// perform expected request
		_, err := exchg.GetRangeByHeight(context.Background(), 1, test.amount)
		require.Error(t, err)
		require.ErrorAs(t, err, test.expectedErr)
	}
}

// TestExchange_RequestByHash tests that the Exchange instance can
// respond to an ExtendedHeaderRequest for a hash instead of a height.
func TestExchange_RequestByHash(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	net, err := mocknet.FullMeshConnected(2)
	require.NoError(t, err)
	// get host and peer
	host, peer := net.Hosts()[0], net.Hosts()[1]
	// create and start the ExchangeServer
	store := createStore(t, 5)
	serv, err := NewExchangeServer(host, store, "private")
	require.NoError(t, err)
	err = serv.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		serv.Stop(context.Background()) //nolint:errcheck
	})

	// start a new stream via Peer to see if Host can handle inbound requests
	stream, err := peer.NewStream(context.Background(), libhost.InfoFromHost(host).ID, privateProtocolID)
	require.NoError(t, err)
	// create request for a header at a random height
	reqHeight := store.headHeight - 2
	req := &p2p_pb.ExtendedHeaderRequest{
		Data:   &p2p_pb.ExtendedHeaderRequest_Hash{Hash: store.headers[reqHeight].Hash()},
		Amount: 1,
	}
	// send request
	_, err = serde.Write(stream, req)
	require.NoError(t, err)
	// read resp
	resp := new(p2p_pb.ExtendedHeaderResponse)
	_, err = serde.Read(stream, resp)
	require.NoError(t, err)
	// compare
	eh, err := header.UnmarshalExtendedHeader(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, store.headers[reqHeight].Height, eh.Height)
	assert.Equal(t, store.headers[reqHeight].Hash(), eh.Hash())
}

func Test_bestHead(t *testing.T) {
	params := DefaultClientParameters()
	gen := func() []*header.ExtendedHeader {
		suite := header.NewTestSuite(t, 3)
		res := make([]*header.ExtendedHeader, 0)
		for i := 0; i < 3; i++ {
			res = append(res, suite.GenExtendedHeader())
		}
		return res
	}
	testCases := []struct {
		precondition   func() []*header.ExtendedHeader
		expectedHeight int64
	}{
		/*
			Height -> Amount
			headerHeight[0]=1 -> 1
			headerHeight[1]=2 -> 1
			headerHeight[2]=3 -> 1
			result -> headerHeight[2]
		*/
		{
			precondition:   gen,
			expectedHeight: 3,
		},
		/*
			Height -> Amount
			headerHeight[0]=1 -> 2
			headerHeight[1]=2 -> 1
			headerHeight[2]=3 -> 1
			result -> headerHeight[0]
		*/
		{
			precondition: func() []*header.ExtendedHeader {
				res := gen()
				res = append(res, res[0])
				return res
			},
			expectedHeight: 1,
		},
		/*
			Height -> Amount
			headerHeight[0]=1 -> 3
			headerHeight[1]=2 -> 2
			headerHeight[2]=3 -> 1
			result -> headerHeight[1]
		*/
		{
			precondition: func() []*header.ExtendedHeader {
				res := gen()
				res = append(res, res[0])
				res = append(res, res[0])
				res = append(res, res[1])
				return res
			},
			expectedHeight: 2,
		},
	}
	for _, tt := range testCases {
		res := tt.precondition()
		header, err := bestHead(res, params.MinResponses)
		require.NoError(t, err)
		require.True(t, header.Height == tt.expectedHeight)
	}
}

// TestExchange_RequestByHashFails tests that the Exchange instance can
// respond with a StatusCode_NOT_FOUND if it will not have requested header.
func TestExchange_RequestByHashFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	net, err := mocknet.FullMeshConnected(2)
	require.NoError(t, err)
	// get host and peer
	host, peer := net.Hosts()[0], net.Hosts()[1]
	serv, err := NewExchangeServer(host, createStore(t, 0), "private")
	require.NoError(t, err)
	err = serv.Start(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		serv.Stop(context.Background()) //nolint:errcheck
	})

	stream, err := peer.NewStream(context.Background(), libhost.InfoFromHost(host).ID, privateProtocolID)
	require.NoError(t, err)
	req := &p2p_pb.ExtendedHeaderRequest{
		Data:   &p2p_pb.ExtendedHeaderRequest_Hash{Hash: []byte("dummy_hash")},
		Amount: 1,
	}
	// send request
	_, err = serde.Write(stream, req)
	require.NoError(t, err)
	// read resp
	resp := new(p2p_pb.ExtendedHeaderResponse)
	_, err = serde.Read(stream, resp)
	require.NoError(t, err)
	require.Equal(t, resp.StatusCode, p2p_pb.StatusCode_NOT_FOUND)
}

func createMocknet(t *testing.T, amount int) []libhost.Host {
	net, err := mocknet.FullMeshConnected(amount)
	require.NoError(t, err)
	// get host and peer
	return net.Hosts()
}

// createP2PExAndServer creates a Exchange with 5 headers already in its store.
func createP2PExAndServer(t *testing.T, host, tpeer libhost.Host) (header.Exchange, *mockStore) {
	store := createStore(t, 5)
	serverSideEx, err := NewExchangeServer(tpeer, store, "private")
	require.NoError(t, err)
	err = serverSideEx.Start(context.Background())
	require.NoError(t, err)

	ex, err := NewExchange(host, []peer.ID{tpeer.ID()}, "private")
	require.NoError(t, err)
	ex.peerTracker.connectedPeers[tpeer.ID()] = &peerStat{peerID: tpeer.ID()}
	require.NoError(t, ex.Start(context.Background()))

	t.Cleanup(func() {
		serverSideEx.Stop(context.Background()) //nolint:errcheck
		ex.Stop(context.Background())           //nolint:errcheck
	})
	return ex, store
}

type mockStore struct {
	headers    map[int64]*header.ExtendedHeader
	headHeight int64
}

// createStore creates a mock store and adds several random
// headers
func createStore(t *testing.T, numHeaders int) *mockStore {
	store := &mockStore{
		headers:    make(map[int64]*header.ExtendedHeader),
		headHeight: 0,
	}

	suite := header.NewTestSuite(t, numHeaders)

	for i := 0; i < numHeaders; i++ {
		header := suite.GenExtendedHeader()
		store.headers[header.Height] = header

		if header.Height > store.headHeight {
			store.headHeight = header.Height
		}
	}
	return store
}

func (m *mockStore) Init(context.Context, *header.ExtendedHeader) error { return nil }
func (m *mockStore) Start(context.Context) error                        { return nil }
func (m *mockStore) Stop(context.Context) error                         { return nil }

func (m *mockStore) Height() uint64 {
	return uint64(m.headHeight)
}

func (m *mockStore) Head(context.Context) (*header.ExtendedHeader, error) {
	return m.headers[m.headHeight], nil
}

func (m *mockStore) Get(ctx context.Context, hash tmbytes.HexBytes) (*header.ExtendedHeader, error) {
	for _, header := range m.headers {
		if bytes.Equal(header.Hash(), hash) {
			return header, nil
		}
	}
	return nil, header.ErrNotFound
}

func (m *mockStore) GetByHeight(ctx context.Context, height uint64) (*header.ExtendedHeader, error) {
	return m.headers[int64(height)], nil
}

func (m *mockStore) GetRangeByHeight(ctx context.Context, from, to uint64) ([]*header.ExtendedHeader, error) {
	headers := make([]*header.ExtendedHeader, to-from)
	// As the requested range is [from; to),
	// check that (to-1) height in request is less than
	// the biggest header height in store.
	if to-1 > m.Height() {
		return nil, header.ErrNotFound
	}
	for i := range headers {
		headers[i] = m.headers[int64(from)]
		from++
	}
	return headers, nil
}

func (m *mockStore) GetVerifiedRange(
	ctx context.Context,
	h *header.ExtendedHeader,
	to uint64,
) ([]*header.ExtendedHeader, error) {
	return m.GetRangeByHeight(ctx, uint64(h.Height)+1, to)
}

func (m *mockStore) Has(context.Context, tmbytes.HexBytes) (bool, error) {
	return false, nil
}

func (m *mockStore) Append(ctx context.Context, headers ...*header.ExtendedHeader) (int, error) {
	for _, header := range headers {
		m.headers[header.Height] = header
		// set head
		if header.Height > m.headHeight {
			m.headHeight = header.Height
		}
	}
	return len(headers), nil
}
