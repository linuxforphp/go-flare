// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package secp256k1fx

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"
	"github.com/ava-labs/avalanchego/utils/formatting"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/components/verify"
)

var (
	errCantSpend = errors.New("unable to spend this UTXO")

	_ keychain.Keychain = (*Keychain)(nil)
)

// Keychain is a collection of keys that can be used to spend outputs
type Keychain struct {
	factory        *secp256k1.Factory
	addrToKeyIndex map[ids.ShortID]int

	// These can be used to iterate over. However, they should not be modified externally.
	Addrs set.Set[ids.ShortID]
	Keys  []*secp256k1.PrivateKey
}

// NewKeychain returns a new keychain containing [keys]
func NewKeychain(keys ...*secp256k1.PrivateKey) *Keychain {
	kc := &Keychain{
		factory:        &secp256k1.Factory{},
		addrToKeyIndex: make(map[ids.ShortID]int),
	}
	for _, key := range keys {
		kc.Add(key)
	}
	return kc
}

// Add a new key to the key chain
func (kc *Keychain) Add(key *secp256k1.PrivateKey) {
	addr := key.PublicKey().Address()
	if _, ok := kc.addrToKeyIndex[addr]; !ok {
		kc.addrToKeyIndex[addr] = len(kc.Keys)
		kc.Keys = append(kc.Keys, key)
		kc.Addrs.Add(addr)
	}
}

// Get a key from the keychain. If the key is unknown, return a pointer to an empty key.
// In both cases also return a boolean telling whether the key is known.
func (kc Keychain) Get(id ids.ShortID) (keychain.Signer, bool) {
	return kc.get(id)
}

// Addresses returns a list of addresses this keychain manages
func (kc Keychain) Addresses() set.Set[ids.ShortID] {
	return kc.Addrs
}

// New returns a newly generated private key
func (kc *Keychain) New() (*secp256k1.PrivateKey, error) {
	sk, err := kc.factory.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	kc.Add(sk)
	return sk, nil
}

// Spend attempts to create an input
func (kc *Keychain) Spend(out verify.Verifiable, time uint64) (verify.Verifiable, []*secp256k1.PrivateKey, error) {
	switch out := out.(type) {
	case *MintOutput:
		if sigIndices, keys, able := kc.Match(&out.OutputOwners, time); able {
			return &Input{
				SigIndices: sigIndices,
			}, keys, nil
		}
		return nil, nil, errCantSpend
	case *TransferOutput:
		if sigIndices, keys, able := kc.Match(&out.OutputOwners, time); able {
			return &TransferInput{
				Amt: out.Amt,
				Input: Input{
					SigIndices: sigIndices,
				},
			}, keys, nil
		}
		return nil, nil, errCantSpend
	}
	return nil, nil, fmt.Errorf("can't spend UTXO because it is unexpected type %T", out)
}

// Match attempts to match a list of addresses up to the provided threshold
func (kc *Keychain) Match(owners *OutputOwners, time uint64) ([]uint32, []*secp256k1.PrivateKey, bool) {
	if time < owners.Locktime {
		return nil, nil, false
	}
	sigs := make([]uint32, 0, owners.Threshold)
	keys := make([]*secp256k1.PrivateKey, 0, owners.Threshold)
	for i := uint32(0); i < uint32(len(owners.Addrs)) && uint32(len(keys)) < owners.Threshold; i++ {
		if key, exists := kc.get(owners.Addrs[i]); exists {
			sigs = append(sigs, i)
			keys = append(keys, key)
		}
	}
	return sigs, keys, uint32(len(keys)) == owners.Threshold
}

// PrefixedString returns the key chain as a string representation with [prefix]
// added before every line.
func (kc *Keychain) PrefixedString(prefix string) string {
	sb := strings.Builder{}
	format := fmt.Sprintf("%%sKey[%s]: Key: %%s Address: %%s\n",
		formatting.IntFormat(len(kc.Keys)-1))
	for i, key := range kc.Keys {
		// We assume that the maximum size of a byte slice that
		// can be stringified is at least the length of a SECP256K1 private key
		keyStr, _ := formatting.Encode(formatting.HexNC, key.Bytes())
		sb.WriteString(fmt.Sprintf(format,
			prefix,
			i,
			keyStr,
			key.PublicKey().Address(),
		))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

func (kc *Keychain) String() string {
	return kc.PrefixedString("")
}

// to avoid internals type assertions
func (kc Keychain) get(id ids.ShortID) (*secp256k1.PrivateKey, bool) {
	if i, ok := kc.addrToKeyIndex[id]; ok {
		return kc.Keys[i], true
	}
	return nil, false
}
