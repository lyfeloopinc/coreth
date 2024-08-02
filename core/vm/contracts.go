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
// Copyright 2014 The go-ethereum Authors
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

package vm

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ava-labs/coreth/params"
	"github.com/ava-labs/coreth/precompile/contract"
	"github.com/ava-labs/coreth/precompile/modules"
	"github.com/ava-labs/coreth/vmerrs"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/blake2b"
	"github.com/ethereum/go-ethereum/crypto/bn256"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"golang.org/x/crypto/ripemd160"
)

// PrecompiledContract is the basic interface for native Go contracts. The implementation
// requires a deterministic gas count based on the input size of the Run method of the
// contract.
type PrecompiledContract interface {
	RequiredGas(input []byte) uint64  // RequiredPrice calculates the contract gas use
	Run(input []byte) ([]byte, error) // Run runs the precompiled contract
}

// PrecompiledContractsHomestead contains the default set of pre-compiled Ethereum
// contracts used in the Frontier and Homestead releases.
var PrecompiledContractsHomestead = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
}

// PrecompiledContractsByzantium contains the default set of pre-compiled Ethereum
// contracts used in the Byzantium release.
var PrecompiledContractsByzantium = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: false}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddByzantium{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulByzantium{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingByzantium{}),
}

// PrecompiledContractsIstanbul contains the default set of pre-compiled Ethereum
// contracts used in the Istanbul release.
var PrecompiledContractsIstanbul = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: false}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}): newWrappedPrecompiledContract(&blake2F{}),
}

// PrecompiledContractsApricotPhase2 contains the default set of pre-compiled Ethereum
// contracts used in the Apricot Phase 2 release.
var PrecompiledContractsApricotPhase2 = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: true}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}): newWrappedPrecompiledContract(&blake2F{}),
	genesisContractAddr:              &deprecatedContract{},
	NativeAssetBalanceAddr:           &nativeAssetBalance{gasCost: params.AssetBalanceApricot},
	NativeAssetCallAddr:              &nativeAssetCall{gasCost: params.AssetCallApricot},
}

// PrecompiledContractsApricotPhasePre6 contains the default set of pre-compiled Ethereum
// contracts used in the PrecompiledContractsApricotPhasePre6 release.
var PrecompiledContractsApricotPhasePre6 = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: true}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}): newWrappedPrecompiledContract(&blake2F{}),
	genesisContractAddr:              &deprecatedContract{},
	NativeAssetBalanceAddr:           &deprecatedContract{},
	NativeAssetCallAddr:              &deprecatedContract{},
}

// PrecompiledContractsApricotPhase6 contains the default set of pre-compiled Ethereum
// contracts used in the Apricot Phase 6 release.
var PrecompiledContractsApricotPhase6 = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: true}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}): newWrappedPrecompiledContract(&blake2F{}),
	genesisContractAddr:              &deprecatedContract{},
	NativeAssetBalanceAddr:           &nativeAssetBalance{gasCost: params.AssetBalanceApricot},
	NativeAssetCallAddr:              &nativeAssetCall{gasCost: params.AssetCallApricot},
}

// PrecompiledContractsBanff contains the default set of pre-compiled Ethereum
// contracts used in the Banff release.
var PrecompiledContractsBanff = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}): newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}): newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}): newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}): newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}): newWrappedPrecompiledContract(&bigModExp{eip2565: true}),
	common.BytesToAddress([]byte{6}): newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}): newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}): newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}): newWrappedPrecompiledContract(&blake2F{}),
	genesisContractAddr:              &deprecatedContract{},
	NativeAssetBalanceAddr:           &deprecatedContract{},
	NativeAssetCallAddr:              &deprecatedContract{},
}

