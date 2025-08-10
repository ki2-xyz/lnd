package utreexodnotify

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	fnv2 "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "utreexod"

	// defaultBlockPollInterval is the default interval for polling blocks.
	defaultBlockPollInterval = 5 * time.Second
)

// UtreexodNotifier implements a ChainNotifier for utreexod.
type UtreexodNotifier struct {
	started int32
	stopped int32
	
	utreexodConn       *rpcclient.Client
	chainParams        *chaincfg.Params
	
	notificationCancels  chan interface{}
	notificationRegistry chan interface{}
	
	txNotifier *chainntnfs.TxNotifier
	
	blockEpochClients map[uint64]*blockEpochRegistration
	
	bestBlock chainntnfs.BlockEpoch
	
	spendHintCache   chainntnfs.SpendHintCache
	confirmHintCache chainntnfs.ConfirmHintCache
	
	blockCache *blockcache.BlockCache
	
	quit chan struct{}
	wg   sync.WaitGroup
	
	clientMtx sync.Mutex
}

// Ensure UtreexodNotifier implements the ChainNotifier interface.
var _ chainntnfs.ChainNotifier = (*UtreexodNotifier)(nil)

// New returns a new UtreexodNotifier instance.
func New(utreexodConn *rpcclient.Client, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache,
	blockCache *blockcache.BlockCache) *UtreexodNotifier {

	return &UtreexodNotifier{
		utreexodConn:         utreexodConn,
		chainParams:          chainParams,
		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),
		blockEpochClients:    make(map[uint64]*blockEpochRegistration),
		spendHintCache:       spendHintCache,
		confirmHintCache:     confirmHintCache,
		blockCache:           blockCache,
		quit:                 make(chan struct{}),
	}
}

// Start launches all goroutines required for the UtreexodNotifier to carry out
// its duties.
func (u *UtreexodNotifier) Start() error {
	if !atomic.CompareAndSwapInt32(&u.started, 0, 1) {
		return nil
	}

	// Get the current best block.
	blockHash, blockHeight, err := u.utreexodConn.GetBestBlock()
	if err != nil {
		return err
	}

	// Get the block header.
	blockHeader, err := u.utreexodConn.GetBlockHeader(blockHash)
	if err != nil {
		return err
	}

	u.bestBlock = chainntnfs.BlockEpoch{
		Hash:        blockHash,
		Height:      blockHeight,
		BlockHeader: blockHeader,
	}

	u.txNotifier = chainntnfs.NewTxNotifier(
		uint32(u.bestBlock.Height), chainntnfs.ReorgSafetyLimit,
		u.confirmHintCache, u.spendHintCache,
	)

	u.wg.Add(1)
	go u.notificationDispatcher()

	return nil
}

// Started returns true if the notifier has been started.
func (u *UtreexodNotifier) Started() bool {
	return atomic.LoadInt32(&u.started) != 0
}

// Stop halts the UtreexodNotifier.
func (u *UtreexodNotifier) Stop() error {
	if !atomic.CompareAndSwapInt32(&u.stopped, 0, 1) {
		return nil
	}

	close(u.quit)
	u.wg.Wait()

	// Notify all pending clients that we're shutting down.
	u.clientMtx.Lock()
	for _, epochClient := range u.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()
		close(epochClient.epochChan)
	}
	u.clientMtx.Unlock()

	return nil
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (u *UtreexodNotifier) notificationDispatcher() {
	defer u.wg.Done()

	blockTicker := time.NewTicker(defaultBlockPollInterval)
	defer blockTicker.Stop()

	for {
		select {
		case <-u.quit:
			return

		case <-blockTicker.C:
			// Poll for new blocks.
			u.pollForBlocks()

		case registerMsg := <-u.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *chainntnfs.HistoricalConfDispatch:
				// We'll start by looking up the transaction to
				// determine its confirmation status.
				_, err := u.confDetailsManually(msg.ConfRequest.TxID,
					msg.ConfRequest.PkScript.Script(), msg.StartHeight)
				if err != nil {
					log.Errorf("unable to get conf details: %v", err)
				}

			case *blockEpochRegistration:
				u.clientMtx.Lock()
				u.blockEpochClients[msg.id] = msg
				u.clientMtx.Unlock()

			case *chainntnfs.HistoricalSpendDispatch:
				// We'll query the backend to see if the output has been
				// spent, if it has, we'll dispatch immediately.
				_, err := u.spendDetailsManually(msg.SpendRequest.OutPoint,
					msg.SpendRequest.PkScript.Script(), msg.StartHeight)
				if err != nil {
					log.Errorf("unable to get spend details: %v", err)
				}
			}

		case item := <-u.notificationCancels:
			switch msg := item.(type) {
			case *epochCancel:
				u.clientMtx.Lock()
				delete(u.blockEpochClients, msg.epochID)
				u.clientMtx.Unlock()
			}
		}
	}
}

