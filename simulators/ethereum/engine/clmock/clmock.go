package clmock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"time"

	api "github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/hive/simulators/ethereum/engine/client"
	"github.com/ethereum/hive/simulators/ethereum/engine/globals"
	"github.com/ethereum/hive/simulators/ethereum/engine/helper"
	typ "github.com/ethereum/hive/simulators/ethereum/engine/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/hive/hivesim"
)

var (
	DefaultSlotsToSafe      = big.NewInt(1)
	DefaultSlotsToFinalized = big.NewInt(2)

	// Time delay between ForkchoiceUpdated and GetPayload to allow the clients
	// to produce a new Payload
	DefaultPayloadProductionClientDelay = time.Second

	// Fork specific constants
	BLOB_COMMITMENT_VERSION_KZG = byte(0x01)
)

type ExecutableDataHistory map[uint64]*typ.ExecutableData

func (h ExecutableDataHistory) LatestPayloadNumber() uint64 {
	latest := uint64(0)
	for n := range h {
		if n > latest {
			latest = n
		}
	}
	return latest
}

func (h ExecutableDataHistory) LatestWithdrawalsIndex() uint64 {
	latest := uint64(0)
	for _, p := range h {
		if p.Withdrawals != nil {
			for _, w := range p.Withdrawals {
				if w.Index > latest {
					latest = w.Index
				}
			}
		}
	}
	return latest
}

// Consensus Layer Client Mock used to sync the Execution Clients once the TTD has been reached
type CLMocker struct {
	*hivesim.T
	// List of Engine Clients being served by the CL Mocker
	EngineClients []client.EngineClient
	// Lock required so no client is offboarded during block production.
	EngineClientsLock sync.Mutex
	// Number of required slots before a block which was set as Head moves to `safe` and `finalized` respectively
	SlotsToSafe      *big.Int
	SlotsToFinalized *big.Int

	// Wait time before attempting to get the payload
	PayloadProductionClientDelay time.Duration

	// Block production related
	BlockTimestampIncrement *big.Int

	// Block Production State
	NextBlockProducer    client.EngineClient
	NextFeeRecipient     common.Address
	NextPayloadID        *api.PayloadID
	CurrentPayloadNumber uint64

	// Chain History
	HeaderHistory map[uint64]*types.Header

	// PoS Chain History Information
	PrevRandaoHistory      map[uint64]common.Hash
	ExecutedPayloadHistory ExecutableDataHistory
	HeadHashHistory        []common.Hash

	// Latest broadcasted data using the PoS Engine API
	LatestHeadNumber        *big.Int
	LatestHeader            *types.Header
	LatestPayloadBuilt      typ.ExecutableData
	LatestBlockValue        *big.Int
	LatestBlobBundle        *typ.BlobsBundle
	LatestPayloadAttributes typ.PayloadAttributes
	LatestExecutedPayload   typ.ExecutableData
	LatestForkchoice        api.ForkchoiceStateV1

	// Merge related
	FirstPoSBlockNumber             *big.Int
	TTDReached                      bool
	TransitionPayloadTimestamp      *big.Int
	SafeSlotsToImportOptimistically *big.Int
	ChainTotalDifficulty            *big.Int

	// Fork configuration
	*globals.ForkConfig
	Genesis *core.Genesis

	NextWithdrawals types.Withdrawals

	// Global context which all procedures shall stop
	TestContext    context.Context
	TimeoutContext context.Context
}

