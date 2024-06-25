// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	avalanchegoConstants "github.com/ava-labs/avalanchego/utils/constants"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ava-labs/coreth/consensus/dummy"
	"github.com/ava-labs/coreth/constants"
	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/rawdb"
	"github.com/ava-labs/coreth/core/state"
	"github.com/ava-labs/coreth/core/txpool"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/eth"
	"github.com/ava-labs/coreth/eth/ethconfig"
	"github.com/ava-labs/coreth/metrics"
	corethPrometheus "github.com/ava-labs/coreth/metrics/prometheus"
	"github.com/ava-labs/coreth/miner"
	"github.com/ava-labs/coreth/node"
	"github.com/ava-labs/coreth/params"
	"github.com/ava-labs/coreth/peer"
	"github.com/ava-labs/coreth/plugin/atx"
	"github.com/ava-labs/coreth/plugin/evm/message"
	"github.com/ava-labs/coreth/trie/triedb/hashdb"

	warpPrecompile "github.com/ava-labs/coreth/precompile/contracts/warp"
	"github.com/ava-labs/coreth/rpc"
	statesyncclient "github.com/ava-labs/coreth/sync/client"
	"github.com/ava-labs/coreth/sync/client/stats"
	"github.com/ava-labs/coreth/trie"
	"github.com/ava-labs/coreth/utils"
	"github.com/ava-labs/coreth/warp"
	warpValidators "github.com/ava-labs/coreth/warp/validators"

	// Force-load tracer engine to trigger registration
	//
	// We must import this package (not referenced elsewhere) so that the native "callTracer"
	// is added to a map of client-accessible tracers. In geth, this is done
	// inside of cmd/geth.
	_ "github.com/ava-labs/coreth/eth/tracers/js"
	_ "github.com/ava-labs/coreth/eth/tracers/native"

	"github.com/ava-labs/coreth/precompile/precompileconfig"
	// Force-load precompiles to trigger registration
	_ "github.com/ava-labs/coreth/precompile/registry"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"

	avalancheRPC "github.com/gorilla/rpc/v2"

	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/formatting/address"
	"github.com/ava-labs/avalanchego/utils/perms"
	"github.com/ava-labs/avalanchego/utils/profiler"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/components/chain"
	pluginDb "github.com/ava-labs/coreth/plugin/db"

	commonEng "github.com/ava-labs/avalanchego/snow/engine/common"

	avalancheUtils "github.com/ava-labs/avalanchego/utils"
	avalancheJSON "github.com/ava-labs/avalanchego/utils/json"
)

var (
	_ block.ChainVM                      = &VM{}
	_ block.BuildBlockWithContextChainVM = &VM{}
	_ block.StateSyncableVM              = &VM{}
	_ statesyncclient.EthBlockParser     = &VM{}
)

const (
	x2cRateInt64       int64 = 1_000_000_000
	x2cRateMinus1Int64 int64 = x2cRateInt64 - 1
)

var (
	// x2cRate is the conversion rate between the smallest denomination on the X-Chain
	// 1 nAVAX and the smallest denomination on the C-Chain 1 wei. Where 1 nAVAX = 1 gWei.
	// This is only required for AVAX because the denomination of 1 AVAX is 9 decimal
	// places on the X and P chains, but is 18 decimal places within the EVM.
	x2cRate       = big.NewInt(x2cRateInt64)
	x2cRateMinus1 = big.NewInt(x2cRateMinus1Int64)
)

const (
	// Max time from current time allowed for blocks, before they're considered future blocks
	// and fail verification
	maxFutureBlockTime = 10 * time.Second
	defaultMempoolSize = 4096
	codecVersion       = uint16(0)

	decidedCacheSize       = 10 * units.MiB
	missingCacheSize       = 50
	unverifiedCacheSize    = 5 * units.MiB
	bytesToIDCacheSize     = 5 * units.MiB
	warpSignatureCacheSize = 500

	// Prefixes for metrics gatherers
	ethMetricsPrefix        = "eth"
	sdkMetricsPrefix        = "sdk"
	chainStateMetricsPrefix = "chain_state"

	targetAtomicTxsSize = 40 * units.KiB

	// p2p app protocols
	ethTxGossipProtocol    = 0x0
	atomicTxGossipProtocol = 0x1

	// gossip constants
	pushGossipDiscardedElements          = 16_384
	txGossipBloomMinTargetElements       = 8 * 1024
	txGossipBloomTargetFalsePositiveRate = 0.01
	txGossipBloomResetFalsePositiveRate  = 0.05
	txGossipBloomChurnMultiplier         = 3
	txGossipTargetMessageSize            = 20 * units.KiB
	maxValidatorSetStaleness             = time.Minute
	txGossipThrottlingPeriod             = 10 * time.Second
	txGossipThrottlingLimit              = 2
	txGossipPollSize                     = 1
)

// Define the API endpoints for the VM
const (
	avaxEndpoint            = "/avax"
	adminEndpoint           = "/admin"
	ethRPCEndpoint          = "/rpc"
	ethWSEndpoint           = "/ws"
	ethTxGossipNamespace    = "eth_tx_gossip"
	atomicTxGossipNamespace = "atomic_tx_gossip"
)

var (
	// Set last accepted key to be longer than the keys used to store accepted block IDs.
	lastAcceptedKey = []byte("last_accepted_key")
	acceptedPrefix  = []byte("snowman_accepted")
	metadataPrefix  = []byte("metadata")
	warpPrefix      = []byte("warp")
	ethDBPrefix     = []byte("ethdb")
)

var (
	errEmptyBlock                     = errors.New("empty block")
	errUnsupportedFXs                 = errors.New("unsupported feature extensions")
	errInvalidBlock                   = errors.New("invalid block")
	errInvalidAddr                    = errors.New("invalid hex address")
	errInsufficientAtomicTxFee        = atx.ErrInsufficientAtomicTxFee
	errAssetIDMismatch                = errors.New("asset IDs in the input don't match the utxo")
	errNoImportInputs                 = errors.New("tx has no imported inputs")
	errInputsNotSortedUnique          = errors.New("inputs not sorted and unique")
	errPublicKeySignatureMismatch     = errors.New("signature doesn't match public key")
	errWrongChainID                   = errors.New("tx has wrong chain ID")
	errInsufficientFunds              = errors.New("insufficient funds")
	errNoExportOutputs                = errors.New("tx has no export outputs")
	errOutputsNotSorted               = errors.New("tx outputs not sorted")
	errOutputsNotSortedUnique         = errors.New("outputs not sorted and unique")
	errOverflowExport                 = errors.New("overflow when computing export amount + txFee")
	errInvalidNonce                   = errors.New("invalid nonce")
	errConflictingAtomicInputs        = atx.ErrConflictingAtomicInputs
	errUnclesUnsupported              = errors.New("uncles unsupported")
	errRejectedParent                 = errors.New("rejected parent")
	errInsufficientFundsForFee        = errors.New("insufficient AVAX funds to pay transaction fee")
	errNoEVMOutputs                   = errors.New("tx has no EVM outputs")
	errNilBaseFeeApricotPhase3        = errors.New("nil base fee is invalid after apricotPhase3")
	errNilExtDataGasUsedApricotPhase4 = errors.New("nil extDataGasUsed is invalid after apricotPhase4")
	errNilBlockGasCostApricotPhase4   = errors.New("nil blockGasCost is invalid after apricotPhase4")
	errConflictingAtomicTx            = atx.ErrConflictingAtomicTx
	errTooManyAtomicTx                = atx.ErrTooManyAtomicTx
	errMissingAtomicTxs               = errors.New("cannot build a block with non-empty extra data and zero atomic transactions")
	errInvalidHeaderPredicateResults  = errors.New("invalid header predicate results")
)

