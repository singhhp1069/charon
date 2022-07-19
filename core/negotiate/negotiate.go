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

package negotiate

import (
	"context"
	"io"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"google.golang.org/protobuf/proto"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/log"
	"github.com/obolnetwork/charon/app/z"
	pbv1 "github.com/obolnetwork/charon/core/corepb/v1"
	"github.com/obolnetwork/charon/p2p"
)

const protocolID = "charon/negotiate/1.0.0"

// Consensus abstracts consensus on negotiated topics.
type Consensus interface {
	Propose(ctx context.Context, slot int64, result *pbv1.NegotiateResult)
	Subscribe(func(ctx context.Context, slot int64, result *pbv1.NegotiateResult) error)
}

// msgProvider abstracts creation of a new signed negotiation messages.
type msgProvider func(slot int64) (*pbv1.NegotiateMsg, error)

// msgValidator abstracts validation of a received negotiation messages.
type msgValidator func(*pbv1.NegotiateMsg) error

// tickerProvider abstracts the consensus timeout ticker (for testing purposes only).
type tickerProvider func() (<-chan time.Time, func())

// subscriber abstracts the output subscriber of Negotiator.
type subscriber func(ctx context.Context, slot int64, topic string, labels []string) error

// NewNegotiatorForT returns a new negotiator with a test ticker.
func NewNegotiatorForT(_ *testing.T, tcpNode host.Host, sendFunc p2p.SendReceiveFunc,
	consensus Consensus, msgProvider msgProvider, msgValidator msgValidator,
	consensusTimeout time.Duration, tickerProvider tickerProvider,
) *Negotiator {
	return newInternal(tcpNode, sendFunc, consensus, msgProvider, msgValidator, consensusTimeout, tickerProvider)
}

// NewNegotiator returns a new negotiator.
func NewNegotiator(tcpNode host.Host, sendFunc p2p.SendReceiveFunc, consensus Consensus,
	msgProvider msgProvider, msgValidator msgValidator, consensusTimeout time.Duration,
) *Negotiator {
	// Tick 10 times per timeout period.
	tickerProvider := func() (<-chan time.Time, func()) {
		ticker := time.NewTicker(consensusTimeout / 10)
		return ticker.C, ticker.Stop
	}

	return newInternal(tcpNode, sendFunc, consensus, msgProvider,
		msgValidator, consensusTimeout, tickerProvider)
}

// newInternal returns a new negotiator. This is for internal use only.
func newInternal(tcpNode host.Host, sendFunc p2p.SendReceiveFunc,
	consensus Consensus, msgProvider msgProvider, msgValidator msgValidator,
	consensusTimeout time.Duration, tickerProvider tickerProvider,
) *Negotiator {
	n := &Negotiator{
		tcpNode:          tcpNode,
		sendFunc:         sendFunc,
		consensus:        consensus,
		msgProvider:      msgProvider,
		msgValidator:     msgValidator,
		consensusTimeout: consensusTimeout,
		tickerProvider:   tickerProvider,
		subs:             make(map[string][]subscriber),
		trigger:          make(chan int64, 1), // Buffer a single trigger
	}

	// Wire consensus output to Negotiator subscribers.
	consensus.Subscribe(func(ctx context.Context, slot int64, result *pbv1.NegotiateResult) error {
		for _, topic := range result.Topics {
			for _, sub := range n.subs[topic.Topic] {
				if err := sub(ctx, slot, topic.Topic, topic.Labels); err != nil {
					return err
				}
			}
		}

		return nil
	})

	// Register negotiator protocol handler.
	tcpNode.SetStreamHandler(protocolID, func(stream network.Stream) {
		ctx := log.WithTopic(context.Background(), "negotiate")
		err := n.handleStream(stream)
		if err != nil {
			log.Error(ctx, "Handle negotiate message", err)
		}
	})

	return n
}