func NewCLMocker(t *hivesim.T, genesis *core.Genesis, slotsToSafe, slotsToFinalized, safeSlotsToImportOptimistically *big.Int, forkConfig globals.ForkConfig) *CLMocker {
	// Init random seed for different purposes
	seed := time.Now().Unix()
	t.Logf("Randomness seed: %v\n", seed)
	rand.Seed(seed)

	if slotsToSafe == nil {
		// Use default
		slotsToSafe = DefaultSlotsToSafe
	}
	if slotsToFinalized == nil {
		// Use default
		slotsToFinalized = DefaultSlotsToFinalized
	}
	// Create the new CL mocker
	newCLMocker := &CLMocker{
		T:                               t,
		EngineClients:                   make([]client.EngineClient, 0),
		PrevRandaoHistory:               map[uint64]common.Hash{},
		ExecutedPayloadHistory:          ExecutableDataHistory{},
		SlotsToSafe:                     slotsToSafe,
		SlotsToFinalized:                slotsToFinalized,
		SafeSlotsToImportOptimistically: safeSlotsToImportOptimistically,
		PayloadProductionClientDelay:    DefaultPayloadProductionClientDelay,
		LatestHeader:                    nil,
		FirstPoSBlockNumber:             nil,
		LatestHeadNumber:                nil,
		TTDReached:                      false,
		NextFeeRecipient:                common.Address{},
		LatestForkchoice: api.ForkchoiceStateV1{
			HeadBlockHash:      common.Hash{},
			SafeBlockHash:      common.Hash{},
			FinalizedBlockHash: common.Hash{},
		},
		ChainTotalDifficulty: genesis.Difficulty,
		ForkConfig:           &forkConfig,
		Genesis:              genesis,
		TestContext:          context.Background(),
	}

	// Create header history
	newCLMocker.HeaderHistory = make(map[uint64]*types.Header)

	// Add genesis to the header history
	newCLMocker.HeaderHistory[0] = genesis.ToBlock().Header()

	return newCLMocker
}

// Return genesis block of the canonical chain
func (cl *CLMocker) GenesisBlock() *types.Block {
	return cl.Genesis.ToBlock()
}

// Add a Client to be kept in sync with the latest payloads
func (cl *CLMocker) AddEngineClient(ec client.EngineClient) {
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()
	cl.Logf("CLMocker: Adding engine client %v", ec.ID())
	cl.EngineClients = append(cl.EngineClients, ec)
}

// Remove a Client to stop sending latest payloads
func (cl *CLMocker) RemoveEngineClient(ec client.EngineClient) {
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()

	cl.Logf("CLMocker: Removing engine client %v", ec.ID())
	for i, engine := range cl.EngineClients {
		if engine.ID() == ec.ID() {
			cl.EngineClients = append(cl.EngineClients[:i], cl.EngineClients[i+1:]...)
			// engine.Close()
			return
		}
	}
}

// Close all the engine clients
func (cl *CLMocker) CloseClients() {
	for _, engine := range cl.EngineClients {
		cl.Logf("CLMocker: Closing engine client %v", engine.ID())
		if err := engine.Close(); err != nil {
			panic(err)
		}
		cl.Logf("CLMocker: Closed engine client %v", engine.ID())
	}
}

func (cl *CLMocker) IsOptimisticallySyncing() bool {
	if cl.SafeSlotsToImportOptimistically.Cmp(common.Big0) == 0 {
		return true
	}
	if cl.FirstPoSBlockNumber == nil {
		return false
	}
	diff := big.NewInt(int64(cl.LatestExecutedPayload.Number) - cl.FirstPoSBlockNumber.Int64())
	return diff.Cmp(cl.SafeSlotsToImportOptimistically) >= 0
}

func (cl *CLMocker) ForkID() forkid.ID {
	return forkid.NewID(cl.Genesis.Config, cl.GenesisBlock().Hash(), cl.LatestHeader.Number.Uint64(), cl.Genesis.Timestamp)
}

func (cl *CLMocker) GetHeaders(amount uint64, originHash common.Hash, originNumber uint64, reverse bool, skip uint64) ([]*types.Header, error) {
	if amount < 1 {
		return nil, errors.New("no block headers requested")
	}

	headers := make([]*types.Header, amount)
	var blockNumber uint64

	// range over blocks to check if our chain has the requested header
	for _, h := range cl.HeaderHistory {
		if h.Hash() == originHash || h.Number.Uint64() == originNumber {
			headers[0] = h
			blockNumber = h.Number.Uint64()
		}
	}
	if headers[0] == nil {
		return nil, fmt.Errorf("no headers found for given origin number %v, hash %v", originNumber, originHash)
	}

	if reverse {
		for i := 1; i < int(amount); i++ {
			blockNumber -= (1 - skip)
			headers[i] = cl.HeaderHistory[blockNumber]
		}
		return headers, nil
	}

	for i := 1; i < int(amount); i++ {
		blockNumber += (1 + skip)
		headers[i] = cl.HeaderHistory[blockNumber]
	}

	return headers, nil
}