var originalStderr *os.File

// legacyApiNames maps pre geth v1.10.20 api names to their updated counterparts.
// used in attachEthService for backward configuration compatibility.
var legacyApiNames = map[string]string{
	"internal-public-eth":              "internal-eth",
	"internal-public-blockchain":       "internal-blockchain",
	"internal-public-transaction-pool": "internal-transaction",
	"internal-public-tx-pool":          "internal-tx-pool",
	"internal-public-debug":            "internal-debug",
	"internal-private-debug":           "internal-debug",
	"internal-public-account":          "internal-account",
	"internal-private-personal":        "internal-personal",

	"public-eth":        "eth",
	"public-eth-filter": "eth-filter",
	"private-admin":     "admin",
	"public-debug":      "debug",
	"private-debug":     "debug",
}

func init() {
	// Preserve [os.Stderr] prior to the call in plugin/main.go to plugin.Serve(...).
	// Preserving the log level allows us to update the root handler while writing to the original
	// [os.Stderr] that is being piped through to the logger via the rpcchainvm.
	originalStderr = os.Stderr
}

// VM implements the snowman.ChainVM interface
type VM struct {
	ctx *snow.Context
	// [cancel] may be nil until [snow.NormalOp] starts
	cancel context.CancelFunc
	// *chain.State helps to implement the VM interface by wrapping blocks
	// with an efficient caching layer.
	*chain.State

	config Config

	chainID     *big.Int
	networkID   uint64
	genesisHash common.Hash
	chainConfig *params.ChainConfig
	ethConfig   ethconfig.Config

	// pointers to eth constructs
	eth        *eth.Ethereum
	txPool     *txpool.TxPool
	blockChain *core.BlockChain
	miner      *miner.Miner

	// [db] is the VM's current database managed by ChainState
	db *versiondb.Database

	// metadataDB is used to store one off keys.
	metadataDB database.Database

	// [chaindb] is the database supplied to the Ethereum backend
	chaindb ethdb.Database

	// [acceptedBlockDB] is the database to store the last accepted
	// block.
	acceptedBlockDB database.Database

	// [warpDB] is used to store warp message signatures
	// set to a prefixDB with the prefix [warpPrefix]
	warpDB database.Database

	toEngine chan<- commonEng.Message

	syntacticBlockValidator BlockValidator

	atomicBackend atx.AtomicBackend
	// [atomicTrie] maintains a merkle forest of [height]=>[atomic txs].
	atomicTrie atx.AtomicTrie

	builder *blockBuilder

	codec codec.Manager
	clock mockable.Clock

	shutdownChan chan struct{}
	shutdownWg   sync.WaitGroup

	// Continuous Profiler
	profiler profiler.ContinuousProfiler

	peer.Network
	client       peer.NetworkClient
	networkCodec codec.Manager

	validators *p2p.Validators

	// Metrics
	sdkMetrics *prometheus.Registry

	bootstrapped bool
	IsPlugin     bool

	logger CorethLogger
	// State sync server and client
	StateSyncServer
	StateSyncClient

	// Avalanche Warp Messaging backend
	// Used to serve BLS signatures of warp messages over RPC
	warpBackend warp.Backend

	// Initialize only sets these if nil so they can be overridden in tests
	p2pSender             commonEng.AppSender
	ethTxGossipHandler    p2p.Handler
	ethTxPushGossiper     avalancheUtils.Atomic[*gossip.PushGossiper[*GossipEthTx]]
	ethTxPullGossiper     gossip.Gossiper
	atomicTxGossipHandler p2p.Handler
	atomicTxPushGossiper  *gossip.PushGossiper[*atx.GossipAtomicTx]
	atomicTxPullGossiper  gossip.Gossiper

	*atx.VM
}

type (
	Tx               = atx.Tx
	EVMInput         = atx.EVMInput
	EVMOutput        = atx.EVMOutput
	UnsignedImportTx = atx.UnsignedImportTx
	UnsignedAtomicTx = atx.UnsignedAtomicTx
	UnsignedExportTx = atx.UnsignedExportTx
)

var (
	EVMOutputGas = atx.EVMOutputGas
	EVMInputGas  = atx.EVMInputGas
)

var (
	CalculateDynamicFee = atx.CalculateDynamicFee
)

/*
 ******************************************************************************
 ********************************* Snowman API ********************************
 ******************************************************************************
 */

// implements SnowmanPlusPlusVM interface
func (vm *VM) GetActivationTime() time.Time {
	return utils.Uint64ToTime(vm.chainConfig.ApricotPhase4BlockTimestamp)
}

