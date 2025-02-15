package lassie

import (
	"context"
	"net/http"
	"time"

	"github.com/filecoin-project/lassie/pkg/indexerlookup"
	"github.com/filecoin-project/lassie/pkg/net/client"
	"github.com/filecoin-project/lassie/pkg/net/host"
	"github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/filecoin-project/lassie/pkg/session"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multicodec"
)

var _ types.Fetcher = &Lassie{}

const DefaultProviderTimeout = 20 * time.Second
const DefaultBitswapConcurrency = 32
const DefaultBitswapConcurrencyPerRetrieval = 12

// Lassie represents a reusable retrieval client.
type Lassie struct {
	cfg       *LassieConfig
	retriever *retriever.Retriever
}

// LassieConfig customizes the behavior of a Lassie instance.
type LassieConfig struct {
	Finder                         retriever.CandidateFinder
	Host                           host.Host
	ProviderTimeout                time.Duration
	ConcurrentSPRetrievals         uint
	GlobalTimeout                  time.Duration
	Libp2pOptions                  []libp2p.Option
	Protocols                      []multicodec.Code
	ProviderBlockList              map[peer.ID]bool
	ProviderAllowList              map[peer.ID]bool
	BitswapConcurrency             int
	BitswapConcurrencyPerRetrieval int
}

type LassieOption func(cfg *LassieConfig)

// NewLassie creates a new Lassie instance.
func NewLassie(ctx context.Context, opts ...LassieOption) (*Lassie, error) {
	cfg := NewLassieConfig(opts...)
	return NewLassieWithConfig(ctx, cfg)
}

// NewLassieConfig creates a new LassieConfig instance with the given LassieOptions.
func NewLassieConfig(opts ...LassieOption) *LassieConfig {
	cfg := &LassieConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

// NewLassieWithConfig creates a new Lassie instance with a custom
// configuration.
func NewLassieWithConfig(ctx context.Context, cfg *LassieConfig) (*Lassie, error) {
	if cfg.Finder == nil {
		var err error
		cfg.Finder, err = indexerlookup.NewCandidateFinder(indexerlookup.WithHttpClient(&http.Client{}))
		if err != nil {
			return nil, err
		}
	}

	if cfg.ProviderTimeout == 0 {
		cfg.ProviderTimeout = DefaultProviderTimeout
	}
	if cfg.BitswapConcurrency == 0 {
		cfg.BitswapConcurrency = DefaultBitswapConcurrency
	}
	if cfg.BitswapConcurrencyPerRetrieval == 0 {
		cfg.BitswapConcurrencyPerRetrieval = DefaultBitswapConcurrencyPerRetrieval
	}

	datastore := sync.MutexWrap(datastore.NewMapDatastore())

	if cfg.Host == nil {
		var err error
		cfg.Host, err = host.InitHost(ctx, cfg.Libp2pOptions)
		if err != nil {
			return nil, err
		}
	}

	sessionConfig := session.DefaultConfig().
		WithProviderBlockList(cfg.ProviderBlockList).
		WithProviderAllowList(cfg.ProviderAllowList).
		WithDefaultProviderConfig(session.ProviderConfig{
			RetrievalTimeout:        cfg.ProviderTimeout,
			MaxConcurrentRetrievals: cfg.ConcurrentSPRetrievals,
		})
	session := session.NewSession(sessionConfig, true)

	if len(cfg.Protocols) == 0 {
		cfg.Protocols = []multicodec.Code{multicodec.TransportBitswap, multicodec.TransportGraphsyncFilecoinv1, multicodec.TransportIpfsGatewayHttp}
	}

	protocolRetrievers := make(map[multicodec.Code]types.CandidateRetriever)
	for _, protocol := range cfg.Protocols {
		switch protocol {
		case multicodec.TransportGraphsyncFilecoinv1:
			retrievalClient, err := client.NewClient(ctx, datastore, cfg.Host)
			if err != nil {
				return nil, err
			}

			if err := retrievalClient.AwaitReady(); err != nil { // wait for dt setup
				return nil, err
			}
			protocolRetrievers[protocol] = retriever.NewGraphsyncRetriever(session, retrievalClient)
		case multicodec.TransportBitswap:
			protocolRetrievers[protocol] = retriever.NewBitswapRetrieverFromHost(ctx, cfg.Host, retriever.BitswapConfig{
				BlockTimeout:            cfg.ProviderTimeout,
				Concurrency:             cfg.BitswapConcurrency,
				ConcurrencyPerRetrieval: cfg.BitswapConcurrencyPerRetrieval,
			})
		case multicodec.TransportIpfsGatewayHttp:
			protocolRetrievers[protocol] = retriever.NewHttpRetriever(session, http.DefaultClient)
		}
	}

	retriever, err := retriever.NewRetriever(ctx, session, cfg.Finder, protocolRetrievers)
	if err != nil {
		return nil, err
	}
	retriever.Start()

	lassie := &Lassie{
		cfg:       cfg,
		retriever: retriever,
	}

	return lassie, nil
}

// WithFinder allows you to specify a custom candidate finder.
func WithFinder(finder retriever.CandidateFinder) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.Finder = finder
	}
}