// Sets the specified client's chain head as Terminal PoW block by sending the initial forkchoiceUpdated.
func (cl *CLMocker) SetTTDBlockClient(ec client.EngineClient) {
	var err error
	ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
	defer cancel()

	cl.LatestHeader, err = ec.HeaderByNumber(ctx, nil)
	if err != nil {
		cl.Fatalf("CLMocker: Unable to get latest header: %v", err)
	}
	cl.HeaderHistory[cl.LatestHeader.Number.Uint64()] = cl.LatestHeader

	ctx, cancel = context.WithTimeout(cl.TestContext, globals.RPCTimeout)
	defer cancel()

	if td, err := ec.GetTotalDifficulty(ctx); err != nil {
		cl.Fatalf("CLMocker: Error getting total difficulty from engine client: %v", err)
	} else if td.Cmp(ec.TerminalTotalDifficulty()) < 0 {
		cl.Fatalf("CLMocker: Attempted to set TTD Block when TTD had not been reached: %d > %d", ec.TerminalTotalDifficulty(), td)
	} else {
		cl.Logf("CLMocker: TTD has been reached at block %d (%d>=%d)\n", cl.LatestHeader.Number, td, ec.TerminalTotalDifficulty())
		jsH, _ := json.MarshalIndent(cl.LatestHeader, "", " ")
		cl.Logf("CLMocker: Client: %s, Block %d: %s\n", ec.ID(), cl.LatestHeader.Number, jsH)
		cl.ChainTotalDifficulty = td
	}

	cl.TTDReached = true

	// Reset transition values
	cl.LatestHeadNumber = cl.LatestHeader.Number
	cl.HeadHashHistory = []common.Hash{}
	cl.FirstPoSBlockNumber = nil

	// Prepare initial forkchoice, to be sent to the transition payload producer
	cl.LatestForkchoice = api.ForkchoiceStateV1{}
	cl.LatestForkchoice.HeadBlockHash = cl.LatestHeader.Hash()

}

// This method waits for TTD in at least one of the clients, then sends the
// initial forkchoiceUpdated with the info obtained from the client.
func (cl *CLMocker) WaitForTTD() {
	ec, err := helper.WaitAnyClientForTTD(cl.EngineClients, cl.TestContext)
	if ec == nil || err != nil {
		cl.Fatalf("CLMocker: Error while waiting for TTD: %v", err)
	}
	// One of the clients has reached TTD, send the initial fcU with this client as head
	cl.SetTTDBlockClient(ec)
}

// Check whether a block number is a PoS block
func (cl *CLMocker) IsBlockPoS(bn *big.Int) bool {
	if cl.FirstPoSBlockNumber == nil || cl.FirstPoSBlockNumber.Cmp(bn) > 0 {
		return false
	}
	return true
}

// Return the per-block timestamp value increment
func (cl *CLMocker) GetTimestampIncrement() uint64 {
	if cl.BlockTimestampIncrement == nil {
		return 1
	}
	return cl.BlockTimestampIncrement.Uint64()
}

// Returns the timestamp value to be included in the next payload attributes
func (cl *CLMocker) GetNextBlockTimestamp() uint64 {
	if cl.FirstPoSBlockNumber == nil && cl.TransitionPayloadTimestamp != nil {
		// We are producing the transition payload and there's a value specified
		// for this specific payload
		return cl.TransitionPayloadTimestamp.Uint64()
	}
	return cl.LatestHeader.Time + cl.GetTimestampIncrement()
}