// Initialize implements the snowman.ChainVM interface
func (vm *VM) Initialize(
	_ context.Context,
	chainCtx *snow.Context,
	db database.Database,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- commonEng.Message,
	fxs []*commonEng.Fx,
	appSender commonEng.AppSender,
) error {
	vm.config.SetDefaults()
	if len(configBytes) > 0 {
		if err := json.Unmarshal(configBytes, &vm.config); err != nil {
			return fmt.Errorf("failed to unmarshal config %s: %w", string(configBytes), err)
		}
	}
	if err := vm.config.Validate(); err != nil {
		return err
	}
	// We should deprecate config flags as the first thing, before we do anything else
	// because this can set old flags to new flags. log the message after we have
	// initialized the logger.
	deprecateMsg := vm.config.Deprecate()

	vm.ctx = chainCtx

	// Create logger
	alias, err := vm.ctx.BCLookup.PrimaryAlias(vm.ctx.ChainID)
	if err != nil {
		// fallback to ChainID string instead of erroring
		alias = vm.ctx.ChainID.String()
	}

	var writer io.Writer = vm.ctx.Log
	if vm.IsPlugin {
		writer = originalStderr
	}

	corethLogger, err := InitLogger(alias, vm.config.LogLevel, vm.config.LogJSONFormat, writer)
	if err != nil {
		return fmt.Errorf("failed to initialize logger due to: %w ", err)
	}
	vm.logger = corethLogger

	log.Info("Initializing Coreth VM", "Version", Version, "Config", vm.config)

	if deprecateMsg != "" {
		log.Warn("Deprecation Warning", "msg", deprecateMsg)
	}

	if len(fxs) > 0 {
		return errUnsupportedFXs
	}

	// Enable debug-level metrics that might impact runtime performance
	metrics.EnabledExpensive = vm.config.MetricsExpensiveEnabled

	vm.toEngine = toEngine
	vm.shutdownChan = make(chan struct{}, 1)
	// Use NewNested rather than New so that the structure of the database
	// remains the same regardless of the provided baseDB type.
	vm.chaindb = rawdb.NewDatabase(pluginDb.Database{Database: prefixdb.NewNested(ethDBPrefix, db)})
	vm.db = versiondb.New(db)
	vm.acceptedBlockDB = prefixdb.New(acceptedPrefix, vm.db)
	vm.metadataDB = prefixdb.New(metadataPrefix, vm.db)
	// Note warpDB is not part of versiondb because it is not necessary
	// that warp signatures are committed to the database atomically with
	// the last accepted block.
	vm.warpDB = prefixdb.New(warpPrefix, db)

	if vm.config.InspectDatabase {
		start := time.Now()
		log.Info("Starting database inspection")
		if err := rawdb.InspectDatabase(vm.chaindb, nil, nil); err != nil {
			return err
		}
		log.Info("Completed database inspection", "elapsed", time.Since(start))
	}

	g := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, g); err != nil {
		return err
	}

	var extDataHashes map[common.Hash]common.Hash
	// Set the chain config for mainnet/fuji chain IDs
	switch {
	case g.Config.ChainID.Cmp(params.AvalancheMainnetChainID) == 0:
		config := *params.AvalancheMainnetChainConfig
		g.Config = &config
		extDataHashes = mainnetExtDataHashes
	case g.Config.ChainID.Cmp(params.AvalancheFujiChainID) == 0:
		config := *params.AvalancheFujiChainConfig
		g.Config = &config
		extDataHashes = fujiExtDataHashes
	case g.Config.ChainID.Cmp(params.AvalancheLocalChainID) == 0:
		config := *params.AvalancheLocalChainConfig
		g.Config = &config
	}
	// If the Durango is activated, activate the Warp Precompile at the same time
	if g.Config.DurangoBlockTimestamp != nil {
		g.Config.PrecompileUpgrades = append(g.Config.PrecompileUpgrades, params.PrecompileUpgrade{
			Config: warpPrecompile.NewDefaultConfig(g.Config.DurangoBlockTimestamp),
		})
	}
	// Set the Avalanche Context on the ChainConfig
	g.Config.AvalancheContext = params.AvalancheContext{
		SnowCtx: chainCtx,
	}
	vm.syntacticBlockValidator = NewBlockValidator(extDataHashes)

	// Ensure that non-standard commit interval is only allowed for the local network
	if g.Config.ChainID.Cmp(params.AvalancheLocalChainID) != 0 {
		if vm.config.CommitInterval != defaultCommitInterval {
			return fmt.Errorf("cannot start non-local network with commit interval %d", vm.config.CommitInterval)
		}
		if vm.config.StateSyncCommitInterval != defaultSyncableCommitInterval {
			return fmt.Errorf("cannot start non-local network with syncable interval %d", vm.config.StateSyncCommitInterval)
		}
	}

	// Free the memory of the extDataHash map that is not used (i.e. if mainnet
	// config, free fuji)
	fujiExtDataHashes = nil
	mainnetExtDataHashes = nil

	vm.chainID = g.Config.ChainID

	vm.ethConfig = ethconfig.NewDefaultConfig()
	vm.ethConfig.Genesis = g
	vm.ethConfig.NetworkId = vm.chainID.Uint64()
	vm.genesisHash = vm.ethConfig.Genesis.ToBlock().Hash() // must create genesis hash before [vm.readLastAccepted]
	lastAcceptedHash, lastAcceptedHeight, err := vm.readLastAccepted()
	if err != nil {
		return err
	}
	log.Info(fmt.Sprintf("lastAccepted = %s", lastAcceptedHash))

	// Set minimum price for mining and default gas price oracle value to the min
	// gas price to prevent so transactions and blocks all use the correct fees
	vm.ethConfig.RPCGasCap = vm.config.RPCGasCap
	vm.ethConfig.RPCEVMTimeout = vm.config.APIMaxDuration.Duration
	vm.ethConfig.RPCTxFeeCap = vm.config.RPCTxFeeCap

	vm.ethConfig.TxPool.NoLocals = !vm.config.LocalTxsEnabled
	vm.ethConfig.TxPool.PriceLimit = vm.config.TxPoolPriceLimit
	vm.ethConfig.TxPool.PriceBump = vm.config.TxPoolPriceBump
	vm.ethConfig.TxPool.AccountSlots = vm.config.TxPoolAccountSlots
	vm.ethConfig.TxPool.GlobalSlots = vm.config.TxPoolGlobalSlots
	vm.ethConfig.TxPool.AccountQueue = vm.config.TxPoolAccountQueue
	vm.ethConfig.TxPool.GlobalQueue = vm.config.TxPoolGlobalQueue
	vm.ethConfig.TxPool.Lifetime = vm.config.TxPoolLifetime.Duration

	vm.ethConfig.AllowUnfinalizedQueries = vm.config.AllowUnfinalizedQueries
	vm.ethConfig.AllowUnprotectedTxs = vm.config.AllowUnprotectedTxs
	vm.ethConfig.AllowUnprotectedTxHashes = vm.config.AllowUnprotectedTxHashes
	vm.ethConfig.Preimages = vm.config.Preimages
	vm.ethConfig.Pruning = vm.config.Pruning
	vm.ethConfig.TrieCleanCache = vm.config.TrieCleanCache
	vm.ethConfig.TrieDirtyCache = vm.config.TrieDirtyCache
	vm.ethConfig.TrieDirtyCommitTarget = vm.config.TrieDirtyCommitTarget
	vm.ethConfig.TriePrefetcherParallelism = vm.config.TriePrefetcherParallelism
	vm.ethConfig.SnapshotCache = vm.config.SnapshotCache
	vm.ethConfig.AcceptorQueueLimit = vm.config.AcceptorQueueLimit
	vm.ethConfig.PopulateMissingTries = vm.config.PopulateMissingTries
	vm.ethConfig.PopulateMissingTriesParallelism = vm.config.PopulateMissingTriesParallelism
	vm.ethConfig.AllowMissingTries = vm.config.AllowMissingTries
	vm.ethConfig.SnapshotDelayInit = vm.stateSyncEnabled(lastAcceptedHeight)
	vm.ethConfig.SnapshotWait = vm.config.SnapshotWait
	vm.ethConfig.SnapshotVerify = vm.config.SnapshotVerify
	vm.ethConfig.OfflinePruning = vm.config.OfflinePruning
	vm.ethConfig.OfflinePruningBloomFilterSize = vm.config.OfflinePruningBloomFilterSize
	vm.ethConfig.OfflinePruningDataDirectory = vm.config.OfflinePruningDataDirectory
	vm.ethConfig.CommitInterval = vm.config.CommitInterval
	vm.ethConfig.SkipUpgradeCheck = vm.config.SkipUpgradeCheck
	vm.ethConfig.AcceptedCacheSize = vm.config.AcceptedCacheSize
	vm.ethConfig.TxLookupLimit = vm.config.TxLookupLimit
	vm.ethConfig.SkipTxIndexing = vm.config.SkipTxIndexing

	// Create directory for offline pruning
	if len(vm.ethConfig.OfflinePruningDataDirectory) != 0 {
		if err := os.MkdirAll(vm.ethConfig.OfflinePruningDataDirectory, perms.ReadWriteExecute); err != nil {
			log.Error("failed to create offline pruning data directory", "error", err)
			return err
		}
	}

	vm.chainConfig = g.Config
	vm.networkID = vm.ethConfig.NetworkId

	if err := vm.chainConfig.Verify(); err != nil {
		return fmt.Errorf("failed to verify chain config: %w", err)
	}

	vm.codec = Codec

	if err := vm.initializeMetrics(); err != nil {
		return err
	}

	// initialize peer network
	if vm.p2pSender == nil {
		vm.p2pSender = appSender
	}

	p2pNetwork, err := p2p.NewNetwork(vm.ctx.Log, vm.p2pSender, vm.sdkMetrics, "p2p")
	if err != nil {
		return fmt.Errorf("failed to initialize p2p network: %w", err)
	}
	vm.validators = p2p.NewValidators(p2pNetwork.Peers, vm.ctx.Log, vm.ctx.SubnetID, vm.ctx.ValidatorState, maxValidatorSetStaleness)
	vm.networkCodec = message.Codec
	vm.Network = peer.NewNetwork(p2pNetwork, appSender, vm.networkCodec, message.CrossChainCodec, chainCtx.NodeID, vm.config.MaxOutboundActiveRequests, vm.config.MaxOutboundActiveCrossChainRequests)
	vm.client = peer.NewNetworkClient(vm.Network)

	// Initialize warp backend
	offchainWarpMessages := make([][]byte, len(vm.config.WarpOffChainMessages))
	for i, hexMsg := range vm.config.WarpOffChainMessages {
		offchainWarpMessages[i] = []byte(hexMsg)
	}
	vm.warpBackend, err = warp.NewBackend(vm.ctx.NetworkID, vm.ctx.ChainID, vm.ctx.WarpSigner, vm, vm.warpDB, warpSignatureCacheSize, offchainWarpMessages)
	if err != nil {
		return err
	}

	// clear warpdb on initialization if config enabled
	if vm.config.PruneWarpDB {
		if err := vm.warpBackend.Clear(); err != nil {
			return fmt.Errorf("failed to prune warpDB: %w", err)
		}
	}

	vm.VM, err = atx.NewVM(vm.ctx, vm, vm.codec, &vm.clock, vm.chainConfig, vm.sdkMetrics, defaultMempoolSize, vm.verifyTxAtTip)
	if err != nil {
		return err
	}

	if err := vm.initializeChain(lastAcceptedHash); err != nil {
		return err
	}
	go vm.ctx.Log.RecoverAndPanic(vm.startContinuousProfiler)

	// initialize bonus blocks on mainnet
	var (
		bonusBlockHeights map[uint64]ids.ID
	)
	if vm.chainID.Cmp(params.AvalancheMainnetChainID) == 0 {
		bonusBlockHeights, err = readMainnetBonusBlocks()
		if err != nil {
			return fmt.Errorf("failed to read mainnet bonus blocks: %w", err)
		}
	}
	if err := vm.VM.Initialize(
		vm.db, &atxChain{vm.blockChain}, lastAcceptedHeight, lastAcceptedHash,
		bonusBlockHeights, vm.config.CommitInterval,
	); err != nil {
		return err
	}
	vm.atomicBackend = vm.VM.AtomicBackend()
	vm.atomicTrie = vm.atomicBackend.AtomicTrie()

	vm.initializeStateSyncServer()
	return vm.initializeStateSyncClient(lastAcceptedHeight)
}

