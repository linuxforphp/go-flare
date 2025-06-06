// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package tracers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/ava-labs/coreth/consensus"
	"github.com/ava-labs/coreth/consensus/dummy"
	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/core/rawdb"
	"github.com/ava-labs/coreth/core/state"
	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/core/vm"
	"github.com/ava-labs/coreth/eth/tracers/logger"
	"github.com/ava-labs/coreth/ethdb"
	"github.com/ava-labs/coreth/internal/ethapi"
	"github.com/ava-labs/coreth/params"
	"github.com/ava-labs/coreth/rpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	errStateNotFound       = errors.New("state not found")
	errBlockNotFound       = errors.New("block not found")
	errTransactionNotFound = errors.New("transaction not found")
)

type testBackend struct {
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	chaindb     ethdb.Database
	chain       *core.BlockChain

	refHook func() // Hook is invoked when the requested state is referenced
	relHook func() // Hook is invoked when the requested state is released
}

func newTestBackend(t *testing.T, n int, gspec *core.Genesis, generator func(i int, b *core.BlockGen)) *testBackend {
	backend := &testBackend{
		chainConfig: gspec.Config,
		engine:      dummy.NewETHFaker(),
		chaindb:     rawdb.NewMemoryDatabase(),
	}
	// Generate blocks for testing
	_, blocks, _, err := core.GenerateChainWithGenesis(gspec, backend.engine, n, 10, generator)
	if err != nil {
		t.Fatal(err)
	}

	// Import the canonical chain
	cacheConfig := &core.CacheConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		SnapshotLimit:  128,
		Pruning:        false, // Archive mode
	}
	chain, err := core.NewBlockChain(backend.chaindb, cacheConfig, gspec, backend.engine, vm.Config{}, common.Hash{}, false)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
	for _, block := range blocks {
		if err := chain.Accept(block); err != nil {
			t.Fatalf("block %d: failed to accept block in chain: %v", n, err)
		}
	}
	backend.chain = chain
	chain.DrainAcceptorQueue()
	return backend
}

func (b *testBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return b.chain.GetHeaderByHash(hash), nil
}

func (b *testBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number == rpc.PendingBlockNumber || number == rpc.LatestBlockNumber {
		return b.chain.CurrentHeader(), nil
	}
	return b.chain.GetHeaderByNumber(uint64(number)), nil
}

func (b *testBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return b.chain.GetBlockByHash(hash), nil
}

func (b *testBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.PendingBlockNumber || number == rpc.LatestBlockNumber {
		return b.chain.CurrentBlock(), nil
	}
	return b.chain.GetBlockByNumber(uint64(number)), nil
}

func (b *testBackend) BadBlocks() ([]*types.Block, []*core.BadBlockReason) { return nil, nil }

func (b *testBackend) GetTransaction(ctx context.Context, txHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, hash, blockNumber, index := rawdb.ReadTransaction(b.chaindb, txHash)
	if tx == nil {
		return nil, common.Hash{}, 0, 0, errTransactionNotFound
	}
	return tx, hash, blockNumber, index, nil
}

func (b *testBackend) RPCGasCap() uint64 {
	return 25000000
}

func (b *testBackend) ChainConfig() *params.ChainConfig {
	return b.chainConfig
}

func (b *testBackend) Engine() consensus.Engine {
	return b.engine
}

func (b *testBackend) ChainDb() ethdb.Database {
	return b.chaindb
}

func (b *testBackend) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, StateReleaseFunc, error) {
	statedb, err := b.chain.StateAt(block.Root())
	if err != nil {
		return nil, nil, errStateNotFound
	}
	if b.refHook != nil {
		b.refHook()
	}
	release := func() {
		if b.relHook != nil {
			b.relHook()
		}
	}
	return statedb, release, nil
}

func (b *testBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (core.Message, vm.BlockContext, *state.StateDB, StateReleaseFunc, error) {
	parent := b.chain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, vm.BlockContext{}, nil, nil, errBlockNotFound
	}
	statedb, release, err := b.StateAtBlock(ctx, parent, reexec, nil, true, false)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, errStateNotFound
	}
	if txIndex == 0 && len(block.Transactions()) == 0 {
		return nil, vm.BlockContext{}, statedb, release, nil
	}
	// Recompute transactions up to the target index.
	signer := types.MakeSigner(b.chainConfig, block.Number(), new(big.Int).SetUint64(block.Time()))
	for idx, tx := range block.Transactions() {
		msg, _ := tx.AsMessage(signer, block.BaseFee())
		txContext := core.NewEVMTxContext(msg)
		context := core.NewEVMBlockContext(block.Header(), b.chain, nil)
		if idx == txIndex {
			return msg, context, statedb, release, nil
		}
		vmenv := vm.NewEVM(context, txContext, statedb, b.chainConfig, vm.Config{})
		if _, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(tx.Gas())); err != nil {
			return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("transaction %#x failed: %v", tx.Hash(), err)
		}
		statedb.Finalise(vmenv.ChainConfig().IsEIP158(block.Number()))
	}
	return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("transaction index %d out of range for block %#x", txIndex, block.Hash())
}