// Picks the next payload producer from the set of clients registered
func (cl *CLMocker) pickNextPayloadProducer() {
	if len(cl.EngineClients) == 0 {
		cl.Fatalf("CLMocker: No clients left for block production")
	}

	for i := 0; i < len(cl.EngineClients); i++ {
		// Get a client to generate the payload
		ec_id := (int(cl.LatestHeadNumber.Int64()) + i) % len(cl.EngineClients)
		cl.NextBlockProducer = cl.EngineClients[ec_id]

		cl.Logf("CLMocker: Selected payload producer: %s", cl.NextBlockProducer.ID())

		// Get latest header. Number and hash must coincide with our view of the chain,
		// and only then we can build on top of this client's chain
		ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
		defer cancel()
		latestHeader, err := cl.NextBlockProducer.HeaderByNumber(ctx, nil)
		if err != nil {
			cl.Fatalf("CLMocker: Could not get latest block header while selecting client for payload production (%s): %v", cl.NextBlockProducer.ID(), err)
		}

		lastBlockHash := latestHeader.Hash()

		if cl.LatestHeader.Hash() != lastBlockHash || cl.LatestHeadNumber.Cmp(latestHeader.Number) != 0 {
			// Selected client latest block hash does not match canonical chain, try again
			cl.NextBlockProducer = nil
			continue
		} else {
			break
		}

	}

	if cl.NextBlockProducer == nil {
		cl.Fatalf("CLMocker: Failed to obtain a client on the latest block number")
	}
}

func (cl *CLMocker) SetNextWithdrawals(nextWithdrawals types.Withdrawals) {
	cl.NextWithdrawals = nextWithdrawals
}

func TimestampToBeaconRoot(timestamp uint64) common.Hash {
	// Generates a deterministic hash from the timestamp
	beaconRoot := common.Hash{}
	timestampBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampBytes[:], timestamp)
	md := sha256.New()
	md.Write(timestampBytes)
	timestampHash := md.Sum(nil)
	copy(beaconRoot[:], timestampHash)
	return beaconRoot
}

func (cl *CLMocker) RequestNextPayload() {
	// Generate a random value for the PrevRandao field
	nextPrevRandao := common.Hash{}
	rand.Read(nextPrevRandao[:])

	cl.LatestPayloadAttributes = typ.PayloadAttributes{
		Random:                nextPrevRandao,
		SuggestedFeeRecipient: cl.NextFeeRecipient,
		Timestamp:             cl.GetNextBlockTimestamp(),
	}

	if cl.IsShanghai(cl.LatestPayloadAttributes.Timestamp) && cl.NextWithdrawals != nil {
		cl.LatestPayloadAttributes.Withdrawals = cl.NextWithdrawals
	}

	if cl.IsCancun(cl.LatestPayloadAttributes.Timestamp) {
		// Write a deterministic hash based on the block number
		beaconRoot := TimestampToBeaconRoot(cl.LatestPayloadAttributes.Timestamp)
		cl.LatestPayloadAttributes.BeaconRoot = &beaconRoot
	}

	// Save random value
	cl.PrevRandaoHistory[cl.LatestHeader.Number.Uint64()+1] = nextPrevRandao

	ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
	defer cancel()
	fcUVersion := cl.ForkchoiceUpdatedVersion(cl.LatestPayloadAttributes.Timestamp)
	resp, err := cl.NextBlockProducer.ForkchoiceUpdated(ctx, fcUVersion, &cl.LatestForkchoice, &cl.LatestPayloadAttributes)
	if err != nil {
		cl.Fatalf("CLMocker: Could not send forkchoiceUpdatedV%d (%v): %v", fcUVersion, cl.NextBlockProducer.ID(), err)
	}
	if resp.PayloadStatus.Status != api.VALID {
		cl.Fatalf("CLMocker: Unexpected forkchoiceUpdated Response from Payload builder: %v", resp)
	}
	if resp.PayloadStatus.LatestValidHash == nil || *resp.PayloadStatus.LatestValidHash != cl.LatestForkchoice.HeadBlockHash {
		cl.Fatalf("CLMocker: Unexpected forkchoiceUpdated LatestValidHash Response from Payload builder: %v != %v", resp.PayloadStatus.LatestValidHash, cl.LatestForkchoice.HeadBlockHash)
	}
	cl.NextPayloadID = resp.PayloadID
}