func (vm *VM) initializeMetrics() error {
	vm.sdkMetrics = prometheus.NewRegistry()
	// If metrics are enabled, register the default metrics registry
	if !metrics.Enabled {
		return nil
	}

	gatherer := corethPrometheus.Gatherer(metrics.DefaultRegistry)
	if err := vm.ctx.Metrics.Register(ethMetricsPrefix, gatherer); err != nil {
		return err
	}
	return vm.ctx.Metrics.Register(sdkMetricsPrefix, vm.sdkMetrics)
}

func (vm *VM) initializeChain(lastAcceptedHash common.Hash) error {
	nodecfg := &node.Config{
		CorethVersion:         Version,
		KeyStoreDir:           vm.config.KeystoreDirectory,
		ExternalSigner:        vm.config.KeystoreExternalSigner,
		InsecureUnlockAllowed: vm.config.KeystoreInsecureUnlockAllowed,
	}
	node, err := node.New(nodecfg)
	if err != nil {
		return err
	}
	vm.eth, err = eth.New(
		node,
		&vm.ethConfig,
		vm.createConsensusCallbacks(),
		&EthPushGossiper{vm: vm},
		vm.chaindb,
		vm.config.EthBackendSettings(),
		lastAcceptedHash,
		&vm.clock,
	)
	if err != nil {
		return err
	}
	vm.eth.SetEtherbase(constants.BlackholeAddr)
	vm.txPool = vm.eth.TxPool()
	vm.blockChain = vm.eth.BlockChain()
	vm.miner = vm.eth.Miner()

	// Set the gas parameters for the tx pool to the minimum gas price for the
	// latest upgrade.
	vm.txPool.SetGasTip(big.NewInt(0))
	vm.txPool.SetMinFee(big.NewInt(params.ApricotPhase4MinBaseFee))

	vm.eth.Start()
	return vm.initChainState(vm.blockChain.LastAcceptedBlock())
}

// initializeStateSyncClient initializes the client for performing state sync.
// If state sync is disabled, this function will wipe any ongoing summary from
// disk to ensure that we do not continue syncing from an invalid snapshot.
func (vm *VM) initializeStateSyncClient(lastAcceptedHeight uint64) error {
	stateSyncEnabled := vm.stateSyncEnabled(lastAcceptedHeight)
	// parse nodeIDs from state sync IDs in vm config
	var stateSyncIDs []ids.NodeID
	if stateSyncEnabled && len(vm.config.StateSyncIDs) > 0 {
		nodeIDs := strings.Split(vm.config.StateSyncIDs, ",")
		stateSyncIDs = make([]ids.NodeID, len(nodeIDs))
		for i, nodeIDString := range nodeIDs {
			nodeID, err := ids.NodeIDFromString(nodeIDString)
			if err != nil {
				return fmt.Errorf("failed to parse %s as NodeID: %w", nodeIDString, err)
			}
			stateSyncIDs[i] = nodeID
		}
	}

	vm.StateSyncClient = NewStateSyncClient(&stateSyncClientConfig{
		chain: vm.eth,
		state: vm.State,
		client: statesyncclient.NewClient(
			&statesyncclient.ClientConfig{
				NetworkClient:    vm.client,
				Codec:            vm.networkCodec,
				Stats:            stats.NewClientSyncerStats(),
				StateSyncNodeIDs: stateSyncIDs,
				BlockParser:      vm,
			},
		),
		enabled:              stateSyncEnabled,
		skipResume:           vm.config.StateSyncSkipResume,
		stateSyncMinBlocks:   vm.config.StateSyncMinBlocks,
		stateSyncRequestSize: vm.config.StateSyncRequestSize,
		lastAcceptedHeight:   lastAcceptedHeight, // TODO clean up how this is passed around
		chaindb:              vm.chaindb,
		metadataDB:           vm.metadataDB,
		acceptedBlockDB:      vm.acceptedBlockDB,
		db:                   vm.db,
		atomicBackend:        vm.atomicBackend,
		toEngine:             vm.toEngine,
	})

	// If StateSync is disabled, clear any ongoing summary so that we will not attempt to resume
	// sync using a snapshot that has been modified by the node running normal operations.
	if !stateSyncEnabled {
		return vm.StateSyncClient.ClearOngoingSummary()
	}

	return nil
}

// initializeStateSyncServer should be called after [vm.chain] is initialized.
func (vm *VM) initializeStateSyncServer() {
	vm.StateSyncServer = NewStateSyncServer(&stateSyncServerConfig{
		Chain:            vm.blockChain,
		AtomicTrie:       vm.atomicTrie,
		SyncableInterval: vm.config.StateSyncCommitInterval,
	})

	vm.setAppRequestHandlers()
	vm.setCrossChainAppRequestHandler()
}

func (vm *VM) initChainState(lastAcceptedBlock *types.Block) error {
	block, err := vm.newBlock(lastAcceptedBlock)
	if err != nil {
		return fmt.Errorf("failed to create block wrapper for the last accepted block: %w", err)
	}
	block.status = choices.Accepted

	config := &chain.Config{
		DecidedCacheSize:      decidedCacheSize,
		MissingCacheSize:      missingCacheSize,
		UnverifiedCacheSize:   unverifiedCacheSize,
		BytesToIDCacheSize:    bytesToIDCacheSize,
		GetBlockIDAtHeight:    vm.GetBlockIDAtHeight,
		GetBlock:              vm.getBlock,
		UnmarshalBlock:        vm.parseBlock,
		BuildBlock:            vm.buildBlock,
		BuildBlockWithContext: vm.buildBlockWithContext,
		LastAcceptedBlock:     block,
	}

	// Register chain state metrics
	chainStateRegisterer := prometheus.NewRegistry()
	state, err := chain.NewMeteredState(chainStateRegisterer, config)
	if err != nil {
		return fmt.Errorf("could not create metered state: %w", err)
	}
	vm.State = state

	if !metrics.Enabled {
		return nil
	}

	return vm.ctx.Metrics.Register(chainStateMetricsPrefix, chainStateRegisterer)
}