// PrecompiledContractsCancun contains the default set of pre-compiled Ethereum
// contracts used in the Cancun release.
var PrecompiledContractsCancun = map[common.Address]contract.StatefulPrecompiledContract{
	common.BytesToAddress([]byte{1}):    newWrappedPrecompiledContract(&ecrecover{}),
	common.BytesToAddress([]byte{2}):    newWrappedPrecompiledContract(&sha256hash{}),
	common.BytesToAddress([]byte{3}):    newWrappedPrecompiledContract(&ripemd160hash{}),
	common.BytesToAddress([]byte{4}):    newWrappedPrecompiledContract(&dataCopy{}),
	common.BytesToAddress([]byte{5}):    newWrappedPrecompiledContract(&bigModExp{eip2565: true}),
	common.BytesToAddress([]byte{6}):    newWrappedPrecompiledContract(&bn256AddIstanbul{}),
	common.BytesToAddress([]byte{7}):    newWrappedPrecompiledContract(&bn256ScalarMulIstanbul{}),
	common.BytesToAddress([]byte{8}):    newWrappedPrecompiledContract(&bn256PairingIstanbul{}),
	common.BytesToAddress([]byte{9}):    newWrappedPrecompiledContract(&blake2F{}),
	common.BytesToAddress([]byte{0x0a}): newWrappedPrecompiledContract(&kzgPointEvaluation{}),
}

// PrecompiledContractsBLS contains the set of pre-compiled Ethereum
// contracts specified in EIP-2537. These are exported for testing purposes.
var PrecompiledContractsBLS = map[common.Address]contract.StatefulPrecompiledContract{}

var (
	PrecompiledAddressesCancun           []common.Address
	PrecompiledAddressesBanff            []common.Address
	PrecompiledAddressesApricotPhase6    []common.Address
	PrecompiledAddressesApricotPhasePre6 []common.Address
	PrecompiledAddressesApricotPhase2    []common.Address
	PrecompiledAddressesIstanbul         []common.Address
	PrecompiledAddressesByzantium        []common.Address
	PrecompiledAddressesHomestead        []common.Address
	PrecompiledAddressesBLS              []common.Address
	PrecompileAllNativeAddresses         map[common.Address]struct{}
)

func init() {
	for k := range PrecompiledContractsHomestead {
		PrecompiledAddressesHomestead = append(PrecompiledAddressesHomestead, k)
	}
	for k := range PrecompiledContractsByzantium {
		PrecompiledAddressesByzantium = append(PrecompiledAddressesByzantium, k)
	}
	for k := range PrecompiledContractsIstanbul {
		PrecompiledAddressesIstanbul = append(PrecompiledAddressesIstanbul, k)
	}
	for k := range PrecompiledContractsApricotPhase2 {
		PrecompiledAddressesApricotPhase2 = append(PrecompiledAddressesApricotPhase2, k)
	}
	for k := range PrecompiledContractsApricotPhasePre6 {
		PrecompiledAddressesApricotPhasePre6 = append(PrecompiledAddressesApricotPhasePre6, k)
	}
	for k := range PrecompiledContractsApricotPhase6 {
		PrecompiledAddressesApricotPhase6 = append(PrecompiledAddressesApricotPhase6, k)
	}
	for k := range PrecompiledContractsBanff {
		PrecompiledAddressesBanff = append(PrecompiledAddressesBanff, k)
	}
	for k := range PrecompiledContractsCancun {
		PrecompiledAddressesCancun = append(PrecompiledAddressesCancun, k)
	}
	for k := range PrecompiledContractsBLS {
		PrecompiledAddressesBLS = append(PrecompiledAddressesBLS, k)
	}

	// Set of all native precompile addresses that are in use
	// Note: this will repeat some addresses, but this is cheap and makes the code clearer.
	PrecompileAllNativeAddresses = make(map[common.Address]struct{})
	addrsList := append(PrecompiledAddressesHomestead, PrecompiledAddressesByzantium...)
	addrsList = append(addrsList, PrecompiledAddressesIstanbul...)
	addrsList = append(addrsList, PrecompiledAddressesApricotPhase2...)
	addrsList = append(addrsList, PrecompiledAddressesApricotPhasePre6...)
	addrsList = append(addrsList, PrecompiledAddressesApricotPhase6...)
	addrsList = append(addrsList, PrecompiledAddressesBanff...)
	addrsList = append(addrsList, PrecompiledAddressesCancun...)
	addrsList = append(addrsList, PrecompiledAddressesBLS...)
	for _, k := range addrsList {
		PrecompileAllNativeAddresses[k] = struct{}{}
	}

	// Ensure that this package will panic during init if there is a conflict present with the declared
	// precompile addresses.
	for _, module := range modules.RegisteredModules() {
		address := module.Address
		if _, ok := PrecompileAllNativeAddresses[address]; ok {
			panic(fmt.Errorf("precompile address collides with existing native address: %s", address))
		}
	}
}