func (cl *CLMocker) GetNextPayload() {
	var err error
	ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
	defer cancel()
	if cl.IsCancun(cl.LatestPayloadAttributes.Timestamp) {
		cl.LatestPayloadBuilt, cl.LatestBlockValue, cl.LatestBlobBundle, err = cl.NextBlockProducer.GetPayloadV3(ctx, cl.NextPayloadID)
	} else if cl.IsShanghai(cl.LatestPayloadAttributes.Timestamp) {
		cl.LatestPayloadBuilt, cl.LatestBlockValue, err = cl.NextBlockProducer.GetPayloadV2(ctx, cl.NextPayloadID)
		cl.LatestBlobBundle = nil
	} else {
		cl.LatestPayloadBuilt, err = cl.NextBlockProducer.GetPayloadV1(ctx, cl.NextPayloadID)
		cl.LatestBlockValue = nil
		cl.LatestBlobBundle = nil
	}
	if err != nil {
		cl.Fatalf("CLMocker: Could not getPayload (%v, %v): %v", cl.NextBlockProducer.ID(), cl.NextPayloadID, err)
	}
	if cl.LatestPayloadBuilt.Timestamp != cl.LatestPayloadAttributes.Timestamp {
		cl.Fatalf("CLMocker: Incorrect Timestamp on payload built: %d != %d", cl.LatestPayloadBuilt.Timestamp, cl.LatestPayloadAttributes.Timestamp)
	}
	if cl.LatestPayloadBuilt.FeeRecipient != cl.LatestPayloadAttributes.SuggestedFeeRecipient {
		cl.Fatalf("CLMocker: Incorrect SuggestedFeeRecipient on payload built: %v != %v", cl.LatestPayloadBuilt.FeeRecipient, cl.LatestPayloadAttributes.SuggestedFeeRecipient)
	}
	if cl.LatestPayloadBuilt.Random != cl.LatestPayloadAttributes.Random {
		cl.Fatalf("CLMocker: Incorrect PrevRandao on payload built: %v != %v", cl.LatestPayloadBuilt.Random, cl.LatestPayloadAttributes.Random)
	}
	if cl.LatestPayloadBuilt.ParentHash != cl.LatestHeader.Hash() {
		cl.Fatalf("CLMocker: Incorrect ParentHash on payload built: %v != %v", cl.LatestPayloadBuilt.ParentHash, cl.LatestHeader.Hash())
	}
	if cl.LatestPayloadBuilt.Number != cl.LatestHeader.Number.Uint64()+1 {
		cl.Fatalf("CLMocker: Incorrect Number on payload built: %v != %v", cl.LatestPayloadBuilt.Number, cl.LatestHeader.Number.Uint64()+1)
	}
}