func (vm *VM) createConsensusCallbacks() dummy.ConsensusCallbacks {
	return dummy.ConsensusCallbacks{
		OnFinalizeAndAssemble: vm.onFinalizeAndAssemble,
		OnExtraStateChange:    vm.onExtraStateChange,
	}
}

func (vm *VM) onFinalizeAndAssemble(header *types.Header, state *state.StateDB, txs []*types.Transaction) ([]byte, *big.Int, *big.Int, error) {
	return vm.VM.OnFinalizeAndAssemble(header, state, txs)
}

func (vm *VM) onExtraStateChange(block *types.Block, state *state.StateDB) (*big.Int, *big.Int, error) {
	return vm.VM.OnExtraStateChange(block, state)
}

func (vm *VM) SetState(_ context.Context, state snow.State) error {
	switch state {
	case snow.StateSyncing:
		vm.bootstrapped = false
		return nil
	case snow.Bootstrapping:
		vm.bootstrapped = false
		if err := vm.StateSyncClient.Error(); err != nil {
			return err
		}
		// After starting bootstrapping, do not attempt to resume a previous state sync.
		if err := vm.StateSyncClient.ClearOngoingSummary(); err != nil {
			return err
		}
		// Ensure snapshots are initialized before bootstrapping (i.e., if state sync is skipped).
		// Note calling this function has no effect if snapshots are already initialized.
		vm.blockChain.InitializeSnapshots()
		return vm.VM.Bootstrapping()
	case snow.NormalOp:
		// Initialize goroutines related to block building once we enter normal operation as there is no need to handle mempool gossip before this point.
		if err := vm.initBlockBuilding(); err != nil {
			return fmt.Errorf("failed to initialize block building: %w", err)
		}
		vm.bootstrapped = true
		return vm.VM.Bootstrapped()
	default:
		return snow.ErrUnknownState
	}
}

// initBlockBuilding starts goroutines to manage block building
func (vm *VM) initBlockBuilding() error {
	ctx, cancel := context.WithCancel(context.TODO())
	vm.cancel = cancel

	ethTxGossipMarshaller := GossipEthTxMarshaller{}
	ethTxGossipClient := vm.Network.NewClient(ethTxGossipProtocol, p2p.WithValidatorSampling(vm.validators))
	ethTxGossipMetrics, err := gossip.NewMetrics(vm.sdkMetrics, ethTxGossipNamespace)
	if err != nil {
		return fmt.Errorf("failed to initialize eth tx gossip metrics: %w", err)
	}
	ethTxPool, err := NewGossipEthTxPool(vm.txPool, vm.sdkMetrics)
	if err != nil {
		return err
	}
	vm.shutdownWg.Add(1)
	go func() {
		ethTxPool.Subscribe(ctx)
		vm.shutdownWg.Done()
	}()

	atomicTxGossipMarshaller := atx.GossipAtomicTxMarshaller{}
	atomicTxGossipClient := vm.Network.NewClient(atomicTxGossipProtocol, p2p.WithValidatorSampling(vm.validators))
	atomicTxGossipMetrics, err := gossip.NewMetrics(vm.sdkMetrics, atomicTxGossipNamespace)
	if err != nil {
		return fmt.Errorf("failed to initialize atomic tx gossip metrics: %w", err)
	}

	pushGossipParams := gossip.BranchingFactor{
		StakePercentage: vm.config.PushGossipPercentStake,
		Validators:      vm.config.PushGossipNumValidators,
		Peers:           vm.config.PushGossipNumPeers,
	}
	pushRegossipParams := gossip.BranchingFactor{
		Validators: vm.config.PushRegossipNumValidators,
		Peers:      vm.config.PushRegossipNumPeers,
	}

	ethTxPushGossiper := vm.ethTxPushGossiper.Get()
	if ethTxPushGossiper == nil {
		ethTxPushGossiper, err = gossip.NewPushGossiper[*GossipEthTx](
			ethTxGossipMarshaller,
			ethTxPool,
			vm.validators,
			ethTxGossipClient,
			ethTxGossipMetrics,
			pushGossipParams,
			pushRegossipParams,
			pushGossipDiscardedElements,
			txGossipTargetMessageSize,
			vm.config.RegossipFrequency.Duration,
		)
		if err != nil {
			return fmt.Errorf("failed to initialize eth tx push gossiper: %w", err)
		}
		vm.ethTxPushGossiper.Set(ethTxPushGossiper)
	}

	if vm.atomicTxPushGossiper == nil {
		vm.atomicTxPushGossiper, err = gossip.NewPushGossiper[*atx.GossipAtomicTx](
			atomicTxGossipMarshaller,
			vm.Mempool(),
			vm.validators,
			atomicTxGossipClient,
			atomicTxGossipMetrics,
			pushGossipParams,
			pushRegossipParams,
			pushGossipDiscardedElements,
			txGossipTargetMessageSize,
			vm.config.RegossipFrequency.Duration,
		)
		if err != nil {
			return fmt.Errorf("failed to initialize atomic tx push gossiper: %w", err)
		}
	}

	// NOTE: gossip network must be initialized first otherwise ETH tx gossip will not work.
	gossipStats := NewGossipStats()
	vm.builder = vm.NewBlockBuilder(vm.toEngine)
	vm.builder.awaitSubmittedTxs()
	vm.Network.SetGossipHandler(NewGossipHandler(vm, gossipStats))

	if vm.ethTxGossipHandler == nil {
		vm.ethTxGossipHandler = newTxGossipHandler[*GossipEthTx](
			vm.ctx.Log,
			ethTxGossipMarshaller,
			ethTxPool,
			ethTxGossipMetrics,
			txGossipTargetMessageSize,
			txGossipThrottlingPeriod,
			txGossipThrottlingLimit,
			vm.validators,
		)
	}

	if err := vm.Network.AddHandler(ethTxGossipProtocol, vm.ethTxGossipHandler); err != nil {
		return err
	}

	if vm.atomicTxGossipHandler == nil {
		vm.atomicTxGossipHandler = newTxGossipHandler[*atx.GossipAtomicTx](
			vm.ctx.Log,
			atomicTxGossipMarshaller,
			vm.Mempool(),
			atomicTxGossipMetrics,
			txGossipTargetMessageSize,
			txGossipThrottlingPeriod,
			txGossipThrottlingLimit,
			vm.validators,
		)
	}

	if err := vm.Network.AddHandler(atomicTxGossipProtocol, vm.atomicTxGossipHandler); err != nil {
		return err
	}

	if vm.ethTxPullGossiper == nil {
		ethTxPullGossiper := gossip.NewPullGossiper[*GossipEthTx](
			vm.ctx.Log,
			ethTxGossipMarshaller,
			ethTxPool,
			ethTxGossipClient,
			ethTxGossipMetrics,
			txGossipPollSize,
		)

		vm.ethTxPullGossiper = gossip.ValidatorGossiper{
			Gossiper:   ethTxPullGossiper,
			NodeID:     vm.ctx.NodeID,
			Validators: vm.validators,
		}
	}

	vm.shutdownWg.Add(2)
	go func() {
		gossip.Every(ctx, vm.ctx.Log, ethTxPushGossiper, vm.config.PushGossipFrequency.Duration)
		vm.shutdownWg.Done()
	}()
	go func() {
		gossip.Every(ctx, vm.ctx.Log, vm.ethTxPullGossiper, vm.config.PullGossipFrequency.Duration)
		vm.shutdownWg.Done()
	}()

	if vm.atomicTxPullGossiper == nil {
		atomicTxPullGossiper := gossip.NewPullGossiper[*atx.GossipAtomicTx](
			vm.ctx.Log,
			atomicTxGossipMarshaller,
			vm.Mempool(),
			atomicTxGossipClient,
			atomicTxGossipMetrics,
			txGossipPollSize,
		)

		vm.atomicTxPullGossiper = &gossip.ValidatorGossiper{
			Gossiper:   atomicTxPullGossiper,
			NodeID:     vm.ctx.NodeID,
			Validators: vm.validators,
		}
	}

	vm.shutdownWg.Add(2)
	go func() {
		gossip.Every(ctx, vm.ctx.Log, vm.atomicTxPushGossiper, vm.config.PushGossipFrequency.Duration)
		vm.shutdownWg.Done()
	}()
	go func() {
		gossip.Every(ctx, vm.ctx.Log, vm.atomicTxPullGossiper, vm.config.PullGossipFrequency.Duration)
		vm.shutdownWg.Done()
	}()

	return nil
}

