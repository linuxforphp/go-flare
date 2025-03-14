// Copyright (C) 2019-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// TODO: consider moving the network implementation to a separate package

package builder

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/ava-labs/avalanchego/cache"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/vms/platformvm/message"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
)

const (
	// We allow [recentCacheSize] to be fairly large because we only store hashes
	// in the cache, not entire transactions.
	recentCacheSize = 512
)

var _ Network = &network{}

type Network interface {
	common.AppHandler

	// GossipTx gossips the transaction to some of the connected peers
	GossipTx(tx *txs.Tx) error
}

type network struct {
	ctx        *snow.Context
	blkBuilder Builder

	// gossip related attributes
	appSender common.AppSender
	recentTxs *cache.LRU
}

func NewNetwork(
	ctx *snow.Context,
	blkBuilder *builder,
	appSender common.AppSender,
) Network {
	return &network{
		ctx:        ctx,
		blkBuilder: blkBuilder,
		appSender:  appSender,
		recentTxs:  &cache.LRU{Size: recentCacheSize},
	}
}

func (n *network) AppRequestFailed(nodeID ids.NodeID, requestID uint32) error {
	// This VM currently only supports gossiping of txs, so there are no
	// requests.
	return nil
}

func (n *network) AppRequest(nodeID ids.NodeID, requestID uint32, deadline time.Time, msgBytes []byte) error {
	// This VM currently only supports gossiping of txs, so there are no
	// requests.
	return nil
}

func (n *network) AppResponse(nodeID ids.NodeID, requestID uint32, msgBytes []byte) error {
	// This VM currently only supports gossiping of txs, so there are no
	// requests.
	return nil
}

func (n *network) AppGossip(nodeID ids.NodeID, msgBytes []byte) error {
	n.ctx.Log.Debug("called AppGossip message handler",
		zap.Stringer("nodeID", nodeID),
		zap.Int("messageLen", len(msgBytes)),
	)

	msgIntf, err := message.Parse(msgBytes)
	if err != nil {
		n.ctx.Log.Debug("dropping AppGossip message",
			zap.String("reason", "failed to parse message"),
		)
		return nil
	}

	msg, ok := msgIntf.(*message.Tx)
	if !ok {
		n.ctx.Log.Debug("dropping unexpected message",
			zap.Stringer("nodeID", nodeID),
		)
		return nil
	}

	tx, err := txs.Parse(txs.Codec, msg.Tx)
	if err != nil {
		n.ctx.Log.Verbo("received invalid tx",
			zap.Stringer("nodeID", nodeID),
			zap.Binary("tx", msg.Tx),
			zap.Error(err),
		)
		return nil
	}

	txID := tx.ID()

	// We need to grab the context lock here to avoid racy behavior with
	// transaction verification + mempool modifications.
	n.ctx.Lock.Lock()
	defer n.ctx.Lock.Unlock()

	if _, dropped := n.blkBuilder.GetDropReason(txID); dropped {
		// If the tx is being dropped - just ignore it
		return nil
	}

	// add to mempool
	if err = n.blkBuilder.AddUnverifiedTx(tx); err != nil {
		n.ctx.Log.Debug("tx failed verification",
			zap.Stringer("nodeID", nodeID),
			zap.Error(err),
		)
	}
	return nil
}

func (n *network) GossipTx(tx *txs.Tx) error {
	txID := tx.ID()
	// Don't gossip a transaction if it has been recently gossiped.
	if _, has := n.recentTxs.Get(txID); has {
		return nil
	}
	n.recentTxs.Put(txID, nil)

	n.ctx.Log.Debug("gossiping tx",
		zap.Stringer("txID", txID),
	)

	msg := &message.Tx{Tx: tx.Bytes()}
	msgBytes, err := message.Build(msg)
	if err != nil {
		return fmt.Errorf("GossipTx: failed to build Tx message: %w", err)
	}
	return n.appSender.SendAppGossip(msgBytes)
}