// WithProviderTimeout allows you to specify a custom timeout for retrieving
// data from a provider. Beyond this limit, when no data has been received,
// the retrieval will fail.
func WithProviderTimeout(timeout time.Duration) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.ProviderTimeout = timeout
	}
}

// WithGlobalTimeout allows you to specify a custom timeout for the entire
// retrieval process.
func WithGlobalTimeout(timeout time.Duration) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.GlobalTimeout = timeout
	}
}

// WithHost allows you to specify a custom libp2p host.
func WithHost(host host.Host) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.Host = host
	}
}

// WithLibp2pOpts allows you to specify custom libp2p options.
func WithLibp2pOpts(libp2pOptions ...libp2p.Option) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.Libp2pOptions = libp2pOptions
	}
}

// WithConcurrentSPRetrievals allows you to specify a custom number of
// concurrent retrievals from a single storage provider.
func WithConcurrentSPRetrievals(maxConcurrentSPRtreievals uint) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.ConcurrentSPRetrievals = maxConcurrentSPRtreievals
	}
}

// WithProtocols allows you to specify a custom set of protocols to use for
// retrieval.
func WithProtocols(protocols []multicodec.Code) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.Protocols = protocols
	}
}

// WithProviderBlockList allows you to specify a custom provider block list.
func WithProviderBlockList(providerBlockList map[peer.ID]bool) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.ProviderBlockList = providerBlockList
	}
}

// WithProviderAllowList allows you to specify a custom set of providers to
// allow fetching from. If this is not set, all providers will be allowed unless
// they are in the block list.
func WithProviderAllowList(providerAllowList map[peer.ID]bool) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.ProviderAllowList = providerAllowList
	}
}

// WithBitswapConcurrency allows you to specify a custom concurrency for bitswap
// retrievals across all parallel retrievals in the same Lassie instance. This
// is applied using a preloader during traversals. The default is 32.
func WithBitswapConcurrency(concurrency int) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.BitswapConcurrency = concurrency
	}
}

// WithBitswapConcurrencyPerRetrieval allows you to specify a custom concurrency
// for bitswap retrievals for each individual parallel retrieval. This is
// applied using a preloader during traversals. The default is 8.
func WithBitswapConcurrencyPerRetrieval(concurrency int) LassieOption {
	return func(cfg *LassieConfig) {
		cfg.BitswapConcurrencyPerRetrieval = concurrency
	}
}

// Fetch initiates a retrieval request and returns either some details about
// the retrieval or an error. The request should contain all of the parameters
// of the requested retrieval, including the LinkSystem where the blocks are
// intended to be stored.
func (l *Lassie) Fetch(ctx context.Context, request types.RetrievalRequest, opts ...types.FetchOption) (*types.RetrievalStats, error) {
	var cancel context.CancelFunc
	if l.cfg.GlobalTimeout != time.Duration(0) {
		ctx, cancel = context.WithTimeout(ctx, l.cfg.GlobalTimeout)
		defer cancel()
	}
	return l.retriever.Retrieve(ctx, request, types.NewFetchConfig(opts...).EventsCallback)
}

// RegisterSubscriber registers a subscriber to receive retrieval events.
// The returned function can be called to unregister the subscriber.
func (l *Lassie) RegisterSubscriber(subscriber types.RetrievalEventSubscriber) func() {
	return l.retriever.RegisterSubscriber(subscriber)
}