func TestTraceCall(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestBanffChainConfig, // TODO: go-ethereum has not enabled Shanghai yet, so we use Banff here so tests pass.
		Alloc: core.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		}}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	api := NewAPI(newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), accounts[1].addr, big.NewInt(1000), params.TxGas, b.BaseFee(), nil), signer, accounts[0].key)
		b.AddTx(tx)
	}))
	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		call        ethapi.TransactionArgs
		config      *TraceCallConfig
		expectErr   error
		expect      string
	}{
		// Standard JSON trace upon the genesis, plain transfer.
		{
			blockNumber: rpc.BlockNumber(0),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}`,
		},
		// Standard JSON trace upon the head, plain transfer.
		{
			blockNumber: rpc.BlockNumber(genBlocks),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}`,
		},
		// Standard JSON trace upon the non-existent block, error expects
		{
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: fmt.Errorf("block #%d not found", genBlocks+1),
			//expect:    nil,
		},
		// Standard JSON trace upon the latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}`,
		},
		// Tracing on 'pending' should fail:
		{
			blockNumber: rpc.PendingBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: errors.New("tracing on top of pending is not supported"),
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				Input: &hexutil.Bytes{0x43}, // blocknumber
			},
			config: &TraceCallConfig{
				BlockOverrides: &ethapi.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			expectErr: nil,
			expect: ` {"gas":53018,"failed":false,"returnValue":"","structLogs":[
		{"pc":0,"op":"NUMBER","gas":24946984,"gasCost":2,"depth":1,"stack":[]},
		{"pc":1,"op":"STOP","gas":24946982,"gasCost":0,"depth":1,"stack":["0x1337"]}]}`,
		},
	}
	for i, testspec := range testSuite {
		result, err := api.TraceCall(context.Background(), testspec.call, rpc.BlockNumberOrHash{BlockNumber: &testspec.blockNumber}, testspec.config)
		if testspec.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: expect error %v, got nothing", i, testspec.expectErr)
				continue
			}
			if !reflect.DeepEqual(err, testspec.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, git %v", i, testspec.expectErr, err)
			}
		} else {
			if err != nil {
				t.Errorf("test %d: expect no error, got %v", i, err)
				continue
			}
			var have *logger.ExecutionResult
			if err := json.Unmarshal(result.(json.RawMessage), &have); err != nil {
				t.Errorf("test %d: failed to unmarshal result %v", i, err)
			}
			var want *logger.ExecutionResult
			if err := json.Unmarshal([]byte(testspec.expect), &want); err != nil {
				t.Errorf("test %d: failed to unmarshal result %v", i, err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Errorf("test %d: result mismatch, want %v, got %v", i, testspec.expect, string(result.(json.RawMessage)))
			}
		}
	}
}

func TestTraceTransaction(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: core.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	target := common.Hash{}
	signer := types.HomesteadSigner{}
	api := NewAPI(newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), accounts[1].addr, big.NewInt(1000), params.TxGas, new(big.Int).Add(b.BaseFee(), big.NewInt(int64(500*params.GWei))), nil), signer, accounts[0].key)
		b.AddTx(tx)
		target = tx.Hash()
	}))
	result, err := api.TraceTransaction(context.Background(), target, nil)
	if err != nil {
		t.Errorf("Failed to trace transaction %v", err)
	}
	var have *logger.ExecutionResult
	if err := json.Unmarshal(result.(json.RawMessage), &have); err != nil {
		t.Errorf("failed to unmarshal result %v", err)
	}
	expected := &logger.ExecutionResult{
		Gas:         params.TxGas,
		Failed:      false,
		ReturnValue: "",
		StructLogs:  []logger.StructLogRes{},
	}
	if !reflect.DeepEqual(have, expected) {
		t.Errorf("Transaction tracing result is different: have %v want %v", have, expected)
	}
}