// ActivePrecompiles returns the precompiles enabled with the current configuration.
func ActivePrecompiles(rules params.Rules) []common.Address {
	switch {
	case rules.IsCancun:
		return PrecompiledAddressesCancun
	case rules.IsBanff:
		return PrecompiledAddressesBanff
	case rules.IsApricotPhase2:
		return PrecompiledAddressesApricotPhase2
	case rules.IsIstanbul:
		return PrecompiledAddressesIstanbul
	case rules.IsByzantium:
		return PrecompiledAddressesByzantium
	default:
		return PrecompiledAddressesHomestead
	}
}

// RunPrecompiledContract runs and evaluates the output of a precompiled contract.
// It returns
// - the returned bytes,
// - the _remaining_ gas,
// - any error that occurred
func RunPrecompiledContract(p PrecompiledContract, input []byte, suppliedGas uint64) (ret []byte, remainingGas uint64, err error) {
	gasCost := p.RequiredGas(input)
	if suppliedGas < gasCost {
		return nil, 0, vmerrs.ErrOutOfGas
	}
	suppliedGas -= gasCost
	output, err := p.Run(input)
	return output, suppliedGas, err
}

// ECRECOVER implemented as a native contract.
type ecrecover struct{}

func (c *ecrecover) RequiredGas(input []byte) uint64 {
	return params.EcrecoverGas
}

func (c *ecrecover) Run(input []byte) ([]byte, error) {
	const ecRecoverInputLength = 128

	input = common.RightPadBytes(input, ecRecoverInputLength)
	// "input" is (hash, v, r, s), each 32 bytes
	// but for ecrecover we want (r, s, v)

	r := new(big.Int).SetBytes(input[64:96])
	s := new(big.Int).SetBytes(input[96:128])
	v := input[63] - 27

	// tighter sig s values input homestead only apply to tx sigs
	if !allZero(input[32:63]) || !crypto.ValidateSignatureValues(v, r, s, false) {
		return nil, nil
	}
	// We must make sure not to modify the 'input', so placing the 'v' along with
	// the signature needs to be done on a new allocation
	sig := make([]byte, 65)
	copy(sig, input[64:128])
	sig[64] = v
	// v needs to be at the end for libsecp256k1
	pubKey, err := crypto.Ecrecover(input[:32], sig)
	// make sure the public key is a valid one
	if err != nil {
		return nil, nil
	}

	// the first byte of pubkey is bitcoin heritage
	return common.LeftPadBytes(crypto.Keccak256(pubKey[1:])[12:], 32), nil
}

// SHA256 implemented as a native contract.
type sha256hash struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *sha256hash) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.Sha256PerWordGas + params.Sha256BaseGas
}
func (c *sha256hash) Run(input []byte) ([]byte, error) {
	h := sha256.Sum256(input)
	return h[:], nil
}

// RIPEMD160 implemented as a native contract.
type ripemd160hash struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *ripemd160hash) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.Ripemd160PerWordGas + params.Ripemd160BaseGas
}
func (c *ripemd160hash) Run(input []byte) ([]byte, error) {
	ripemd := ripemd160.New()
	ripemd.Write(input)
	return common.LeftPadBytes(ripemd.Sum(nil), 32), nil
}

