#!/usr/bin/env bash
# https://blockstream.info/testnet/api/blocks/tip/height
# https://blockstream.info/api/blocks/tip/height
while true; do
  START=$(date +%s)
  H0=$(lncli --macaroonpath=/data3/.lnd/data/chain/bitcoin/testnet/admin.macaroon --tlscertpath=/data3/.lnd/tls.cert getinfo | jq .block_height)
  TIP=$(curl -s https://blockstream.info/testnet/api/blocks/tip/height)

  sleep 30

  H1=$(lncli --macaroonpath=/data3/.lnd/data/chain/bitcoin/testnet/admin.macaroon --tlscertpath=/data3/.lnd/tls.cert getinfo | jq .block_height)
  END=$(date +%s)

  DELTA_H=$((H1 - H0))
  DELTA_T=$((END - START))

  REMAINING=$((TIP - H1))

  if (( DELTA_H > 0 )); then
    SPEED=$(echo "scale=2; $DELTA_H / $DELTA_T" | bc)
    ETA=$(echo "scale=1; $REMAINING / $SPEED / 60" | bc)
    echo "🚀 Sync speed: $SPEED blocks/sec"
    echo "⏱️  ETA: ~$ETA minutes remaining"
  else
    echo "📉 No progress in last $DELTA_T seconds"
  fi

  echo "📦 Local: $H1 / $TIP — Remaining: $REMAINING blocks"
  echo "-----"
done