func (cl *CLMocker) broadcastNextNewPayload() {
	// Check if we have blobs to include in the broadcast
	var versionedHashes *[]common.Hash
	if cl.LatestBlobBundle != nil {
		// Broadcast the blob bundle to all clients
		var err error
		versionedHashes, err = cl.LatestBlobBundle.VersionedHashes(BLOB_COMMITMENT_VERSION_KZG)
		if err != nil {
			cl.Fatalf("CLMocker: Could not get versioned hashes from blob bundle: %v", err)
		}
	}
	// Broadcast the executePayload to all clients
	version := cl.NewPayloadVersion(cl.LatestPayloadBuilt.Timestamp)
	validations := 0
	for _, resp := range cl.BroadcastNewPayload(&cl.LatestPayloadBuilt, versionedHashes, cl.LatestPayloadAttributes.BeaconRoot, version) {
		if resp.Error != nil {
			cl.Logf("CLMocker: BroadcastNewPayload Error (%v): %v\n", resp.Container, resp.Error)
		} else {
			if resp.ExecutePayloadResponse.Status == api.VALID {
				// The client is synced and the payload was immediately validated
				// https://github.com/ethereum/execution-apis/blob/main/src/engine/specification.md:
				// - If validation succeeds, the response MUST contain {status: VALID, latestValidHash: payload.blockHash}
				if resp.ExecutePayloadResponse.LatestValidHash == nil {
					cl.Fatalf("CLMocker: NewPayload returned VALID status with nil LatestValidHash, expected %v", cl.LatestPayloadBuilt.BlockHash)
				}
				if *resp.ExecutePayloadResponse.LatestValidHash != cl.LatestPayloadBuilt.BlockHash {
					cl.Fatalf("CLMocker: NewPayload returned VALID status with incorrect LatestValidHash==%v, expected %v", resp.ExecutePayloadResponse.LatestValidHash, cl.LatestPayloadBuilt.BlockHash)
				}
				validations += 1
			} else if resp.ExecutePayloadResponse.Status == api.ACCEPTED {
				// The client is not synced but the payload was accepted
				// https://github.com/ethereum/execution-apis/blob/main/src/engine/specification.md:
				// - {status: ACCEPTED, latestValidHash: null, validationError: null} if the following conditions are met:
				// the blockHash of the payload is valid
				// the payload doesn't extend the canonical chain
				// the payload hasn't been fully validated.
				if resp.ExecutePayloadResponse.LatestValidHash != nil && *resp.ExecutePayloadResponse.LatestValidHash != (common.Hash{}) {
					cl.Fatalf("CLMocker: NewPayload returned ACCEPTED status with incorrect LatestValidHash==%v", resp.ExecutePayloadResponse.LatestValidHash)
				}
			} else {
				cl.Logf("CLMocker: BroadcastNewPayload Response (%v): %v\n", resp.Container, resp.ExecutePayloadResponse)
			}
		}
	}
	if validations == 0 {
		cl.Fatalf("CLMocker: No clients validated the payload")
	}
	cl.LatestExecutedPayload = cl.LatestPayloadBuilt
	payload := cl.LatestPayloadBuilt
	cl.ExecutedPayloadHistory[cl.LatestPayloadBuilt.Number] = &payload
}

func (cl *CLMocker) broadcastLatestForkchoice() {
	version := cl.ForkchoiceUpdatedVersion(cl.LatestExecutedPayload.Timestamp)
	for _, resp := range cl.BroadcastForkchoiceUpdated(&cl.LatestForkchoice, nil, version) {
		if resp.Error != nil {
			cl.Logf("CLMocker: BroadcastForkchoiceUpdated Error (%v): %v\n", resp.Container, resp.Error)
		} else if resp.ForkchoiceResponse.PayloadStatus.Status == api.VALID {
			// {payloadStatus: {status: VALID, latestValidHash: forkchoiceState.headBlockHash, validationError: null},
			//  payloadId: null}
			if *resp.ForkchoiceResponse.PayloadStatus.LatestValidHash != cl.LatestForkchoice.HeadBlockHash {
				cl.Fatalf("CLMocker: Incorrect LatestValidHash from ForkchoiceUpdated (%v): %v != %v\n", resp.Container, resp.ForkchoiceResponse.PayloadStatus.LatestValidHash, cl.LatestForkchoice.HeadBlockHash)
			}
			if resp.ForkchoiceResponse.PayloadStatus.ValidationError != nil {
				cl.Fatalf("CLMocker: Expected empty validationError: %s\n", resp.Container, *resp.ForkchoiceResponse.PayloadStatus.ValidationError)
			}
			if resp.ForkchoiceResponse.PayloadID != nil {
				cl.Fatalf("CLMocker: Expected empty PayloadID: %v\n", resp.Container, resp.ForkchoiceResponse.PayloadID)
			}
		} else if resp.ForkchoiceResponse.PayloadStatus.Status != api.VALID {
			cl.Logf("CLMocker: BroadcastForkchoiceUpdated Response (%v): %v\n", resp.Container, resp.ForkchoiceResponse)
		}
	}

}

type BlockProcessCallbacks struct {
	OnPayloadProducerSelected func()
	OnRequestNextPayload      func()
	OnGetPayload              func()
	OnNewPayloadBroadcast     func()
	OnForkchoiceBroadcast     func()
	OnSafeBlockChange         func()
	OnFinalizedBlockChange    func()
}

