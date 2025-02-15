// Copyright © 2022 Obol Labs Inc.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of  MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details.
//
// You should have received a copy of the GNU General Public License along with
// this program.  If not, see <http://www.gnu.org/licenses/>.

package cluster

import (
	"crypto/ecdsa"

	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/eth2util"
)

// eip712Type defines the EIP712 (https://eips.ethereum.org/EIPS/eip-712) Primary type and the Message field and value.
type eip712Type struct {
	PrimaryType string
	Field       string
	ValueFunc   func(Definition, Operator) string
}

var (
	// eip712CreatorConfigHash defines the EIP712 structure of the legacy config signature for v1.4 and later.
	eip712CreatorConfigHash = eip712Type{
		PrimaryType: "CreatorConfigHash",
		Field:       "creator_config_hash",
		ValueFunc: func(definition Definition, _ Operator) string {
			return to0xHex(definition.ConfigHash)
		},
	}

	// eip712OperatorConfigHash defines the EIP712 structure of the operator config signature for v1.4 and later.
	eip712OperatorConfigHash = eip712Type{
		PrimaryType: "OperatorConfigHash",
		Field:       "operator_config_hash",
		ValueFunc: func(definition Definition, _ Operator) string {
			return to0xHex(definition.ConfigHash)
		},
	}

	// eip712V1x3ConfigHash defines the EIP712 structure of the legacy config signature for v1.3 and before.
	eip712V1x3ConfigHash = eip712Type{
		PrimaryType: "ConfigHash",
		Field:       "config_hash",
		ValueFunc: func(definition Definition, _ Operator) string {
			return to0xHex(definition.ConfigHash)
		},
	}

	// eip712ENR defines the EIP712 structure of the enr signature.
	eip712ENR = eip712Type{
		PrimaryType: "ENR",
		Field:       "enr",
		ValueFunc: func(_ Definition, operator Operator) string {
			return operator.ENR
		},
	}
)

// getOperatorEIP712Type returns the latest or legacy operator eip712 type.
func getOperatorEIP712Type(version string) eip712Type {
	if !supportEIP712Sigs(version) {
		panic("invalid eip712 signature version") // This should never happen
	}

	if isAnyVersion(version, v1_3) {
		return eip712V1x3ConfigHash
	}

	return eip712OperatorConfigHash
}

// digestEIP712 returns the digest for the EIP712 structured type for the provided definition and operator.
func digestEIP712(typ eip712Type, def Definition, operator Operator) ([]byte, error) {
	chainID, err := eth2util.ForkVersionToChainID(def.ForkVersion)
	if err != nil {
		return nil, err
	}

	data := apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": []apitypes.Type{
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
			},
			typ.PrimaryType: []apitypes.Type{
				{Name: typ.Field, Type: "string"},
			},
		},
		PrimaryType: typ.PrimaryType,
		Message: apitypes.TypedDataMessage{
			typ.Field: typ.ValueFunc(def, operator),
		},
		Domain: apitypes.TypedDataDomain{
			Name:    "Obol",
			Version: "1",
			ChainId: ethmath.NewHexOrDecimal256(chainID),
		},
	}

	digest, _, err := apitypes.TypedDataAndHash(data)
	if err != nil {
		return nil, errors.Wrap(err, "hash EIP712")
	}

	return digest, nil
}

// signEIP712 returns the EIP712 signature for the primary type.
func signEIP712(secret *ecdsa.PrivateKey, typ eip712Type, def Definition, operator Operator) ([]byte, error) {
	digest, err := digestEIP712(typ, def, operator)
	if err != nil {
		return nil, err
	}

	sig, err := crypto.Sign(digest, secret)
	if err != nil {
		return nil, errors.Wrap(err, "sign EIP712")
	}

	return sig, nil
}
