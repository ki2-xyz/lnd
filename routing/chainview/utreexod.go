package chainview

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/blockcache"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
)

// UtreexodFilteredChainView is an implementation of the FilteredChainView
// interface which uses an utreexod RPC client to get filtered blocks.
type UtreexodFilteredChainView struct {
	// bestBlock is the best known block.
	bestBlock FilteredBlock

	// utreexodConn is the RPC client we'll use to query utreexod.
	utreexodConn *rpcclient.Client

	// blockCache is the cache that we'll use to fetch blocks.
	blockCache *blockcache.BlockCache

	// blockEventQueue is the channel used to send block updates.
	blockEventQueue chan *FilteredBlock

	// filterUpdates is a channel in which updates to the filter are sent.
	filterUpdates chan utreexodFilterUpdate

	// filterBlockReqs is a channel in which requests to filter blocks are sent.
	filterBlockReqs chan *utreexodFilterBlockReq

	// chainFilter is the current chain filter.
	chainFilter map[wire.OutPoint][]byte

	// filterMtx protects the chainFilter.
	filterMtx sync.RWMutex

	started int32
	stopped int32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// utreexodFilterUpdate represents an update to the filter.
type utreexodFilterUpdate struct {
	newUtxos     []wire.OutPoint
	updateHeight uint32
	done         chan error
}

// utreexodFilterBlockReq represents a request to filter a block.
type utreexodFilterBlockReq struct {
	blockHash *chainhash.Hash
	resp      chan *FilteredBlock
	err       chan error
}

// NewUtreexodFilteredChainView creates a new instance of a FilteredChainView
// from an active utreexod RPC client.
func NewUtreexodFilteredChainView(utreexodConn *rpcclient.Client,
	blockCache *blockcache.BlockCache) (*UtreexodFilteredChainView, error) {

	return &UtreexodFilteredChainView{
		utreexodConn:     utreexodConn,
		blockCache:       blockCache,
		blockEventQueue:  make(chan *FilteredBlock),
		filterUpdates:    make(chan utreexodFilterUpdate),
		filterBlockReqs:  make(chan *utreexodFilterBlockReq),
		chainFilter:      make(map[wire.OutPoint][]byte),
		quit:            make(chan struct{}),
	}, nil
}

// Start starts the UtreexodFilteredChainView.
func (u *UtreexodFilteredChainView) Start() error {
	if !atomic.CompareAndSwapInt32(&u.started, 0, 1) {
		return nil
	}

	log.Infof("Starting utreexod filtered chain view")

	// Get the best block to use as our starting point.
	blockHash, blockHeight, err := u.utreexodConn.GetBestBlock()
	if err != nil {
		return fmt.Errorf("unable to get best block: %v", err)
	}

	u.bestBlock = FilteredBlock{
		Hash:   *blockHash,
		Height: uint32(blockHeight),
	}

	u.wg.Add(2)
	go u.chainFilterer()
	go u.blockPoller()

	return nil
}

// Stop stops the UtreexodFilteredChainView.
func (u *UtreexodFilteredChainView) Stop() error {
	if !atomic.CompareAndSwapInt32(&u.stopped, 0, 1) {
		return nil
	}

	log.Infof("Stopping utreexod filtered chain view")

	close(u.quit)
	u.wg.Wait()

	return nil
}

// blockPoller polls for new blocks and sends them to the block event queue.
func (u *UtreexodFilteredChainView) blockPoller() {
	defer u.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Get the best block.
			_, blockHeight, err := u.utreexodConn.GetBestBlock()
			if err != nil {
				log.Errorf("Unable to get best block: %v", err)
				continue
			}

			// If we've already seen this block, skip it.
			if uint32(blockHeight) <= u.bestBlock.Height {
				continue
			}

			// Process new blocks.
			for height := u.bestBlock.Height + 1; height <= uint32(blockHeight); height++ {
				hash, err := u.utreexodConn.GetBlockHash(int64(height))
				if err != nil {
					log.Errorf("Unable to get block hash: %v", err)
					continue
				}

				// Filter the block.
				filteredBlock, err := u.filterBlock(hash)
				if err != nil {
					log.Errorf("Unable to filter block: %v", err)
					continue
				}

				// Update our best block.
				u.bestBlock = *filteredBlock

				// Send the filtered block.
				select {
				case u.blockEventQueue <- filteredBlock:
				case <-u.quit:
					return
				}
			}

		case <-u.quit:
			return
		}
	}
}

