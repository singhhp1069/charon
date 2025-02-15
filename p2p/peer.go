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
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/obolnetwork/charon/app/errors"
)

// Peer represents a peer in the libp2p network, either a charon node or a relay.
type Peer struct {
	// ENR defines the networking information of the peer.
	ENR enr.Record

	// Enode represents the networking host of the peer.
	Enode enode.Node

	// ID is a libp2p peer identity. It is inferred from the ENR.
	ID peer.ID

	// Index is the order of this node in the cluster.
	// This is only applicable to charon nodes, not relays.
	Index int

	// Name represents a human friendly name for the peer.
	Name string
}

// ShareIdx returns share index of this Peer. ShareIdx is 1-indexed while peerIdx is 0-indexed.
func (p Peer) ShareIdx() int {
	return p.Index + 1
}

// PublicKey returns peer public key.
func (p Peer) PublicKey() (*ecdsa.PublicKey, error) {
	return PeerIDToKey(p.ID)
}

// MultiAddr returns the libp2p multiaddr (tcp ip and port) from the peer ENR.
func (p Peer) MultiAddr() (ma.Multiaddr, error) {
	tcpAddr, err := p.TCPAddr()
	if err != nil {
		return nil, err
	}

	return multiAddrFromIPPort(tcpAddr.IP, tcpAddr.Port)
}

// AddrInfo returns the libp2p peer addr info (peer ID and multiaddr) from the peer ENR.
func (p Peer) AddrInfo() (peer.AddrInfo, error) {
	addr, err := p.MultiAddr()
	if err != nil {
		return peer.AddrInfo{}, err
	}

	return peer.AddrInfo{
		ID:    p.ID,
		Addrs: []ma.Multiaddr{addr},
	}, nil
}

// TCPAddr returns the tcp address (ip and port) from the peer ENR.
func (p Peer) TCPAddr() (*net.TCPAddr, error) {
	var (
		ip   enr.IPv4
		port enr.TCP
	)
	if err := p.ENR.Load(&ip); err != nil {
		return nil, errors.Wrap(err, "load ip from enr")
	}
	if err := p.ENR.Load(&port); err != nil {
		return nil, errors.Wrap(err, "load port from enr")
	}

	return resolveListenAddr(fmt.Sprintf("%s:%d", net.IP(ip), port))
}

// NewPeer returns a new charon peer.
func NewPeer(record enr.Record, index int) (Peer, error) {
	var enodePubkey enode.Secp256k1
	if err := record.Load(&enodePubkey); err != nil {
		return Peer{}, errors.Wrap(err, "pubkey from enr")
	}

	ecdsaPubkey := ecdsa.PublicKey(enodePubkey)

	id, err := PeerIDFromKey(&ecdsaPubkey)
	if err != nil {
		return Peer{}, err
	}

	node, err := enode.New(new(enode.V4ID), &record)
	if err != nil {
		return Peer{}, errors.Wrap(err, "new peer enode")
	}

	return Peer{
		ENR:   record,
		Enode: *node,
		ID:    id,
		Index: index,
		Name:  PeerName(id),
	}, nil
}

// PeerIDToKey returns the public key of the peer ID.
func PeerIDToKey(p peer.ID) (*ecdsa.PublicKey, error) {
	pk, err := p.ExtractPublicKey()
	if err != nil {
		return nil, errors.Wrap(err, "extract pubkey")
	}

	raw, err := pk.Raw()
	if err != nil {
		return nil, errors.Wrap(err, "raw pubkey")
	}

	resp, err := crypto.DecompressPubkey(raw)
	if err != nil {
		return nil, errors.Wrap(err, "decompress pubkey")
	}

	return resp, nil
}

// PeerIDFromKey returns the peer ID of the public key.
func PeerIDFromKey(pubkey *ecdsa.PublicKey) (peer.ID, error) {
	p2pPubkey, err := libp2pcrypto.UnmarshalSecp256k1PublicKey(crypto.CompressPubkey(pubkey))
	if err != nil {
		return "", errors.Wrap(err, "convert pubkey")
	}

	id, err := peer.IDFromPublicKey(p2pPubkey)
	if err != nil {
		return "", errors.Wrap(err, "p2p id from pubkey")
	}

	return id, nil
}

// NewMutablePeer returns a new non-empty mutable peer.
func NewMutablePeer(p Peer) *MutablePeer {
	return &MutablePeer{peer: &p}
}

// MutablePeer defines a mutable peer used mostly for stateless bootnodes/relays that change ID on restart
// but have a consistent URL to resolve them. The zero value is a valid empty MutablePeer.
type MutablePeer struct {
	mu   sync.Mutex
	peer *Peer
	subs []func(Peer)
}

// Set updates the mutable enode and calls all subscribers.
func (p *MutablePeer) Set(peer Peer) {
	p.mu.Lock()
	p.peer = &peer
	clone := append([]func(Peer){}, p.subs...)
	p.mu.Unlock()

	for _, sub := range clone {
		sub(peer)
	}
}

// Peer returns the current peer or false if not available.
func (p *MutablePeer) Peer() (Peer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.peer == nil {
		return Peer{}, false
	}

	return *p.peer, true
}

// Subscribe registers a function that is called when the peer is updated.
func (p *MutablePeer) Subscribe(sub func(Peer)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.subs = append(p.subs, sub)
}

// VerifyP2PKey returns an error if the p2pkey doesn't match any lock operator ENR.
func VerifyP2PKey(peers []Peer, key *ecdsa.PrivateKey) error {
	wantBytes := crypto.CompressPubkey(&key.PublicKey)

	for _, p := range peers {
		pk, err := p.ID.ExtractPublicKey()
		if err != nil {
			return errors.Wrap(err, "extract pubkey from peer id")
		}

		gotBytes, err := pk.Raw()
		if err != nil {
			return errors.Wrap(err, "key to bytes")
		}

		if bytes.Equal(wantBytes, gotBytes) {
			return nil
		}
	}

	return errors.New("unknown private key provided, it doesn't match any public key encoded inside the operator ENRs") // This message seems to know something about the context in which it is used :(
}