// setAppRequestHandlers sets the request handlers for the VM to serve state sync
// requests.
func (vm *VM) setAppRequestHandlers() {
	// Create separate EVM TrieDB (read only) for serving leafs requests.
	// We create a separate TrieDB here, so that it has a separate cache from the one
	// used by the node when processing blocks.
	evmTrieDB := trie.NewDatabase(
		vm.chaindb,
		&trie.Config{
			HashDB: &hashdb.Config{
				CleanCacheSize: vm.config.StateSyncServerTrieCache * units.MiB,
			},
		},
	)
	networkHandler := newNetworkHandler(
		vm.blockChain,
		vm.chaindb,
		evmTrieDB,
		vm.atomicTrie.TrieDB(),
		vm.warpBackend,
		vm.networkCodec,
	)
	vm.Network.SetRequestHandler(networkHandler)
}

// setCrossChainAppRequestHandler sets the request handlers for the VM to serve cross chain
// requests.
func (vm *VM) setCrossChainAppRequestHandler() {
	crossChainRequestHandler := message.NewCrossChainHandler(vm.eth.APIBackend, message.CrossChainCodec)
	vm.Network.SetCrossChainRequestHandler(crossChainRequestHandler)
}

// Shutdown implements the snowman.ChainVM interface
func (vm *VM) Shutdown(context.Context) error {
	if vm.ctx == nil {
		return nil
	}
	if vm.cancel != nil {
		vm.cancel()
	}
	vm.Network.Shutdown()
	if err := vm.StateSyncClient.Shutdown(); err != nil {
		log.Error("error stopping state syncer", "err", err)
	}
	close(vm.shutdownChan)
	vm.eth.Stop()
	vm.shutdownWg.Wait()
	return nil
}

// buildBlock builds a block to be wrapped by ChainState
func (vm *VM) buildBlock(ctx context.Context) (snowman.Block, error) {
	return vm.buildBlockWithContext(ctx, nil)
}

func (vm *VM) buildBlockWithContext(ctx context.Context, proposerVMBlockCtx *block.Context) (snowman.Block, error) {
	if proposerVMBlockCtx != nil {
		log.Debug("Building block with context", "pChainBlockHeight", proposerVMBlockCtx.PChainHeight)
	} else {
		log.Debug("Building block without context")
	}
	predicateCtx := &precompileconfig.PredicateContext{
		SnowCtx:            vm.ctx,
		ProposerVMBlockCtx: proposerVMBlockCtx,
	}

	block, err := vm.miner.GenerateBlock(predicateCtx)
	vm.builder.handleGenerateBlock()
	if err != nil {
		vm.Mempool().CancelCurrentTxs()
		return nil, err
	}

	// Note: the status of block is set by ChainState
	blk, err := vm.newBlock(block)
	if err != nil {
		log.Debug("discarding txs due to error making new block", "err", err)
		vm.Mempool().DiscardCurrentTxs()
		return nil, err
	}

	// Verify is called on a non-wrapped block here, such that this
	// does not add [blk] to the processing blocks map in ChainState.
	//
	// TODO cache verification since Verify() will be called by the
	// consensus engine as well.
	//
	// Note: this is only called when building a new block, so caching
	// verification will only be a significant optimization for nodes
	// that produce a large number of blocks.
	// We call verify without writes here to avoid generating a reference
	// to the blk state root in the triedb when we are going to call verify
	// again from the consensus engine with writes enabled.
	if err := blk.verify(predicateCtx, false /*=writes*/); err != nil {
		vm.Mempool().CancelCurrentTxs()
		return nil, fmt.Errorf("block failed verification due to: %w", err)
	}

	log.Debug(fmt.Sprintf("Built block %s", blk.ID()))
	// Marks the current transactions from the mempool as being successfully issued
	// into a block.
	vm.Mempool().IssueCurrentTxs()
	return blk, nil
}

// parseBlock parses [b] into a block to be wrapped by ChainState.
func (vm *VM) parseBlock(_ context.Context, b []byte) (snowman.Block, error) {
	ethBlock := new(types.Block)
	if err := rlp.DecodeBytes(b, ethBlock); err != nil {
		return nil, err
	}

	// Note: the status of block is set by ChainState
	block, err := vm.newBlock(ethBlock)
	if err != nil {
		return nil, err
	}
	// Performing syntactic verification in ParseBlock allows for
	// short-circuiting bad blocks before they are processed by the VM.
	if err := block.syntacticVerify(); err != nil {
		return nil, fmt.Errorf("syntactic block verification failed: %w", err)
	}
	return block, nil
}

func (vm *VM) ParseEthBlock(b []byte) (*types.Block, error) {
	block, err := vm.parseBlock(context.TODO(), b)
	if err != nil {
		return nil, err
	}

	return block.(*Block).ethBlock, nil
}

// getBlock attempts to retrieve block [id] from the VM to be wrapped
// by ChainState.
func (vm *VM) getBlock(_ context.Context, id ids.ID) (snowman.Block, error) {
	ethBlock := vm.blockChain.GetBlockByHash(common.Hash(id))
	// If [ethBlock] is nil, return [database.ErrNotFound] here
	// so that the miss is considered cacheable.
	if ethBlock == nil {
		return nil, database.ErrNotFound
	}
	// Note: the status of block is set by ChainState
	return vm.newBlock(ethBlock)
}

// SetPreference sets what the current tail of the chain is
func (vm *VM) SetPreference(ctx context.Context, blkID ids.ID) error {
	// Since each internal handler used by [vm.State] always returns a block
	// with non-nil ethBlock value, GetBlockInternal should never return a
	// (*Block) with a nil ethBlock value.
	block, err := vm.GetBlockInternal(ctx, blkID)
	if err != nil {
		return fmt.Errorf("failed to set preference to %s: %w", blkID, err)
	}

	return vm.blockChain.SetPreference(block.(*Block).ethBlock)
}

// VerifyHeightIndex always returns a nil error since the index is maintained by
// vm.blockChain.
func (vm *VM) VerifyHeightIndex(context.Context) error {
	return nil
}

// GetBlockAtHeight returns the canonical block at [blkHeight].
// If [blkHeight] is less than the height of the last accepted block, this will return
// the block accepted at that height. Otherwise, it may return a blkID that has not yet
// been accepted.
// Note: the engine assumes that if a block is not found at [blkHeight], then
// [database.ErrNotFound] will be returned. This indicates that the VM has state synced
// and does not have all historical blocks available.
func (vm *VM) GetBlockIDAtHeight(_ context.Context, blkHeight uint64) (ids.ID, error) {
	ethBlock := vm.blockChain.GetBlockByNumber(blkHeight)
	if ethBlock == nil {
		return ids.ID{}, database.ErrNotFound
	}

	return ids.ID(ethBlock.Hash()), nil
}