// chainFilterer handles filter updates and block filter requests.
func (u *UtreexodFilteredChainView) chainFilterer() {
	defer u.wg.Done()

	for {
		select {
		case update := <-u.filterUpdates:
			// Update our filter.
			u.filterMtx.Lock()
			for _, utxo := range update.newUtxos {
				u.chainFilter[utxo] = nil
			}
			u.filterMtx.Unlock()

			// Signal completion.
			if update.done != nil {
				update.done <- nil
			}

		case req := <-u.filterBlockReqs:
			// Filter the requested block.
			filteredBlock, err := u.filterBlock(req.blockHash)
			if err != nil {
				req.err <- err
				continue
			}
			req.resp <- filteredBlock

		case <-u.quit:
			return
		}
	}
}

// filterBlock filters a block for relevant transactions.
func (u *UtreexodFilteredChainView) filterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	// Get the block.
	block, err := u.blockCache.GetBlock(blockHash, u.utreexodConn.GetBlock)
	if err != nil {
		return nil, err
	}

	// Get the block height.
	info, err := u.utreexodConn.GetBlockHeaderVerbose(blockHash)
	if err != nil {
		return nil, err
	}

	// Check which transactions are relevant.
	u.filterMtx.RLock()
	defer u.filterMtx.RUnlock()

	relevantTxs := make([]*wire.MsgTx, 0)
	for _, tx := range block.Transactions {
		// Check if any of the inputs spend our watched outputs.
		for _, txIn := range tx.TxIn {
			if _, ok := u.chainFilter[txIn.PreviousOutPoint]; ok {
				relevantTxs = append(relevantTxs, tx)
				break
			}
		}

		// Check if any of the outputs are ones we're watching.
		txHash := tx.TxHash()
		for i := range tx.TxOut {
			op := wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			}
			if _, ok := u.chainFilter[op]; ok {
				relevantTxs = append(relevantTxs, tx)
				break
			}
		}
	}

	return &FilteredBlock{
		Hash:         *blockHash,
		Height:       uint32(info.Height),
		Transactions: relevantTxs,
	}, nil
}

// UpdateFilter updates the filter with new outputs to watch.
func (u *UtreexodFilteredChainView) UpdateFilter(ops []graphdb.EdgePoint,
	updateHeight uint32) error {

	// Convert EdgePoints to OutPoints.
	outpoints := make([]wire.OutPoint, len(ops))
	for i, op := range ops {
		outpoints[i] = op.OutPoint
	}

	update := utreexodFilterUpdate{
		newUtxos:     outpoints,
		updateHeight: updateHeight,
		done:         make(chan error),
	}

	select {
	case u.filterUpdates <- update:
	case <-u.quit:
		return fmt.Errorf("chain view shutting down")
	}

	select {
	case err := <-update.done:
		return err
	case <-u.quit:
		return fmt.Errorf("chain view shutting down")
	}
}

// FilterBlock filters a block for transactions we're interested in.
func (u *UtreexodFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	req := &utreexodFilterBlockReq{
		blockHash: blockHash,
		resp:      make(chan *FilteredBlock),
		err:       make(chan error),
	}

	select {
	case u.filterBlockReqs <- req:
	case <-u.quit:
		return nil, fmt.Errorf("chain view shutting down")
	}

	select {
	case filteredBlock := <-req.resp:
		return filteredBlock, nil
	case err := <-req.err:
		return nil, err
	case <-u.quit:
		return nil, fmt.Errorf("chain view shutting down")
	}
}

// FilteredBlocks returns a channel that block updates are sent on.
func (u *UtreexodFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return u.blockEventQueue
}

// DisconnectedBlocks returns a channel that disconnected block updates are sent on.
func (u *UtreexodFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	// TODO: Implement reorg handling
	return make(chan *FilteredBlock)
}