// data copy implemented as a native contract.
type dataCopy struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *dataCopy) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.IdentityPerWordGas + params.IdentityBaseGas
}
func (c *dataCopy) Run(in []byte) ([]byte, error) {
	return common.CopyBytes(in), nil
}

// bigModExp implements a native big integer exponential modular operation.
type bigModExp struct {
	eip2565 bool
}

var (
	big0      = big.NewInt(0)
	big1      = big.NewInt(1)
	big3      = big.NewInt(3)
	big4      = big.NewInt(4)
	big7      = big.NewInt(7)
	big8      = big.NewInt(8)
	big16     = big.NewInt(16)
	big20     = big.NewInt(20)
	big32     = big.NewInt(32)
	big64     = big.NewInt(64)
	big96     = big.NewInt(96)
	big480    = big.NewInt(480)
	big1024   = big.NewInt(1024)
	big3072   = big.NewInt(3072)
	big199680 = big.NewInt(199680)
)

// modexpMultComplexity implements bigModexp multComplexity formula, as defined in EIP-198
//
//	def mult_complexity(x):
//		if x <= 64: return x ** 2
//		elif x <= 1024: return x ** 2 // 4 + 96 * x - 3072
//		else: return x ** 2 // 16 + 480 * x - 199680
//
// where is x is max(length_of_MODULUS, length_of_BASE)
func modexpMultComplexity(x *big.Int) *big.Int {
	switch {
	case x.Cmp(big64) <= 0:
		x.Mul(x, x) // x ** 2
	case x.Cmp(big1024) <= 0:
		// (x ** 2 // 4 ) + ( 96 * x - 3072)
		x = new(big.Int).Add(
			new(big.Int).Div(new(big.Int).Mul(x, x), big4),
			new(big.Int).Sub(new(big.Int).Mul(big96, x), big3072),
		)
	default:
		// (x ** 2 // 16) + (480 * x - 199680)
		x = new(big.Int).Add(
			new(big.Int).Div(new(big.Int).Mul(x, x), big16),
			new(big.Int).Sub(new(big.Int).Mul(big480, x), big199680),
		)
	}
	return x
}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bigModExp) RequiredGas(input []byte) uint64 {
	var (
		baseLen = new(big.Int).SetBytes(getData(input, 0, 32))
		expLen  = new(big.Int).SetBytes(getData(input, 32, 32))
		modLen  = new(big.Int).SetBytes(getData(input, 64, 32))
	)
	if len(input) > 96 {
		input = input[96:]
	} else {
		input = input[:0]
	}
	// Retrieve the head 32 bytes of exp for the adjusted exponent length
	var expHead *big.Int
	if big.NewInt(int64(len(input))).Cmp(baseLen) <= 0 {
		expHead = new(big.Int)
	} else {
		if expLen.Cmp(big32) > 0 {
			expHead = new(big.Int).SetBytes(getData(input, baseLen.Uint64(), 32))
		} else {
			expHead = new(big.Int).SetBytes(getData(input, baseLen.Uint64(), expLen.Uint64()))
		}
	}
	// Calculate the adjusted exponent length
	var msb int
	if bitlen := expHead.BitLen(); bitlen > 0 {
		msb = bitlen - 1
	}
	adjExpLen := new(big.Int)
	if expLen.Cmp(big32) > 0 {
		adjExpLen.Sub(expLen, big32)
		adjExpLen.Mul(big8, adjExpLen)
	}
	adjExpLen.Add(adjExpLen, big.NewInt(int64(msb)))
	// Calculate the gas cost of the operation
	gas := new(big.Int).Set(math.BigMax(modLen, baseLen))
	if c.eip2565 {
		// EIP-2565 has three changes
		// 1. Different multComplexity (inlined here)
		// in EIP-2565 (https://eips.ethereum.org/EIPS/eip-2565):
		//
		// def mult_complexity(x):
		//    ceiling(x/8)^2
		//
		//where is x is max(length_of_MODULUS, length_of_BASE)
		gas = gas.Add(gas, big7)
		gas = gas.Div(gas, big8)
		gas.Mul(gas, gas)

		gas.Mul(gas, math.BigMax(adjExpLen, big1))
		// 2. Different divisor (`GQUADDIVISOR`) (3)
		gas.Div(gas, big3)
		if gas.BitLen() > 64 {
			return math.MaxUint64
		}
		// 3. Minimum price of 200 gas
		if gas.Uint64() < 200 {
			return 200
		}
		return gas.Uint64()
	}
	gas = modexpMultComplexity(gas)
	gas.Mul(gas, math.BigMax(adjExpLen, big1))
	gas.Div(gas, big20)

	if gas.BitLen() > 64 {
		return math.MaxUint64
	}
	return gas.Uint64()
}

