// Copyright (C) 2019-2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txs

import (
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/components/verify"
	"github.com/ava-labs/avalanchego/vms/platformvm/fx"
	"github.com/ava-labs/avalanchego/vms/platformvm/reward"
	"github.com/ava-labs/avalanchego/vms/platformvm/validator"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
)

var (
	_ ValidatorTx = &AddValidatorTx{}

	errTooManyShares = fmt.Errorf("a staker can only require at most %d shares from delegators", reward.PercentDenominator)
)

// AddValidatorTx is an unsigned addValidatorTx
type AddValidatorTx struct {
	// Metadata, inputs and outputs
	BaseTx `serialize:"true"`
	// Describes the delegatee
	Validator validator.Validator `serialize:"true" json:"validator"`
	// Where to send staked tokens when done validating
	StakeOuts []*avax.TransferableOutput `serialize:"true" json:"stake"`
	// Where to send staking rewards when done validating
	RewardsOwner fx.Owner `serialize:"true" json:"rewardsOwner"`
	// Fee this validator charges delegators as a percentage, times 10,000
	// For example, if this validator has DelegationShares=300,000 then they
	// take 30% of rewards from delegators
	DelegationShares uint32 `serialize:"true" json:"shares"`
}

// InitCtx sets the FxID fields in the inputs and outputs of this
// [AddValidatorTx]. Also sets the [ctx] to the given [vm.ctx] so that
// the addresses can be json marshalled into human readable format
func (tx *AddValidatorTx) InitCtx(ctx *snow.Context) {
	tx.BaseTx.InitCtx(ctx)
	for _, out := range tx.StakeOuts {
		out.FxID = secp256k1fx.ID
		out.InitCtx(ctx)
	}
	tx.RewardsOwner.InitCtx(ctx)
}

func (tx *AddValidatorTx) SubnetID() ids.ID                  { return constants.PrimaryNetworkID }
func (tx *AddValidatorTx) NodeID() ids.NodeID                { return tx.Validator.NodeID }
func (tx *AddValidatorTx) StartTime() time.Time              { return tx.Validator.StartTime() }
func (tx *AddValidatorTx) EndTime() time.Time                { return tx.Validator.EndTime() }
func (tx *AddValidatorTx) Weight() uint64                    { return tx.Validator.Wght }
func (tx *AddValidatorTx) PendingPriority() Priority         { return PrimaryNetworkValidatorPendingPriority }
func (tx *AddValidatorTx) CurrentPriority() Priority         { return PrimaryNetworkValidatorCurrentPriority }
func (tx *AddValidatorTx) Stake() []*avax.TransferableOutput { return tx.StakeOuts }
func (tx *AddValidatorTx) ValidationRewardsOwner() fx.Owner  { return tx.RewardsOwner }
func (tx *AddValidatorTx) DelegationRewardsOwner() fx.Owner  { return tx.RewardsOwner }
func (tx *AddValidatorTx) Shares() uint32                    { return tx.DelegationShares }

// SyntacticVerify returns nil iff [tx] is valid
func (tx *AddValidatorTx) SyntacticVerify(ctx *snow.Context) error {
	switch {
	case tx == nil:
		return ErrNilTx
	case tx.SyntacticallyVerified: // already passed syntactic verification
		return nil
	case tx.DelegationShares > reward.PercentDenominator: // Ensure delegators shares are in the allowed amount
		return errTooManyShares
	}

	if err := tx.BaseTx.SyntacticVerify(ctx); err != nil {
		return fmt.Errorf("failed to verify BaseTx: %w", err)
	}
	if err := verify.All(&tx.Validator, tx.RewardsOwner); err != nil {
		return fmt.Errorf("failed to verify validator or rewards owner: %w", err)
	}

	totalStakeWeight := uint64(0)
	for _, out := range tx.StakeOuts {
		if err := out.Verify(); err != nil {
			return fmt.Errorf("failed to verify output: %w", err)
		}
		newWeight, err := math.Add64(totalStakeWeight, out.Output().Amount())
		if err != nil {
			return err
		}
		totalStakeWeight = newWeight

		assetID := out.AssetID()
		if assetID != ctx.AVAXAssetID {
			return fmt.Errorf("stake output must be AVAX but is %q", assetID)
		}
	}

	switch {
	case !avax.IsSortedTransferableOutputs(tx.StakeOuts, Codec):
		return errOutputsNotSorted
	case totalStakeWeight != tx.Validator.Wght:
		return fmt.Errorf("%w: weight %d != stake %d", errValidatorWeightMismatch, tx.Validator.Wght, totalStakeWeight)
	}

	// cache that this is valid
	tx.SyntacticallyVerified = true
	return nil
}

func (tx *AddValidatorTx) Visit(visitor Visitor) error {
	return visitor.AddValidatorTx(tx)
}