func TestTraceBlock(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: core.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	api := NewAPI(newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), accounts[1].addr, big.NewInt(1000), params.TxGas, new(big.Int).Add(b.BaseFee(), big.NewInt(int64(500*params.GWei))), nil), signer, accounts[0].key)
		b.AddTx(tx)
	}))

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		config      *TraceConfig
		want        string
		expectErr   error
	}{
		// Trace genesis block, expect error
		{
			blockNumber: rpc.BlockNumber(0),
			expectErr:   errors.New("genesis is not traceable"),
		},
		// Trace head block
		{
			blockNumber: rpc.BlockNumber(genBlocks),
			want:        `[{"result":{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}}]`,
		},
		// Trace non-existent block
		{
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			expectErr:   fmt.Errorf("block #%d not found", genBlocks+1),
		},
		// Trace latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			want:        `[{"result":{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}}]`,
		},
		// Trace pending block
		{
			blockNumber: rpc.PendingBlockNumber,
			want:        `[{"result":{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}}]`,
		},
	}
	for i, tc := range testSuite {
		result, err := api.TraceBlockByNumber(context.Background(), tc.blockNumber, tc.config)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %d, want error %v", i, tc.expectErr)
				continue
			}
			if !reflect.DeepEqual(err, tc.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, get %v", i, tc.expectErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d, want no error, have %v", i, err)
			continue
		}
		have, _ := json.Marshal(result)
		want := tc.want
		if string(have) != want {
			t.Errorf("test %d, result mismatch, have\n%v\n, want\n%v\n", i, string(have), want)
		}
	}
}

func TestTracingWithOverrides(t *testing.T) {
	t.Parallel()
	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestBanffChainConfig, // TODO: go-ethereum has not enabled Shanghai yet, so we use Banff here so tests pass.
		Alloc: core.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	api := NewAPI(newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), accounts[1].addr, big.NewInt(1000), params.TxGas, b.BaseFee(), nil), signer, accounts[0].key)
		b.AddTx(tx)
	}))
	randomAccounts := newAccounts(3)
	type res struct {
		Gas         int
		Failed      bool
		ReturnValue string
	}
	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		call        ethapi.TransactionArgs
		config      *TraceCallConfig
		expectErr   error
		want        string
	}{
		// Call which can only succeed if state is state overridden
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config: &TraceCallConfig{
				StateOverrides: &ethapi.StateOverride{
					randomAccounts[0].addr: ethapi.OverrideAccount{Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether)))},
				},
			},
			want: `{"gas":21000,"failed":false,"returnValue":""}`,
		},
		// Invalid call without state overriding
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    &TraceCallConfig{},
			expectErr: core.ErrInsufficientFunds,
		},
		// Successful simple contract call
		//
		// // SPDX-License-Identifier: GPL-3.0
		//
		//  pragma solidity >=0.7.0 <0.8.0;
		//
		//  /**
		//   * @title Storage
		//   * @dev Store & retrieve value in a variable
		//   */
		//  contract Storage {
		//      uint256 public number;
		//      constructor() {
		//          number = block.number;
		//      }
		//  }
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr,
				Data: newRPCBytes(common.Hex2Bytes("8381f58a")), // call number()
			},
			config: &TraceCallConfig{
				//Tracer: &tracer,
				StateOverrides: &ethapi.StateOverride{
					randomAccounts[2].addr: ethapi.OverrideAccount{
						Code:      newRPCBytes(common.Hex2Bytes("6080604052348015600f57600080fd5b506004361060285760003560e01c80638381f58a14602d575b600080fd5b60336049565b6040518082815260200191505060405180910390f35b6000548156fea2646970667358221220eab35ffa6ab2adfe380772a48b8ba78e82a1b820a18fcb6f59aa4efb20a5f60064736f6c63430007040033")),
						StateDiff: newStates([]common.Hash{{}}, []common.Hash{common.BigToHash(big.NewInt(123))}),
					},
				},
			},
			want: `{"gas":23347,"failed":false,"returnValue":"000000000000000000000000000000000000000000000000000000000000007b"}`,
		},
		{ // Override blocknumber
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &accounts[0].addr,
				// BLOCKNUMBER PUSH1 MSTORE
				Input: newRPCBytes(common.Hex2Bytes("4360005260206000f3")),
				//&hexutil.Bytes{0x43}, // blocknumber
			},
			config: &TraceCallConfig{
				BlockOverrides: &ethapi.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			want: `{"gas":59537,"failed":false,"returnValue":"0000000000000000000000000000000000000000000000000000000000001337"}`,
		},
		{ // Override blocknumber, and query a blockhash
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &accounts[0].addr,
				Input: &hexutil.Bytes{
					0x60, 0x00, 0x40, // BLOCKHASH(0)
					0x60, 0x00, 0x52, // STORE memory offset 0
					0x61, 0x13, 0x36, 0x40, // BLOCKHASH(0x1336)
					0x60, 0x20, 0x52, // STORE memory offset 32
					0x61, 0x13, 0x37, 0x40, // BLOCKHASH(0x1337)
					0x60, 0x40, 0x52, // STORE memory offset 64
					0x60, 0x60, 0x60, 0x00, 0xf3, // RETURN (0-96)

				}, // blocknumber
			},
			config: &TraceCallConfig{
				BlockOverrides: &ethapi.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			want: `{"gas":72666,"failed":false,"returnValue":"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"}`,
		},
	}
	for i, tc := range testSuite {
		result, err := api.TraceCall(context.Background(), tc.call, rpc.BlockNumberOrHash{BlockNumber: &tc.blockNumber}, tc.config)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: want error %v, have nothing", i, tc.expectErr)
				continue
			}
			if !errors.Is(err, tc.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, have %v", i, tc.expectErr, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}
		// Turn result into res-struct
		var (
			have res
			want res
		)
		resBytes, _ := json.Marshal(result)
		json.Unmarshal(resBytes, &have)
		json.Unmarshal([]byte(tc.want), &want)
		if !reflect.DeepEqual(have, want) {
			t.Errorf("test %d, result mismatch, have\n%v\n, want\n%v\n", i, string(resBytes), want)
		}
	}
}