func (cl *CLMocker) ProduceSingleBlock(callbacks BlockProcessCallbacks) {

	if !cl.TTDReached {
		cl.Fatalf("CLMocker: Attempted to create a block when the TTD had not yet been reached")
	}

	// Lock needed to ensure EngineClients is not modified mid block production
	cl.EngineClientsLock.Lock()
	defer cl.EngineClientsLock.Unlock()

	cl.CurrentPayloadNumber = cl.LatestHeader.Number.Uint64() + 1

	cl.pickNextPayloadProducer()

	// Check if next withdrawals necessary, test can override this value on
	// `OnPayloadProducerSelected` callback
	if cl.NextWithdrawals == nil {
		cl.SetNextWithdrawals(make(types.Withdrawals, 0))
	}

	if callbacks.OnPayloadProducerSelected != nil {
		callbacks.OnPayloadProducerSelected()
	}

	cl.RequestNextPayload()

	cl.SetNextWithdrawals(nil)

	if callbacks.OnRequestNextPayload != nil {
		callbacks.OnRequestNextPayload()
	}

	// Give the client a delay between getting the payload ID and actually retrieving the payload
	time.Sleep(cl.PayloadProductionClientDelay)

	cl.GetNextPayload()

	if callbacks.OnGetPayload != nil {
		callbacks.OnGetPayload()
	}

	cl.broadcastNextNewPayload()

	if callbacks.OnNewPayloadBroadcast != nil {
		callbacks.OnNewPayloadBroadcast()
	}

	// Broadcast forkchoice updated with new HeadBlock to all clients
	previousForkchoice := cl.LatestForkchoice
	cl.HeadHashHistory = append(cl.HeadHashHistory, cl.LatestPayloadBuilt.BlockHash)

	cl.LatestForkchoice = api.ForkchoiceStateV1{}
	cl.LatestForkchoice.HeadBlockHash = cl.LatestPayloadBuilt.BlockHash
	if len(cl.HeadHashHistory) > int(cl.SlotsToSafe.Int64()) {
		cl.LatestForkchoice.SafeBlockHash = cl.HeadHashHistory[len(cl.HeadHashHistory)-int(cl.SlotsToSafe.Int64())-1]
	}
	if len(cl.HeadHashHistory) > int(cl.SlotsToFinalized.Int64()) {
		cl.LatestForkchoice.FinalizedBlockHash = cl.HeadHashHistory[len(cl.HeadHashHistory)-int(cl.SlotsToFinalized.Int64())-1]
	}
	cl.broadcastLatestForkchoice()

	if callbacks.OnForkchoiceBroadcast != nil {
		callbacks.OnForkchoiceBroadcast()
	}

	// Broadcast forkchoice updated with new SafeBlock to all clients
	if callbacks.OnSafeBlockChange != nil && previousForkchoice.SafeBlockHash != cl.LatestForkchoice.SafeBlockHash {
		callbacks.OnSafeBlockChange()
	}
	// Broadcast forkchoice updated with new FinalizedBlock to all clients
	if callbacks.OnFinalizedBlockChange != nil && previousForkchoice.FinalizedBlockHash != cl.LatestForkchoice.FinalizedBlockHash {
		callbacks.OnFinalizedBlockChange()
	}

	// Save the number of the first PoS block
	if cl.FirstPoSBlockNumber == nil {
		cl.FirstPoSBlockNumber = big.NewInt(int64(cl.LatestHeader.Number.Uint64() + 1))
	}

	// Save the header of the latest block in the PoS chain
	cl.LatestHeadNumber = cl.LatestHeadNumber.Add(cl.LatestHeadNumber, common.Big1)

	// Check if any of the clients accepted the new payload
	cl.LatestHeader = nil
	for _, ec := range cl.EngineClients {
		ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
		defer cancel()
		newHeader, err := ec.HeaderByNumber(ctx, cl.LatestHeadNumber)
		if err != nil {
			continue
		}
		if newHeader.Hash() != cl.LatestPayloadBuilt.BlockHash {
			continue
		}
		// Check that the new finalized header has the correct properties
		// ommersHash == 0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347
		if newHeader.UncleHash != types.EmptyUncleHash {
			cl.Fatalf("CLMocker: Client %v produced a new header with incorrect ommersHash: %v", ec.ID(), newHeader.UncleHash)
		}
		// difficulty == 0
		if newHeader.Difficulty.Cmp(common.Big0) != 0 {
			cl.Fatalf("CLMocker: Client %v produced a new header with incorrect difficulty: %v", ec.ID(), newHeader.Difficulty)
		}
		// mixHash == prevRandao
		if newHeader.MixDigest != cl.PrevRandaoHistory[cl.LatestHeadNumber.Uint64()] {
			cl.Fatalf("CLMocker: Client %v produced a new header with incorrect mixHash: %v != %v", ec.ID(), newHeader.MixDigest, cl.PrevRandaoHistory[cl.LatestHeadNumber.Uint64()])
		}
		// nonce == 0x0000000000000000
		if newHeader.Nonce != (types.BlockNonce{}) {
			cl.Fatalf("CLMocker: Client %v produced a new header with incorrect nonce: %v", ec.ID(), newHeader.Nonce)
		}
		if len(newHeader.Extra) > 32 {
			cl.Fatalf("CLMocker: Client %v produced a new header with incorrect extraData (len > 32): %v", ec.ID(), newHeader.Extra)
		}
		cl.LatestHeader = newHeader
	}
	if cl.LatestHeader == nil {
		cl.Fatalf("CLMocker: None of the clients accepted the newly constructed payload")
	}
	cl.HeaderHistory[cl.LatestHeadNumber.Uint64()] = cl.LatestHeader
}

