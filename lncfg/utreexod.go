package lncfg

import "time"

// Utreexod holds the configuration options for the daemon's connection to
// utreexod.
type Utreexod struct {
	Dir                  string        `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	ConfigPath           string        `long:"config" description:"Configuration filepath. If not set, will default to the default filename under 'dir'."`
	RPCCookie            string        `long:"rpccookie" description:"Authentication cookie file for RPC connections. If not set, will default to .cookie under 'dir'."`
	RPCHost              string        `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser              string        `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass              string        `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	WSEndpoint           string        `long:"wsendpoint" description:"The WebSocket endpoint for real-time notifications. If not set, defaults to ws://[rpchost]/ws"`
	EstimateMode         string        `long:"estimatemode" description:"The fee estimate mode. Must be either ECONOMICAL or CONSERVATIVE."`
	PrunedNodeMaxPeers   int           `long:"pruned-node-max-peers" description:"The maximum number of peers lnd will choose from the backend node to retrieve pruned blocks from. This only applies to pruned nodes."`
	BlockPollingInterval time.Duration `long:"blockpollinginterval" description:"The interval that will be used to poll utreexod for new blocks if WebSocket connection fails."`
	TxPollingInterval    time.Duration `long:"txpollinginterval" description:"The interval that will be used to poll utreexod for new tx if WebSocket connection fails."`
}