package chainreg

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

// mustMarshalJSON marshals v to JSON or panics.
func mustMarshalJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// UtreexodChainSource is a wrapper around the utreexod RPC client that
// implements the chain.Interface.
type UtreexodChainSource struct {
	client *rpcclient.Client

	// notificationQueue holds notifications to be processed.
	notificationQueue chan interface{}

	// notifyBlocks indicates if we're registered for block notifications.
	notifyBlocks bool

	// notifyTxs tracks addresses we're watching for transactions.
	notifyTxs map[string]btcutil.Address

	// bestBlock tracks the current best block.
	bestBlock struct {
		sync.RWMutex
		hash   *chainhash.Hash
		height int32
	}

	started bool
	stopped bool
	quit    chan struct{}
	wg      sync.WaitGroup
	mtx     sync.RWMutex
}

// NewUtreexodChainSource creates a new UtreexodChainSource.
func NewUtreexodChainSource(client *rpcclient.Client) *UtreexodChainSource {
	return &UtreexodChainSource{
		client:            client,
		notificationQueue: make(chan interface{}, 100),
		notifyTxs:         make(map[string]btcutil.Address),
		quit:              make(chan struct{}),
	}
}

// Start begins the background goroutines for the chain source.
func (u *UtreexodChainSource) Start() error {
	u.mtx.Lock()
	defer u.mtx.Unlock()

	if u.started {
		return nil
	}
	u.started = true

	// Get initial best block.
	hash, height, err := u.client.GetBestBlock()
	if err != nil {
		return err
	}

	u.bestBlock.Lock()
	u.bestBlock.hash = hash
	u.bestBlock.height = height
	u.bestBlock.Unlock()

	// Start polling goroutines.
	u.wg.Add(2)
	go u.blockPoller()
	go u.txPoller()

	return nil
}

// Stop halts the background goroutines.
func (u *UtreexodChainSource) Stop() {
	u.mtx.Lock()
	defer u.mtx.Unlock()

	if u.stopped {
		return
	}
	u.stopped = true

	close(u.quit)
	u.wg.Wait()
}

// WaitForShutdown blocks until the chain source has stopped.
func (u *UtreexodChainSource) WaitForShutdown() {
	u.wg.Wait()
}

// GetBestBlock returns the best block hash and height.
func (u *UtreexodChainSource) GetBestBlock() (*chainhash.Hash, int32, error) {
	return u.client.GetBestBlock()
}

// GetBlock fetches a block by hash.
func (u *UtreexodChainSource) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	return u.client.GetBlock(hash)
}

// GetBlockHash fetches a block hash by height.
func (u *UtreexodChainSource) GetBlockHash(height int64) (*chainhash.Hash, error) {
	return u.client.GetBlockHash(height)
}

// GetBlockHeader fetches a block header by hash.
func (u *UtreexodChainSource) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	return u.client.GetBlockHeader(hash)
}

// IsCurrent returns whether the chain is synced.
func (u *UtreexodChainSource) IsCurrent() bool {
	info, err := u.client.GetBlockChainInfo()
	if err != nil {
		return false
	}

	// Consider synced if we have all headers.
	return info.Headers == info.Blocks
}

// BlockStamp returns the current block stamp.
func (u *UtreexodChainSource) BlockStamp() (*waddrmgr.BlockStamp, error) {
	hash, height, err := u.GetBestBlock()
	if err != nil {
		return nil, err
	}

	header, err := u.GetBlockHeader(hash)
	if err != nil {
		return nil, err
	}

	return &waddrmgr.BlockStamp{
		Height:    height,
		Hash:      *hash,
		Timestamp: header.Timestamp,
	}, nil
}

// SendRawTransaction broadcasts a transaction.
func (u *UtreexodChainSource) SendRawTransaction(tx *wire.MsgTx, allowHighFees bool) (*chainhash.Hash, error) {
	return u.client.SendRawTransaction(tx, allowHighFees)
}

// Rescan rescans the blockchain for addresses.
func (u *UtreexodChainSource) Rescan(startHash *chainhash.Hash, addrs []btcutil.Address,
	outPoints map[wire.OutPoint]btcutil.Address) error {
	
	// Utreexod doesn't support rescan, so we'll need to implement
	// our own scanning logic.
	// For now, return nil to allow things to work.
	return nil
}

// NotifyReceived registers addresses for transaction notifications.
func (u *UtreexodChainSource) NotifyReceived(addrs []btcutil.Address) error {
	u.mtx.Lock()
	defer u.mtx.Unlock()

	for _, addr := range addrs {
		u.notifyTxs[addr.String()] = addr
	}

	return nil
}

// NotifyBlocks registers for block notifications.
func (u *UtreexodChainSource) NotifyBlocks() error {
	u.mtx.Lock()
	defer u.mtx.Unlock()

	u.notifyBlocks = true
	return nil
}

// Notifications returns the notification channel.
func (u *UtreexodChainSource) Notifications() <-chan interface{} {
	return u.notificationQueue
}

// BackEnd returns the backend name.
func (u *UtreexodChainSource) BackEnd() string {
	return "utreexod"
}

// TestMempoolAccept tests if transactions would be accepted to mempool.
func (u *UtreexodChainSource) TestMempoolAccept(txns []*wire.MsgTx,
	maxFeeRate float64) ([]*btcjson.TestMempoolAcceptResult, error) {
	
	// Convert transactions to hex.
	hexTxns := make([]string, len(txns))
	for i, tx := range txns {
		var buf bytes.Buffer
		if err := tx.Serialize(&buf); err != nil {
			return nil, err
		}
		hexTxns[i] = hex.EncodeToString(buf.Bytes())
	}

	// Call testmempoolaccept RPC using RawRequest.
	params := []json.RawMessage{
		json.RawMessage(mustMarshalJSON(hexTxns)),
	}
	result, err := u.client.RawRequest("testmempoolaccept", params)
	if err != nil {
		return nil, err
	}

	// Parse the result.
	var results []*btcjson.TestMempoolAcceptResult
	if err := json.Unmarshal(result, &results); err != nil {
		return nil, err
	}

	return results, nil
}

// blockPoller polls for new blocks and sends notifications.
func (u *UtreexodChainSource) blockPoller() {
	defer u.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check for new blocks.
			hash, height, err := u.client.GetBestBlock()
			if err != nil {
				continue
			}

			u.bestBlock.RLock()
			currentHeight := u.bestBlock.height
			u.bestBlock.RUnlock()

			// If we have a new block, send notification.
			if height > currentHeight {
				u.bestBlock.Lock()
				u.bestBlock.hash = hash
				u.bestBlock.height = height
				u.bestBlock.Unlock()

				// For utreexod, we don't send block notifications through
				// the chain source since we have a separate chain notifier.
			}

		case <-u.quit:
			return
		}
	}
}

// txPoller polls for relevant transactions.
func (u *UtreexodChainSource) txPoller() {
	defer u.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// For now, we don't implement mempool polling.
			// This would require checking mempool for transactions
			// that match our watched addresses.

		case <-u.quit:
			return
		}
	}
}

// FilterBlocks filters blocks for relevant transactions.
// This is required by chain.Interface but not implemented for utreexod.
func (u *UtreexodChainSource) FilterBlocks(req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {
	// Utreexod doesn't support FilterBlocks, return empty response.
	return &chain.FilterBlocksResponse{}, nil
}

// MapRPCErr maps an RPC error to a known error type.
func (u *UtreexodChainSource) MapRPCErr(rpcErr error) error {
	// For now, just return the error as-is.
	return rpcErr
}