// Loop produce PoS blocks by using the Engine API
func (cl *CLMocker) ProduceBlocks(blockCount int, callbacks BlockProcessCallbacks) {
	// Produce requested amount of blocks
	for i := 0; i < blockCount; i++ {
		cl.ProduceSingleBlock(callbacks)
	}
}

type ExecutePayloadOutcome struct {
	ExecutePayloadResponse *api.PayloadStatusV1
	Container              string
	Error                  error
}

func (cl *CLMocker) BroadcastNewPayload(payload *typ.ExecutableData, versionedHashes *[]common.Hash, beaconRoot *common.Hash, version int) []ExecutePayloadOutcome {
	responses := make([]ExecutePayloadOutcome, len(cl.EngineClients))
	for i, ec := range cl.EngineClients {
		responses[i].Container = ec.ID()
		ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
		defer cancel()
		execPayloadResp, err := ec.NewPayload(ctx, version, payload, versionedHashes, beaconRoot)
		if err != nil {
			cl.Errorf("CLMocker: Could not ExecutePayloadV1: %v", err)
			responses[i].Error = err
		} else {
			cl.Logf("CLMocker: Executed payload on %s: %v", ec.ID(), execPayloadResp)
			responses[i].ExecutePayloadResponse = &execPayloadResp
		}
	}
	return responses
}

type ForkChoiceOutcome struct {
	ForkchoiceResponse *api.ForkChoiceResponse
	Container          string
	Error              error
}

func (cl *CLMocker) BroadcastForkchoiceUpdated(fcstate *api.ForkchoiceStateV1, payloadAttr *typ.PayloadAttributes, version int) []ForkChoiceOutcome {
	responses := make([]ForkChoiceOutcome, len(cl.EngineClients))
	for i, ec := range cl.EngineClients {
		responses[i].Container = ec.ID()
		newPayloadStatus := ec.LatestNewPayloadResponse()
		if cl.IsOptimisticallySyncing() || newPayloadStatus.Status == "VALID" {
			ctx, cancel := context.WithTimeout(cl.TestContext, globals.RPCTimeout)
			defer cancel()
			fcUpdatedResp, err := ec.ForkchoiceUpdated(ctx, version, fcstate, payloadAttr)
			if err != nil {
				cl.Errorf("CLMocker: Could not ForkchoiceUpdatedV1: %v", err)
				responses[i].Error = err
			} else {
				responses[i].ForkchoiceResponse = &fcUpdatedResp
			}
		} else {
			responses[i].Error = fmt.Errorf("Cannot optimistically import block")
		}
	}
	return responses
}