// Negotiator negotiates cluster wide features/versions/protocols.
type Negotiator struct {
	quit             chan struct{}
	trigger          chan int64
	received         chan *pbv1.NegotiateMsg
	minRequired      int
	consensusTimeout time.Duration
	tcpNode          host.Host
	peers            []peer.ID
	sendFunc         p2p.SendReceiveFunc
	consensus        Consensus
	msgProvider      msgProvider
	msgValidator     msgValidator
	tickerProvider   tickerProvider
	subs             map[string][]subscriber
}

// Subscribe registers a negotiator output subscriber function.
// This is not thread safe and MUST NOT be called after Run.
func (n *Negotiator) Subscribe(topic string, fn subscriber) {
	n.subs[topic] = append(n.subs[topic], fn)
}

// Negotiate starts a new negotiation round for the provided slot.
func (n *Negotiator) Negotiate(slot int64) {
	select {
	case n.trigger <- slot:
	case <-n.quit:
	}
}

// Run runs the negotiator until the context is cancelled.
// Note this will panic if called multiple times.
func (n *Negotiator) Run(ctx context.Context) error {
	defer close(n.quit)

	ticker, stopTicker := n.tickerProvider()
	defer stopTicker()

	var (
		msgs          = make(map[int64]map[peer.ID]*pbv1.NegotiateMsg)
		timeouts      = make(map[int64]time.Time)
		completedSlot int64
	)

	startConsensus := func(slot int64) {
		results := calculateResults(msgs[slot], n.minRequired)

		result := &pbv1.NegotiateResult{Topics: results}
		for _, msg := range msgs[slot] {
			result.Msgs = append(result.Msgs, msg)
		}

		n.consensus.Propose(ctx, slot, result)

		completedSlot = slot
		delete(msgs, slot)
		delete(timeouts, slot)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case slot := <-n.trigger:
			if slot <= completedSlot {
				continue // Ignore triggers for completed slots.
			}

			err := n.negotiateOnce(ctx, slot)
			if err != nil {
				log.Warn(ctx, "Negotiate error", err)
			}

		case msg := <-n.received:
			if msg.Slot <= completedSlot {
				continue // Ignore messages for completed slots.
			}

			if err := n.msgValidator(msg); err != nil {
				log.Warn(ctx, "Invalid negotiate message from peer", err, z.Str("peer", p2p.PeerName(peer.ID(msg.PeerId))))
				continue
			}

			slotMsgs, ok := msgs[msg.Slot]
			if !ok {
				slotMsgs = make(map[peer.ID]*pbv1.NegotiateMsg)
				timeouts[msg.Slot] = time.Now().Add(n.consensusTimeout)
			}
			slotMsgs[peer.ID(msg.PeerId)] = msg
			msgs[msg.Slot] = slotMsgs

			if len(slotMsgs) == len(n.peers) {
				// All messages received before timeout
				startConsensus(msg.Slot)
			}
		case now := <-ticker:
			for slot, timeout := range timeouts {
				if now.Before(timeout) {
					continue
				}

				startConsensus(slot) // Note that iterating and deleting from a map from a single goroutine seems ok.
			}
		}
	}
}

// handleStream handles a negotiation message exchange initiated by a peer.
func (n *Negotiator) handleStream(stream network.Stream) error {
	defer stream.Close()

	b, err := io.ReadAll(stream)
	if err != nil {
		return errors.Wrap(err, "read stream")
	}

	msg := new(pbv1.NegotiateMsg)
	if err := proto.Unmarshal(b, msg); err != nil {
		return errors.Wrap(err, "unmarshal proto")
	}

	select {
	case n.received <- msg:
	case <-n.quit:
	}

	response, err := n.msgProvider(msg.Slot)
	if err != nil {
		return err
	}

	b, err = proto.Marshal(response)
	if err != nil {
		return errors.Wrap(err, "marshal proto")
	}

	if _, err := stream.Write(b); err != nil {
		return errors.Wrap(err, "write response")
	}

	return nil
}