func (c *bigModExp) Run(input []byte) ([]byte, error) {
	var (
		baseLen = new(big.Int).SetBytes(getData(input, 0, 32)).Uint64()
		expLen  = new(big.Int).SetBytes(getData(input, 32, 32)).Uint64()
		modLen  = new(big.Int).SetBytes(getData(input, 64, 32)).Uint64()
	)
	if len(input) > 96 {
		input = input[96:]
	} else {
		input = input[:0]
	}
	// Handle a special case when both the base and mod length is zero
	if baseLen == 0 && modLen == 0 {
		return []byte{}, nil
	}
	// Retrieve the operands and execute the exponentiation
	var (
		base = new(big.Int).SetBytes(getData(input, 0, baseLen))
		exp  = new(big.Int).SetBytes(getData(input, baseLen, expLen))
		mod  = new(big.Int).SetBytes(getData(input, baseLen+expLen, modLen))
		v    []byte
	)
	switch {
	case mod.BitLen() == 0:
		// Modulo 0 is undefined, return zero
		return common.LeftPadBytes([]byte{}, int(modLen)), nil
	case base.BitLen() == 1: // a bit length of 1 means it's 1 (or -1).
		//If base == 1, then we can just return base % mod (if mod >= 1, which it is)
		v = base.Mod(base, mod).Bytes()
	default:
		v = base.Exp(base, exp, mod).Bytes()
	}
	return common.LeftPadBytes(v, int(modLen)), nil
}

// newCurvePoint unmarshals a binary blob into a bn256 elliptic curve point,
// returning it, or an error if the point is invalid.
func newCurvePoint(blob []byte) (*bn256.G1, error) {
	p := new(bn256.G1)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

// newTwistPoint unmarshals a binary blob into a bn256 elliptic curve point,
// returning it, or an error if the point is invalid.
func newTwistPoint(blob []byte) (*bn256.G2, error) {
	p := new(bn256.G2)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

// runBn256Add implements the Bn256Add precompile, referenced by both
// Byzantium and Istanbul operations.
func runBn256Add(input []byte) ([]byte, error) {
	x, err := newCurvePoint(getData(input, 0, 64))
	if err != nil {
		return nil, err
	}
	y, err := newCurvePoint(getData(input, 64, 64))
	if err != nil {
		return nil, err
	}
	res := new(bn256.G1)
	res.Add(x, y)
	return res.Marshal(), nil
}

// bn256Add implements a native elliptic curve point addition conforming to
// Istanbul consensus rules.
type bn256AddIstanbul struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256AddIstanbul) RequiredGas(input []byte) uint64 {
	return params.Bn256AddGasIstanbul
}

func (c *bn256AddIstanbul) Run(input []byte) ([]byte, error) {
	return runBn256Add(input)
}

// bn256AddByzantium implements a native elliptic curve point addition
// conforming to Byzantium consensus rules.
type bn256AddByzantium struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256AddByzantium) RequiredGas(input []byte) uint64 {
	return params.Bn256AddGasByzantium
}

func (c *bn256AddByzantium) Run(input []byte) ([]byte, error) {
	return runBn256Add(input)
}

