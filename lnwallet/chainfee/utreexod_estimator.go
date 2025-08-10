package chainfee

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/rpcclient"
)

const (
	// defaultFallbackFeeRate is the default fallback fee rate in sat/kw.
	defaultFallbackFeeRate SatPerKWeight = 12500 // 50 sat/vbyte

	// utreexodUpdateInterval is how often we update fee estimates.
	utreexodUpdateInterval = 10 * time.Minute
)

// UtreexodEstimator is a fee estimator that queries utreexod for fee estimates.
type UtreexodEstimator struct {
	// client is the RPC client to utreexod.
	client *rpcclient.Client

	// fallbackFeeRate is the fee rate to use if we can't get an estimate.
	fallbackFeeRate SatPerKWeight

	// feeRates caches fee rates for different confirmation targets.
	feeRates map[uint32]SatPerKWeight

	// lastUpdate tracks when we last updated fees.
	lastUpdate time.Time

	// updateInterval is how often to update fee estimates.
	updateInterval time.Duration

	// mtx protects the fee rate cache.
	mtx sync.RWMutex

	quit chan struct{}
}

// NewUtreexodEstimator creates a new UtreexodEstimator.
func NewUtreexodEstimator(client *rpcclient.Client,
	fallbackFeeRate SatPerKWeight) (*UtreexodEstimator, error) {

	return &UtreexodEstimator{
		client:          client,
		fallbackFeeRate: fallbackFeeRate,
		feeRates:        make(map[uint32]SatPerKWeight),
		updateInterval:  utreexodUpdateInterval,
		quit:            make(chan struct{}),
	}, nil
}

// Start begins the fee estimator.
func (u *UtreexodEstimator) Start() error {
	// Get initial fee estimates.
	if err := u.updateFeeEstimates(); err != nil {
		log.Warnf("Unable to get initial fee estimates: %v", err)
	}

	// Start update goroutine.
	go u.feeUpdateLoop()

	return nil
}

// Stop stops the fee estimator.
func (u *UtreexodEstimator) Stop() error {
	close(u.quit)
	return nil
}

// EstimateFeePerKW estimates the fee per kiloweight for a given conf target.
func (u *UtreexodEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	u.mtx.RLock()
	defer u.mtx.RUnlock()

	// Check if we have a cached rate.
	if feeRate, ok := u.feeRates[numBlocks]; ok {
		return feeRate, nil
	}

	// Check nearby targets.
	for delta := uint32(1); delta <= 5; delta++ {
		if numBlocks > delta {
			if feeRate, ok := u.feeRates[numBlocks-delta]; ok {
				return feeRate, nil
			}
		}
		if feeRate, ok := u.feeRates[numBlocks+delta]; ok {
			return feeRate, nil
		}
	}

	// Return fallback rate.
	return u.fallbackFeeRate, nil
}

// RelayFeePerKW returns the minimum relay fee rate.
func (u *UtreexodEstimator) RelayFeePerKW() SatPerKWeight {
	// Try to get network info for relay fee.
	info, err := u.client.GetNetworkInfo()
	if err == nil && info != nil && info.RelayFee > 0 {
		// Convert from BTC/KB to sat/kw
		// 1 BTC = 100,000,000 sat
		// 1 KB = 1000 bytes = 4000 weight units
		// So 1 BTC/KB = 100,000,000 / 4000 = 25,000 sat/kw
		return SatPerKWeight(info.RelayFee * 25000)
	}

	// Default to 1 sat/vbyte = 250 sat/kw
	return SatPerKWeight(250)
}

// feeUpdateLoop periodically updates fee estimates.
func (u *UtreexodEstimator) feeUpdateLoop() {
	ticker := time.NewTicker(u.updateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := u.updateFeeEstimates(); err != nil {
				log.Warnf("Unable to update fee estimates: %v", err)
			}
		case <-u.quit:
			return
		}
	}
}

// updateFeeEstimates queries utreexod for current fee estimates.
func (u *UtreexodEstimator) updateFeeEstimates() error {
	// Common confirmation targets.
	targets := []uint32{1, 2, 3, 6, 12, 24, 144, 504, 1008}

	newRates := make(map[uint32]SatPerKWeight)

	for _, target := range targets {
		// Try estimatesmartfee first.
		estimate, err := u.estimateSmartFee(int64(target))
		if err != nil {
			// If that fails, try estimatefee.
			estimate, err = u.estimateFee(int64(target))
			if err != nil {
				continue
			}
		}

		// Convert from BTC/KB to sat/kw.
		if estimate > 0 {
			// 1 BTC = 100,000,000 sat
			// 1 KB = 1000 bytes = 4000 weight units
			// So 1 BTC/KB = 100,000,000 / 4000 = 25,000 sat/kw
			feeRate := SatPerKWeight(estimate * 25000)
			
			// Sanity check - don't accept extremely high fees.
			if feeRate > 10000000 { // 40,000 sat/vbyte
				continue
			}

			newRates[target] = feeRate
		}
	}

	// Update our cache if we got any estimates.
	if len(newRates) > 0 {
		u.mtx.Lock()
		u.feeRates = newRates
		u.lastUpdate = time.Now()
		u.mtx.Unlock()
	}

	return nil
}

// estimateSmartFee tries to call estimatesmartfee.
func (u *UtreexodEstimator) estimateSmartFee(confTarget int64) (float64, error) {
	// Send the estimatesmartfee command.
	result, err := u.client.RawRequest("estimatesmartfee", []json.RawMessage{
		json.RawMessage(fmt.Sprintf("%d", confTarget)),
	})
	if err != nil {
		return 0, err
	}

	// Parse the result.
	var feeResult struct {
		FeeRate float64  `json:"feerate"`
		Errors  []string `json:"errors"`
		Blocks  int64    `json:"blocks"`
	}

	if err := json.Unmarshal(result, &feeResult); err != nil {
		return 0, err
	}

	// Check for errors.
	if len(feeResult.Errors) > 0 {
		return 0, fmt.Errorf("fee estimation errors: %v", feeResult.Errors)
	}

	// Check if we got a valid fee rate.
	if feeResult.FeeRate <= 0 {
		return 0, fmt.Errorf("invalid fee rate: %v", feeResult.FeeRate)
	}

	return feeResult.FeeRate, nil
}

// estimateFee tries to call the older estimatefee RPC.
func (u *UtreexodEstimator) estimateFee(numBlocks int64) (float64, error) {
	// Send the estimatefee command.
	result, err := u.client.RawRequest("estimatefee", []json.RawMessage{
		json.RawMessage(fmt.Sprintf("%d", numBlocks)),
	})
	if err != nil {
		return 0, err
	}

	// Parse the result.
	var feeRate float64
	if err := json.Unmarshal(result, &feeRate); err != nil {
		return 0, err
	}

	// Check if the fee rate is valid.
	if feeRate < 0 {
		return 0, fmt.Errorf("fee estimation not available")
	}

	return feeRate, nil
}