func (vm *VM) Version(context.Context) (string, error) {
	return Version, nil
}

// NewHandler returns a new Handler for a service where:
//   - The handler's functionality is defined by [service]
//     [service] should be a gorilla RPC service (see https://www.gorillatoolkit.org/pkg/rpc/v2)
//   - The name of the service is [name]
func newHandler(name string, service interface{}) (http.Handler, error) {
	server := avalancheRPC.NewServer()
	server.RegisterCodec(avalancheJSON.NewCodec(), "application/json")
	server.RegisterCodec(avalancheJSON.NewCodec(), "application/json;charset=UTF-8")
	return server, server.RegisterService(service, name)
}

// CreateHandlers makes new http handlers that can handle API calls
func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	handler := rpc.NewServer(vm.config.APIMaxDuration.Duration)
	enabledAPIs := vm.config.EthAPIs()
	if err := attachEthService(handler, vm.eth.APIs(), enabledAPIs); err != nil {
		return nil, err
	}

	primaryAlias, err := vm.ctx.BCLookup.PrimaryAlias(vm.ctx.ChainID)
	if err != nil {
		return nil, fmt.Errorf("failed to get primary alias for chain due to %w", err)
	}
	apis := make(map[string]http.Handler)
	avaxAPI, err := newHandler("avax", &AvaxAPI{vm})
	if err != nil {
		return nil, fmt.Errorf("failed to register service for AVAX API due to %w", err)
	}
	enabledAPIs = append(enabledAPIs, "avax")
	apis[avaxEndpoint] = avaxAPI

	if vm.config.AdminAPIEnabled {
		adminAPI, err := newHandler("admin", NewAdminService(vm, os.ExpandEnv(fmt.Sprintf("%s_coreth_performance_%s", vm.config.AdminAPIDir, primaryAlias))))
		if err != nil {
			return nil, fmt.Errorf("failed to register service for admin API due to %w", err)
		}
		apis[adminEndpoint] = adminAPI
		enabledAPIs = append(enabledAPIs, "coreth-admin")
	}

	if vm.config.SnowmanAPIEnabled {
		if err := handler.RegisterName("snowman", &SnowmanAPI{vm}); err != nil {
			return nil, err
		}
		enabledAPIs = append(enabledAPIs, "snowman")
	}

	if vm.config.WarpAPIEnabled {
		validatorsState := warpValidators.NewState(vm.ctx)
		if err := handler.RegisterName("warp", warp.NewAPI(vm.ctx.NetworkID, vm.ctx.SubnetID, vm.ctx.ChainID, validatorsState, vm.warpBackend, vm.client)); err != nil {
			return nil, err
		}
		enabledAPIs = append(enabledAPIs, "warp")
	}

	log.Info(fmt.Sprintf("Enabled APIs: %s", strings.Join(enabledAPIs, ", ")))
	apis[ethRPCEndpoint] = handler
	apis[ethWSEndpoint] = handler.WebsocketHandlerWithDuration(
		[]string{"*"},
		vm.config.APIMaxDuration.Duration,
		vm.config.WSCPURefillRate.Duration,
		vm.config.WSCPUMaxStored.Duration,
	)

	return apis, nil
}

// CreateStaticHandlers makes new http handlers that can handle API calls
func (vm *VM) CreateStaticHandlers(context.Context) (map[string]http.Handler, error) {
	handler := rpc.NewServer(0)
	if err := handler.RegisterName("static", &StaticService{}); err != nil {
		return nil, err
	}

	return map[string]http.Handler{
		"/rpc": handler,
	}, nil
}

/*
 ******************************************************************************
 *********************************** Helpers **********************************
 ******************************************************************************
 */

// ParseAddress takes in an address and produces the ID of the chain it's for
// the ID of the address
func (vm *VM) ParseAddress(addrStr string) (ids.ID, ids.ShortID, error) {
	chainIDAlias, hrp, addrBytes, err := address.Parse(addrStr)
	if err != nil {
		return ids.ID{}, ids.ShortID{}, err
	}

	chainID, err := vm.ctx.BCLookup.Lookup(chainIDAlias)
	if err != nil {
		return ids.ID{}, ids.ShortID{}, err
	}

	expectedHRP := avalanchegoConstants.GetHRP(vm.ctx.NetworkID)
	if hrp != expectedHRP {
		return ids.ID{}, ids.ShortID{}, fmt.Errorf("expected hrp %q but got %q",
			expectedHRP, hrp)
	}

	addr, err := ids.ToShortID(addrBytes)
	if err != nil {
		return ids.ID{}, ids.ShortID{}, err
	}
	return chainID, addr, nil
}

// verifyTxAtTip verifies that [tx] is valid to be issued on top of the currently preferred block
func (vm *VM) verifyTxAtTip(tx *Tx) error {
	if txByteLen := len(tx.SignedBytes()); txByteLen > targetAtomicTxsSize {
		return fmt.Errorf("tx size (%d) exceeds total atomic txs size target (%d)", txByteLen, targetAtomicTxsSize)
	}
	gasUsed, err := tx.GasUsed(true)
	if err != nil {
		return err
	}
	if new(big.Int).SetUint64(gasUsed).Cmp(params.AtomicGasLimit) > 0 {
		return fmt.Errorf("tx gas usage (%d) exceeds atomic gas limit (%d)", gasUsed, params.AtomicGasLimit.Uint64())
	}

	// Note: we fetch the current block and then the state at that block instead of the current state directly
	// since we need the header of the current block below.
	preferredBlock := vm.blockChain.CurrentBlock()
	preferredState, err := vm.blockChain.StateAt(preferredBlock.Root)
	if err != nil {
		return fmt.Errorf("failed to retrieve block state at tip while verifying atomic tx: %w", err)
	}
	rules := vm.currentRules()
	parentHeader := preferredBlock
	var nextBaseFee *big.Int
	timestamp := uint64(vm.clock.Time().Unix())
	if vm.chainConfig.IsApricotPhase3(timestamp) {
		_, nextBaseFee, err = dummy.EstimateNextBaseFee(vm.chainConfig, parentHeader, timestamp)
		if err != nil {
			// Return extremely detailed error since CalcBaseFee should never encounter an issue here
			return fmt.Errorf("failed to calculate base fee with parent timestamp (%d), parent ExtraData: (0x%x), and current timestamp (%d): %w", parentHeader.Time, parentHeader.Extra, timestamp, err)
		}
	}

	// We don’t need to revert the state here in case verifyTx errors, because
	// [preferredState] is thrown away either way.
	return vm.verifyTx(tx, parentHeader.Hash(), nextBaseFee, preferredState, rules)
}

// verifyTx verifies that [tx] is valid to be issued into a block with parent block [parentHash]
// and validated at [state] using [rules] as the current rule set.
// Note: verifyTx may modify [state]. If [state] needs to be properly maintained, the caller is responsible
// for reverting to the correct snapshot after calling this function. If this function is called with a
// throwaway state, then this is not necessary.
func (vm *VM) verifyTx(tx *Tx, parentHash common.Hash, baseFee *big.Int, state *state.StateDB, rules params.Rules) error {
	parentIntf, err := vm.GetBlockInternal(context.TODO(), ids.ID(parentHash))
	if err != nil {
		return fmt.Errorf("failed to get parent block: %w", err)
	}
	parent, ok := parentIntf.(*Block)
	if !ok {
		return fmt.Errorf("parent block %s had unexpected type %T", parentIntf.ID(), parentIntf)
	}
	if err := tx.UnsignedAtomicTx.SemanticVerify(vm.VM, tx, parent.ID(), baseFee, rules); err != nil {
		return err
	}
	return tx.UnsignedAtomicTx.EVMStateTransfer(vm.ctx, state)
}