// runBn256ScalarMul implements the Bn256ScalarMul precompile, referenced by
// both Byzantium and Istanbul operations.
func runBn256ScalarMul(input []byte) ([]byte, error) {
	p, err := newCurvePoint(getData(input, 0, 64))
	if err != nil {
		return nil, err
	}
	res := new(bn256.G1)
	res.ScalarMult(p, new(big.Int).SetBytes(getData(input, 64, 32)))
	return res.Marshal(), nil
}

// bn256ScalarMulIstanbul implements a native elliptic curve scalar
// multiplication conforming to Istanbul consensus rules.
type bn256ScalarMulIstanbul struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256ScalarMulIstanbul) RequiredGas(input []byte) uint64 {
	return params.Bn256ScalarMulGasIstanbul
}

func (c *bn256ScalarMulIstanbul) Run(input []byte) ([]byte, error) {
	return runBn256ScalarMul(input)
}

// bn256ScalarMulByzantium implements a native elliptic curve scalar
// multiplication conforming to Byzantium consensus rules.
type bn256ScalarMulByzantium struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256ScalarMulByzantium) RequiredGas(input []byte) uint64 {
	return params.Bn256ScalarMulGasByzantium
}

func (c *bn256ScalarMulByzantium) Run(input []byte) ([]byte, error) {
	return runBn256ScalarMul(input)
}

var (
	// true32Byte is returned if the bn256 pairing check succeeds.
	true32Byte = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}

	// false32Byte is returned if the bn256 pairing check fails.
	false32Byte = make([]byte, 32)

	// errBadPairingInput is returned if the bn256 pairing input is invalid.
	errBadPairingInput = errors.New("bad elliptic curve pairing size")
)

// runBn256Pairing implements the Bn256Pairing precompile, referenced by both
// Byzantium and Istanbul operations.
func runBn256Pairing(input []byte) ([]byte, error) {
	// Handle some corner cases cheaply
	if len(input)%192 > 0 {
		return nil, errBadPairingInput
	}
	// Convert the input into a set of coordinates
	var (
		cs []*bn256.G1
		ts []*bn256.G2
	)
	for i := 0; i < len(input); i += 192 {
		c, err := newCurvePoint(input[i : i+64])
		if err != nil {
			return nil, err
		}
		t, err := newTwistPoint(input[i+64 : i+192])
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
		ts = append(ts, t)
	}
	// Execute the pairing checks and return the results
	if bn256.PairingCheck(cs, ts) {
		return true32Byte, nil
	}
	return false32Byte, nil
}

// bn256PairingIstanbul implements a pairing pre-compile for the bn256 curve
// conforming to Istanbul consensus rules.
type bn256PairingIstanbul struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256PairingIstanbul) RequiredGas(input []byte) uint64 {
	return params.Bn256PairingBaseGasIstanbul + uint64(len(input)/192)*params.Bn256PairingPerPointGasIstanbul
}

func (c *bn256PairingIstanbul) Run(input []byte) ([]byte, error) {
	return runBn256Pairing(input)
}

// bn256PairingByzantium implements a pairing pre-compile for the bn256 curve
// conforming to Byzantium consensus rules.
type bn256PairingByzantium struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256PairingByzantium) RequiredGas(input []byte) uint64 {
	return params.Bn256PairingBaseGasByzantium + uint64(len(input)/192)*params.Bn256PairingPerPointGasByzantium
}

func (c *bn256PairingByzantium) Run(input []byte) ([]byte, error) {
	return runBn256Pairing(input)
}

type blake2F struct{}

func (c *blake2F) RequiredGas(input []byte) uint64 {
	// If the input is malformed, we can't calculate the gas, return 0 and let the
	// actual call choke and fault.
	if len(input) != blake2FInputLength {
		return 0
	}
	return uint64(binary.BigEndian.Uint32(input[0:4]))
}

const (
	blake2FInputLength        = 213
	blake2FFinalBlockBytes    = byte(1)
	blake2FNonFinalBlockBytes = byte(0)
)