// negotiateOnce initiates a negotiation message exchange with all peers.
func (n *Negotiator) negotiateOnce(ctx context.Context, slot int64) error {
	msg, err := n.msgProvider(slot)
	if err != nil {
		return err
	}

	// Send our own status message first to start consensus timeout.
	n.received <- msg

	for _, pID := range n.peers {
		if pID == n.tcpNode.ID() {
			// Do not send to self
			continue
		}

		go func(pID peer.ID) {
			response := new(pbv1.NegotiateMsg)
			err := n.sendFunc(ctx, n.tcpNode, pID, msg, response, protocolID)
			// Ignore errors, since p2p sender will log and filter them
			if err == nil {
				n.received <- msg
			}
		}(pID)
	}

	return nil
}

// calculateResults is a deterministic function that returns the minimum
// ordered label set per topic. Labels are included in the result if minimum
// required peers include them and are ordered by number of peers then by overall priority.
//
// Peer messages need to validated beforehand such that
// they do not contain duplicate peers, they contain identical slots,
// individual peers may not contain duplicate topics, individual topics
// may not contain duplicate labels or more than 1000 labels.
func calculateResults(msgs map[peer.ID]*pbv1.NegotiateMsg, minRequired int) []*pbv1.NegotiateTopic {
	// Shuffle peers deterministicly by ID.
	var (
		pIDs []peer.ID
		slot int64
	)
	for pID, msg := range msgs {
		if slot == 0 {
			slot = msg.Slot
		} else if msg.Slot != slot {
			panic("mismatching slots")
		}
		pIDs = append(pIDs, pID)
	}
	//nolint:gosec // Math rand used for deterministic behaviour.
	rand.New(rand.NewSource(slot)).Shuffle(len(pIDs), func(i, j int) {
		pIDs[i], pIDs[j] = pIDs[j], pIDs[i]
	})

	// Group all label sets by topic
	var (
		labelSetsByTopic = make(map[string][][]string)
		dedupPeers       = newDeduper[peer.ID]("peer") // Peers may not be duplicated
	)
	for _, pID := range pIDs {
		dedupPeers(pID)

		dedupTopics := newDeduper[string]("topic") // Peers may not include duplicates.
		for _, topic := range msgs[pID].Topics {
			dedupTopics(topic.Topic)

			labelSetsByTopic[topic.Topic] = append(labelSetsByTopic[topic.Topic], topic.Labels)
		}
	}

	const countWeight = 1000 // Weighs count this much higher than priority

	// Calculate resulting minimum prioritised label set per topic.
	var results []*pbv1.NegotiateTopic
	for topic, labelSet := range labelSetsByTopic {
		if len(labelSet) < minRequired {
			// Shortcut since min require cannot be met.
			continue
		}

		// Calculate a overall score per unique label in the topic
		var (
			scores     = make(map[string]int)
			uniqLabels []string
		)
		for _, labels := range labelSet {
			if len(labels) >= countWeight {
				panic("max labels reached")
			}

			dedupLabels := newDeduper[string]("label") // Topics may not include duplicates labels.
			for prio, label := range labels {
				dedupLabels(label)

				if _, ok := scores[label]; !ok {
					uniqLabels = append(uniqLabels, label)
				}
				scores[label] += countWeight - prio // Equivalent to ordering by count then by priority
			}
		}

		// Order by score decreasing
		sort.Slice(uniqLabels, func(i, j int) bool {
			return scores[uniqLabels[i]] > scores[uniqLabels[j]]
		})

		// Extract scores with min required count.
		result := &pbv1.NegotiateTopic{Topic: topic}
		for _, label := range uniqLabels {
			if scores[label] <= ((minRequired - 1) * countWeight) {
				continue
			}
			result.Labels = append(result.Labels, label)
		}

		results = append(results, result)
	}

	return results
}

// newDeduper returns a new generic named deduplicater.
func newDeduper[T comparable](name string) func(t T) {
	duplicates := make(map[T]bool)

	return func(t T) {
		if duplicates[t] {
			panic("duplicate " + name)
		}
		duplicates[t] = true
	}
}