// pollForBlocks polls the backend for new blocks.
func (u *UtreexodNotifier) pollForBlocks() {
	// Get the current best block.
	_, blockHeight, err := u.utreexodConn.GetBestBlock()
	if err != nil {
		log.Errorf("Unable to get best block: %v", err)
		return
	}

	// If we're already at this height, return early.
	if blockHeight <= u.bestBlock.Height {
		return
	}

	// Process any new blocks.
	for height := u.bestBlock.Height + 1; height <= blockHeight; height++ {
		hash, err := u.utreexodConn.GetBlockHash(int64(height))
		if err != nil {
			log.Errorf("Unable to get block hash for height %d: %v",
				height, err)
			continue
		}

		// Get the block header first (much smaller - 80 bytes)
		header, err := u.utreexodConn.GetBlockHeader(hash)
		if err != nil {
			log.Errorf("Unable to get block header: %v", err)
			continue
		}

		// Update our best block.
		u.bestBlock = chainntnfs.BlockEpoch{
			Hash:        hash,
			Height:      height,
			BlockHeader: header,
		}

		// Notify block epoch clients.
		u.notifyBlockEpochClients(&u.bestBlock)

		// For now, skip downloading full blocks during initial sync
		// This is a temporary optimization - in production, we'd check
		// if we have any subscriptions for this height range
		currentTime := time.Now()
		blockTime := header.Timestamp
		timeDiff := currentTime.Sub(blockTime)
		
		// Only download full blocks if they're recent (within 24 hours)
		// or if we're close to the tip
		if timeDiff < 24*time.Hour {
			block, err := u.utreexodConn.GetBlock(hash)
			if err != nil {
				log.Errorf("Unable to get block %v: %v", hash, err)
				continue
			}
			
			// Handle transaction notifications.
			btcBlock := btcutil.NewBlock(block)
			btcBlock.SetHeight(int32(height))
			err = u.txNotifier.ConnectTip(btcBlock, uint32(height))
			if err != nil {
				log.Errorf("Unable to connect tip: %v", err)
			}
		} else {
			// For old blocks, just create a minimal block with header
			// This allows the sync to proceed without downloading full blocks
			block := &wire.MsgBlock{
				Header:       *header,
				Transactions: []*wire.MsgTx{},
			}
			btcBlock := btcutil.NewBlock(block)
			btcBlock.SetHeight(int32(height))
			err = u.txNotifier.ConnectTip(btcBlock, uint32(height))
			if err != nil {
				log.Debugf("Unable to connect tip with header-only block: %v", err)
			}
		}
	}
}

// notifyBlockEpochClients notifies all registered block epoch clients of the
// block.
func (u *UtreexodNotifier) notifyBlockEpochClients(epoch *chainntnfs.BlockEpoch) {
	u.clientMtx.Lock()
	defer u.clientMtx.Unlock()

	for _, epochClient := range u.blockEpochClients {
		select {
		case epochClient.epochChan <- epoch:
		case <-epochClient.cancelChan:
		case <-u.quit:
			return
		}
	}
}

// RegisterConfirmationsNtfn registers an intent to be notified once txid reaches
// numConfs confirmations.
func (u *UtreexodNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	// Register with the TxNotifier.
	ntfn, err := u.txNotifier.RegisterConf(
		txid, pkScript, numConfs, heightHint, opts...,
	)
	if err != nil {
		return nil, err
	}

	// We'll launch a goroutine to handle any pre-existing confirmation.
	go func() {
		// If we have a historical dispatch request, check for confirmation.
		if ntfn.HistoricalDispatch != nil {
			confDetails, err := u.confDetailsManually(
				ntfn.HistoricalDispatch.ConfRequest.TxID,
				ntfn.HistoricalDispatch.ConfRequest.PkScript.Script(),
				ntfn.HistoricalDispatch.StartHeight,
			)
			if err != nil {
				log.Errorf("Unable to get confirmation details: %v", err)
				return
			}

			// If we found the transaction, update the notifier.
			if confDetails != nil {
				err := u.txNotifier.UpdateConfDetails(
					ntfn.HistoricalDispatch.ConfRequest, confDetails,
				)
				if err != nil {
					log.Errorf("Unable to update conf details: %v", err)
				}
			}
		}
	}()

	return ntfn.Event, nil
}