var (
	errBlake2FInvalidInputLength = errors.New("invalid input length")
	errBlake2FInvalidFinalFlag   = errors.New("invalid final flag")
)

func (c *blake2F) Run(input []byte) ([]byte, error) {
	// Make sure the input is valid (correct length and final flag)
	if len(input) != blake2FInputLength {
		return nil, errBlake2FInvalidInputLength
	}
	if input[212] != blake2FNonFinalBlockBytes && input[212] != blake2FFinalBlockBytes {
		return nil, errBlake2FInvalidFinalFlag
	}
	// Parse the input into the Blake2b call parameters
	var (
		rounds = binary.BigEndian.Uint32(input[0:4])
		final  = input[212] == blake2FFinalBlockBytes

		h [8]uint64
		m [16]uint64
		t [2]uint64
	)
	for i := 0; i < 8; i++ {
		offset := 4 + i*8
		h[i] = binary.LittleEndian.Uint64(input[offset : offset+8])
	}
	for i := 0; i < 16; i++ {
		offset := 68 + i*8
		m[i] = binary.LittleEndian.Uint64(input[offset : offset+8])
	}
	t[0] = binary.LittleEndian.Uint64(input[196:204])
	t[1] = binary.LittleEndian.Uint64(input[204:212])

	// Execute the compression function, extract and return the result
	blake2b.F(&h, m, t, final, rounds)

	output := make([]byte, 64)
	for i := 0; i < 8; i++ {
		offset := i * 8
		binary.LittleEndian.PutUint64(output[offset:offset+8], h[i])
	}
	return output, nil
}

// kzgPointEvaluation implements the EIP-4844 point evaluation precompile.
type kzgPointEvaluation struct{}

// RequiredGas estimates the gas required for running the point evaluation precompile.
func (b *kzgPointEvaluation) RequiredGas(input []byte) uint64 {
	return params.BlobTxPointEvaluationPrecompileGas
}

const (
	blobVerifyInputLength           = 192  // Max input length for the point evaluation precompile.
	blobCommitmentVersionKZG  uint8 = 0x01 // Version byte for the point evaluation precompile.
	blobPrecompileReturnValue       = "000000000000000000000000000000000000000000000000000000000000100073eda753299d7d483339d80809a1d80553bda402fffe5bfeffffffff00000001"
)

var (
	errBlobVerifyInvalidInputLength = errors.New("invalid input length")
	errBlobVerifyMismatchedVersion  = errors.New("mismatched versioned hash")
	errBlobVerifyKZGProof           = errors.New("error verifying kzg proof")
)

// Run executes the point evaluation precompile.
func (b *kzgPointEvaluation) Run(input []byte) ([]byte, error) {
	if len(input) != blobVerifyInputLength {
		return nil, errBlobVerifyInvalidInputLength
	}
	// versioned hash: first 32 bytes
	var versionedHash common.Hash
	copy(versionedHash[:], input[:])

	var (
		point kzg4844.Point
		claim kzg4844.Claim
	)
	// Evaluation point: next 32 bytes
	copy(point[:], input[32:])
	// Expected output: next 32 bytes
	copy(claim[:], input[64:])

	// input kzg point: next 48 bytes
	var commitment kzg4844.Commitment
	copy(commitment[:], input[96:])
	if kZGToVersionedHash(commitment) != versionedHash {
		return nil, errBlobVerifyMismatchedVersion
	}

	// Proof: next 48 bytes
	var proof kzg4844.Proof
	copy(proof[:], input[144:])

	if err := kzg4844.VerifyProof(commitment, point, claim, proof); err != nil {
		return nil, fmt.Errorf("%w: %v", errBlobVerifyKZGProof, err)
	}

	return common.Hex2Bytes(blobPrecompileReturnValue), nil
}

// kZGToVersionedHash implements kzg_to_versioned_hash from EIP-4844
func kZGToVersionedHash(kzg kzg4844.Commitment) common.Hash {
	h := sha256.Sum256(kzg[:])
	h[0] = blobCommitmentVersionKZG

	return h
}