// verifyTxs verifies that [txs] are valid to be issued into a block with parent block [parentHash]
// using [rules] as the current rule set.
func (vm *VM) verifyTxs(txs []*Tx, parentHash common.Hash, baseFee *big.Int, height uint64, rules params.Rules) error {
	// Ensure that the parent was verified and inserted correctly.
	if !vm.blockChain.HasBlock(parentHash, height-1) {
		return errRejectedParent
	}

	ancestorID := ids.ID(parentHash)
	// If the ancestor is unknown, then the parent failed verification when
	// it was called.
	// If the ancestor is rejected, then this block shouldn't be inserted
	// into the canonical chain because the parent will be missing.
	ancestorInf, err := vm.GetBlockInternal(context.TODO(), ancestorID)
	if err != nil {
		return errRejectedParent
	}
	if blkStatus := ancestorInf.Status(); blkStatus == choices.Unknown || blkStatus == choices.Rejected {
		return errRejectedParent
	}
	ancestor, ok := ancestorInf.(*Block)
	if !ok {
		return fmt.Errorf("expected parent block %s, to be *Block but is %T", ancestor.ID(), ancestorInf)
	}

	// Ensure each tx in [txs] doesn't conflict with any other atomic tx in
	// a processing ancestor block.
	inputs := set.Set[ids.ID]{}
	for _, atomicTx := range txs {
		utx := atomicTx.UnsignedAtomicTx
		if err := utx.SemanticVerify(vm.VM, atomicTx, ancestor.ID(), baseFee, rules); err != nil {
			return fmt.Errorf("invalid block due to failed semanatic verify: %w at height %d", err, height)
		}
		txInputs := utx.InputUTXOs()
		if inputs.Overlaps(txInputs) {
			return errConflictingAtomicInputs
		}
		inputs.Union(txInputs)
	}
	return nil
}

func (vm *VM) GetBlockAndAtomicTxs(blkID ids.ID) ([]*Tx, choices.Status, ids.ID, error) {
	blkIntf, err := vm.GetBlockInternal(context.TODO(), blkID)
	if err != nil {
		return nil, choices.Unknown, ids.ID{}, err
	}
	blk, ok := blkIntf.(*Block)
	if !ok {
		return nil, choices.Unknown, ids.ID{}, fmt.Errorf("block %s had unexpected type %T", blk.ID(), blkIntf)
	}
	return blk.atomicTxs, blk.Status(), blk.Parent(), nil
}

// currentRules returns the chain rules for the current block.
func (vm *VM) currentRules() params.Rules {
	header := vm.eth.APIBackend.CurrentHeader()
	return vm.chainConfig.Rules(header.Number, header.Time)
}

func (vm *VM) startContinuousProfiler() {
	// If the profiler directory is empty, return immediately
	// without creating or starting a continuous profiler.
	if vm.config.ContinuousProfilerDir == "" {
		return
	}
	vm.profiler = profiler.NewContinuous(
		filepath.Join(vm.config.ContinuousProfilerDir),
		vm.config.ContinuousProfilerFrequency.Duration,
		vm.config.ContinuousProfilerMaxFiles,
	)
	defer vm.profiler.Shutdown()

	vm.shutdownWg.Add(1)
	go func() {
		defer vm.shutdownWg.Done()
		log.Info("Dispatching continuous profiler", "dir", vm.config.ContinuousProfilerDir, "freq", vm.config.ContinuousProfilerFrequency, "maxFiles", vm.config.ContinuousProfilerMaxFiles)
		err := vm.profiler.Dispatch()
		if err != nil {
			log.Error("continuous profiler failed", "err", err)
		}
	}()
	// Wait for shutdownChan to be closed
	<-vm.shutdownChan
}

func (vm *VM) estimateBaseFee(ctx context.Context) (*big.Int, error) {
	// Get the base fee to use
	baseFee, err := vm.eth.APIBackend.EstimateBaseFee(ctx)
	if err != nil {
		return nil, err
	}
	if baseFee == nil {
		baseFee = initialBaseFee
	} else {
		// give some breathing room
		baseFee.Mul(baseFee, big.NewInt(11))
		baseFee.Div(baseFee, big.NewInt(10))
	}

	return baseFee, nil
}

func (vm *VM) getAtomicTxFromPreApricot5BlockByHeight(height uint64) (*Tx, error) {
	blk := vm.blockChain.GetBlockByNumber(height)
	if blk == nil {
		return nil, nil
	}
	return ExtractAtomicTx(blk.ExtData(), vm.codec)
}

// readLastAccepted reads the last accepted hash from [acceptedBlockDB] and returns the
// last accepted block hash and height by reading directly from [vm.chaindb] instead of relying
// on [chain].
// Note: assumes [vm.chaindb] and [vm.genesisHash] have been initialized.
func (vm *VM) readLastAccepted() (common.Hash, uint64, error) {
	// Attempt to load last accepted block to determine if it is necessary to
	// initialize state with the genesis block.
	lastAcceptedBytes, lastAcceptedErr := vm.acceptedBlockDB.Get(lastAcceptedKey)
	switch {
	case lastAcceptedErr == database.ErrNotFound:
		// If there is nothing in the database, return the genesis block hash and height
		return vm.genesisHash, 0, nil
	case lastAcceptedErr != nil:
		return common.Hash{}, 0, fmt.Errorf("failed to get last accepted block ID due to: %w", lastAcceptedErr)
	case len(lastAcceptedBytes) != common.HashLength:
		return common.Hash{}, 0, fmt.Errorf("last accepted bytes should have been length %d, but found %d", common.HashLength, len(lastAcceptedBytes))
	default:
		lastAcceptedHash := common.BytesToHash(lastAcceptedBytes)
		height := rawdb.ReadHeaderNumber(vm.chaindb, lastAcceptedHash)
		if height == nil {
			return common.Hash{}, 0, fmt.Errorf("failed to retrieve header number of last accepted block: %s", lastAcceptedHash)
		}
		return lastAcceptedHash, *height, nil
	}
}

// attachEthService registers the backend RPC services provided by Ethereum
// to the provided handler under their assigned namespaces.
func attachEthService(handler *rpc.Server, apis []rpc.API, names []string) error {
	enabledServicesSet := make(map[string]struct{})
	for _, ns := range names {
		// handle pre geth v1.10.20 api names as aliases for their updated values
		// to allow configurations to be backwards compatible.
		if newName, isLegacy := legacyApiNames[ns]; isLegacy {
			log.Info("deprecated api name referenced in configuration.", "deprecated", ns, "new", newName)
			enabledServicesSet[newName] = struct{}{}
			continue
		}

		enabledServicesSet[ns] = struct{}{}
	}

	apiSet := make(map[string]rpc.API)
	for _, api := range apis {
		if existingAPI, exists := apiSet[api.Name]; exists {
			return fmt.Errorf("duplicated API name: %s, namespaces %s and %s", api.Name, api.Namespace, existingAPI.Namespace)
		}
		apiSet[api.Name] = api
	}

	for name := range enabledServicesSet {
		api, exists := apiSet[name]
		if !exists {
			return fmt.Errorf("API service %s not found", name)
		}
		if err := handler.RegisterName(api.Namespace, api.Service); err != nil {
			return err
		}
	}

	return nil
}

func (vm *VM) stateSyncEnabled(lastAcceptedHeight uint64) bool {
	if vm.config.StateSyncEnabled != nil {
		// if the config is set, use that
		return *vm.config.StateSyncEnabled
	}

	// enable state sync by default if the chain is empty.
	return lastAcceptedHeight == 0
}
