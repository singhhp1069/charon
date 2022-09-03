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

package p2p

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/obolnetwork/charon/app/errors"
)

// LoadPrivKey returns the ecdsa k1 key saved in the file.
func LoadPrivKey(privKey string) (*ecdsa.PrivateKey, error) {
	key, err := crypto.LoadECDSA(privKey)
	if err != nil {
		return nil, errors.Wrap(err, "load priv key")
	}

	return key, nil
}

// NewSavedPrivKey generates a new ecdsa k1 private key and saves it to the key file path.
func NewSavedPrivKey(privKeyFile string) (*ecdsa.PrivateKey, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, errors.Wrap(err, "gen key")
	}

	err = crypto.SaveECDSA(privKeyFile, key)
	if err != nil {
		return nil, errors.Wrap(err, "save key")
	}

	return key, nil
}