// RegisterSpendNtfn registers an intent to be notified once the target outpoint
// is successfully spent within a transaction.
func (u *UtreexodNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// For now, we don't support spend notifications in the simplified version.
	return nil, errors.New("spend notifications not yet supported")
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications for each new block connected to the main
// chain.
func (u *UtreexodNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	reg := &blockEpochRegistration{
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochQueue: queue.NewConcurrentQueue(20),
		bestBlock:  bestBlock,
		id:         atomic.AddUint64(&epochClientCounter, 1),
	}
	reg.epochQueue.Start()

	// Before we send the request to the main goroutine, we'll launch a new
	// goroutine to proxy items added to our queue to the client itself.
	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()

		for {
			select {
			case <-reg.cancelChan:
				return
			case <-u.quit:
				return
			case item := <-reg.epochQueue.ChanOut():
				blockEpoch := item.(*chainntnfs.BlockEpoch)
				select {
				case reg.epochChan <- blockEpoch:
				case <-reg.cancelChan:
					return
				case <-u.quit:
					return
				}
			}
		}
	}()

	select {
	case <-u.quit:
		return nil, errors.New("notifier shutting down")
	case u.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent{
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.id,
				}

				// Submit cancellation to notification dispatcher.
				select {
				case u.notificationCancels <- cancel:
				case <-u.quit:
				}

				// Wait for the cancellation to be processed.
				select {
				case <-reg.cancelChan:
				case <-u.quit:
				}

				reg.wg.Wait()
			},
		}, nil
	}
}

// confDetailsManually looks up whether a transaction/output script has been
// confirmed.
func (u *UtreexodNotifier) confDetailsManually(txid chainhash.Hash,
	pkScript []byte, heightHint uint32) (*chainntnfs.TxConfirmation, error) {

	// If we have a txid, look it up.
	if txid != chainntnfs.ZeroHash {
		txConf, err := u.fetchConfDetails(txid)
		if err != nil {
			return nil, err
		}

		return txConf, nil
	}

	// Otherwise, we don't support script-based lookups yet.
	return nil, nil
}

// fetchConfDetails looks up the confirmation details for a transaction.
func (u *UtreexodNotifier) fetchConfDetails(txid chainhash.Hash,
) (*chainntnfs.TxConfirmation, error) {

	// First, check if the transaction is in the mempool.
	txInfo, err := u.utreexodConn.GetRawTransactionVerbose(&txid)
	if err != nil {
		// Transaction might not exist.
		return nil, nil
	}

	// If the transaction is unconfirmed, return nil.
	if txInfo.Confirmations <= 0 {
		return nil, nil
	}

	// Get the block hash.
	blockHash, err := chainhash.NewHashFromStr(txInfo.BlockHash)
	if err != nil {
		return nil, err
	}

	// Get the block to find the transaction index.
	block, err := u.utreexodConn.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// Find the transaction index.
	txIndex := -1
	for i, tx := range block.Transactions {
		if tx.TxHash() == txid {
			txIndex = i
			break
		}
	}

	if txIndex == -1 {
		return nil, fmt.Errorf("transaction not found in block")
	}

	// Get the block height.
	_, err = u.utreexodConn.GetBlockHeader(blockHash)
	if err != nil {
		return nil, err
	}

	blockHeight, err := u.utreexodConn.GetBlockCount()
	if err != nil {
		return nil, err
	}

	return &chainntnfs.TxConfirmation{
		Tx:          block.Transactions[txIndex],
		BlockHash:   blockHash,
		BlockHeight: uint32(blockHeight - int64(txInfo.Confirmations) + 1),
		TxIndex:     uint32(txIndex),
		Block:       block,
	}, nil
}

// spendDetailsManually looks up whether an outpoint has been spent.
func (u *UtreexodNotifier) spendDetailsManually(outpoint wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendDetail, error) {
	// Not implemented in simplified version.
	return nil, nil
}

// CompleteRegistration performs any final registration setup.
func (u *UtreexodNotifier) CompleteRegistration(confRequest chainntnfs.ConfRequest,
	confChan chan *chainntnfs.TxConfirmation) error {
	// Not needed for simplified version.
	return nil
}

// CancelMempoolSpendEvent cancels a mempool spend event.
func (u *UtreexodNotifier) CancelMempoolSpendEvent(
	sub *chainntnfs.MempoolSpendEvent) {
	// Not implemented in simplified version.
}

// LookupInputMempoolSpend looks up a spend in the mempool.
func (u *UtreexodNotifier) LookupInputMempoolSpend(
	op wire.OutPoint) fnv2.Option[wire.MsgTx] {
	// Not implemented in simplified version.
	return fnv2.None[wire.MsgTx]()
}

// SubscribeMempoolSpent subscribes to mempool spent notifications.
func (u *UtreexodNotifier) SubscribeMempoolSpent(
	outpoint wire.OutPoint) (*chainntnfs.MempoolSpendEvent, error) {
	// Not implemented in simplified version.
	return nil, errors.New("mempool spend subscriptions not supported")
}

// blockEpochRegistration represents a client's intent to receive block epoch
// notifications.
type blockEpochRegistration struct {
	epochChan  chan *chainntnfs.BlockEpoch
	cancelChan chan struct{}
	epochQueue *queue.ConcurrentQueue
	bestBlock  *chainntnfs.BlockEpoch
	errorChan  chan error
	id         uint64
	wg         sync.WaitGroup
}

// epochCancel represents a client's intent to cancel their block epoch
// notification.
type epochCancel struct {
	epochID uint64
}

// epochClientCounter is a global counter for generating unique epoch client IDs.
var epochClientCounter uint64