type Account struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

type Accounts []Account

func (a Accounts) Len() int           { return len(a) }
func (a Accounts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Accounts) Less(i, j int) bool { return bytes.Compare(a[i].addr.Bytes(), a[j].addr.Bytes()) < 0 }

func newAccounts(n int) (accounts Accounts) {
	for i := 0; i < n; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		accounts = append(accounts, Account{key: key, addr: addr})
	}
	sort.Sort(accounts)
	return accounts
}

func newRPCBalance(balance *big.Int) **hexutil.Big {
	rpcBalance := (*hexutil.Big)(balance)
	return &rpcBalance
}

func newRPCBytes(bytes []byte) *hexutil.Bytes {
	rpcBytes := hexutil.Bytes(bytes)
	return &rpcBytes
}

func newStates(keys []common.Hash, vals []common.Hash) *map[common.Hash]common.Hash {
	if len(keys) != len(vals) {
		panic("invalid input")
	}
	m := make(map[common.Hash]common.Hash)
	for i := 0; i < len(keys); i++ {
		m[keys[i]] = vals[i]
	}
	return &m
}

func TestTraceChain(t *testing.T) {
	// Initialize test accounts
	// Note: the balances in this test have been increased compared to go-ethereum.
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: core.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(5 * params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(5 * params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(5 * params.Ether)},
		},
	}
	genBlocks := 50
	signer := types.HomesteadSigner{}

	var (
		ref   uint32 // total refs has made
		rel   uint32 // total rels has made
		nonce uint64
	)
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		for j := 0; j < i+1; j++ {
			tx, _ := types.SignTx(types.NewTransaction(nonce, accounts[1].addr, big.NewInt(1000), params.TxGas, b.BaseFee(), nil), signer, accounts[0].key)
			b.AddTx(tx)
			nonce += 1
		}
	})
	backend.refHook = func() { atomic.AddUint32(&ref, 1) }
	backend.relHook = func() { atomic.AddUint32(&rel, 1) }
	api := NewAPI(backend)

	single := `{"result":{"gas":21000,"failed":false,"returnValue":"","structLogs":[]}}`
	var cases = []struct {
		start  uint64
		end    uint64
		config *TraceConfig
	}{
		{0, 50, nil},  // the entire chain range, blocks [1, 50]
		{10, 20, nil}, // the middle chain range, blocks [11, 20]
	}
	for _, c := range cases {
		ref, rel = 0, 0 // clean up the counters

		from, _ := api.blockByNumber(context.Background(), rpc.BlockNumber(c.start))
		to, _ := api.blockByNumber(context.Background(), rpc.BlockNumber(c.end))
		resCh := api.traceChain(from, to, c.config, nil)

		next := c.start + 1
		for result := range resCh {
			if next != uint64(result.Block) {
				t.Error("Unexpected tracing block")
			}
			if len(result.Traces) != int(next) {
				t.Error("Unexpected tracing result")
			}
			for _, trace := range result.Traces {
				blob, _ := json.Marshal(trace)
				if string(blob) != single {
					t.Error("Unexpected tracing result")
				}
			}
			next += 1
		}
		if next != c.end+1 {
			t.Error("Missing tracing block")
		}
		if ref != rel {
			t.Errorf("Ref and deref actions are not equal, ref %d rel %d", ref, rel)
		}
	}
}
