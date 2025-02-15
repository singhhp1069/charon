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

package qbft

import (
	"context"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/obolnetwork/charon/app/errors"
)

func TestQBFT(t *testing.T) {
	t.Run("happy 0", func(t *testing.T) {
		testQBFT(t, test{
			Instance:    0,
			StartDelay:  nil,
			DecideRound: 1,
		})
	})

	t.Run("happy 1", func(t *testing.T) {
		testQBFT(t, test{
			Instance:    1,
			StartDelay:  nil,
			DecideRound: 1,
		})
	})

	t.Run("leader late exp", func(t *testing.T) {
		testQBFT(t, test{
			Instance:    0,
			StartDelay:  map[int64]time.Duration{1: time.Second * 2},
			DecideRound: 2,
		})
	})

	t.Run("leader down const", func(t *testing.T) {
		testQBFT(t, test{
			Instance:    0,
			StartDelay:  map[int64]time.Duration{1: time.Second * 2},
			ConstPeriod: true,
			DecideRound: 2,
		})
	})

	t.Run("very late exp", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 3,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 5,
				2: time.Second * 10,
			},
			DecideRound: 4,
		})
	})

	t.Run("very late const", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 1,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 5,
				2: time.Second * 10,
			},
			ConstPeriod: true,
			RandomRound: true,
		})
	})

	t.Run("stagger start exp", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 0,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 0,
				2: time.Second * 1,
				3: time.Second * 2,
				4: time.Second * 3,
			},
			RandomRound: true, // Takes 1 or 2 rounds.
		})
	})

	t.Run("stagger start const", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 0,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 0,
				2: time.Second * 1,
				3: time.Second * 2,
				4: time.Second * 3,
			},
			ConstPeriod: true,
			RandomRound: true, // Takes 1 or 2 rounds.
		})
	})

	t.Run("500ms jitter exp", func(t *testing.T) {
		testQBFT(t, test{
			Instance:      3,
			BCastJitterMS: 500,
			RandomRound:   true,
		})
	})

	t.Run("200ms jitter const", func(t *testing.T) {
		testQBFT(t, test{
			Instance:      3,
			BCastJitterMS: 200, // 0.2-0.4s network delay * 3msgs/round == 0.6-1.2s delay per 1s round.
			ConstPeriod:   true,
			RandomRound:   true,
		})
	})

	t.Run("drop 10% const", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 1,
			DropProb: map[int64]float64{
				1: 0.1,
				2: 0.1,
				3: 0.1,
				4: 0.1,
			},
			ConstPeriod: true,
			RandomRound: true,
		})
	})

	t.Run("drop 30% const", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 1,
			DropProb: map[int64]float64{
				1: 0.3,
				2: 0.3,
				3: 0.3,
				4: 0.3,
			},
			ConstPeriod: true,
			RandomRound: true,
		})
	})

	t.Run("fuzz", func(t *testing.T) {
		testQBFT(t, test{
			Instance:    1,
			Fuzz:        true,
			ConstPeriod: true,
			DecideRound: 1,
		})
	})

	t.Run("fuzz with late leader", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 1,
			Fuzz:     true,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 2,
				2: time.Second * 2,
			},
			ConstPeriod: true,
			RandomRound: true,
		})
	})

	t.Run("fuzz with very late leader", func(t *testing.T) {
		testQBFT(t, test{
			Instance: 1,
			Fuzz:     true,
			StartDelay: map[int64]time.Duration{
				1: time.Second * 10,
				2: time.Second * 10,
			},
			ConstPeriod: true,
			RandomRound: true,
		})
	})
}

type test struct {
	Instance      int64                   // Consensus instance, only affects leader election.
	ConstPeriod   bool                    // ConstPeriod results in 1s round timeout, otherwise exponential (1s,2s,4s...)
	StartDelay    map[int64]time.Duration // Delays start of certain processes
	DropProb      map[int64]float64       // DropProb [0..1] probability of dropped messages per processes
	BCastJitterMS int                     // Add random delays to broadcast of messages.
	DecideRound   int                     // Deterministic consensus at specific round
	RandomRound   bool                    // Non-deterministic consensus at random round.
	Fuzz          bool                    // Enables fuzzing by Node 1.
}

//nolint:gocognit
func testQBFT(t *testing.T, test test) {
	t.Helper()

	const (
		n         = 4
		maxRound  = 50
		fifoLimit = 100
	)

	var (
		ctx, cancel = context.WithCancel(context.Background())
		clock       = new(fakeClock)
		receives    = make(map[int64]chan Msg[int64, int64])
		broadcast   = make(chan Msg[int64, int64])
		resultChan  = make(chan []Msg[int64, int64], n)
		errChan     = make(chan error, n)
	)
	defer cancel()

	defs := Definition[int64, int64]{
		IsLeader: makeIsLeader(n),
		NewTimer: func(round int64) (<-chan time.Time, func()) {
			d := time.Second
			if !test.ConstPeriod { // If not constant periods, then exponential.
				d = time.Duration(math.Pow(2, float64(round-1))) * time.Second
			}

			return clock.NewTimer(d)
		},
		Decide: func(_ context.Context, instance int64, value int64, qcommit []Msg[int64, int64]) {
			resultChan <- qcommit
		},
		LogRoundChange: func(ctx context.Context, instance int64, process, round, newRound int64, rule UponRule, msgs []Msg[int64, int64]) {
			t.Logf("%s %v@%d change to %d ~= %v", clock.NowStr(), process, round, newRound, rule)
		},
		LogUponRule: func(_ context.Context, instance int64, process, round int64, msg Msg[int64, int64], rule UponRule) {
			t.Logf("%s %d => %v@%d -> %v@%d ~= %v", clock.NowStr(), msg.Source(), msg.Type(), msg.Round(), process, round, rule)
			if round > maxRound {
				cancel()
			}
		},
		LogUnjust: func(_ context.Context, instance int64, process int64, msg Msg[int64, int64]) {
			if test.Fuzz {
				return // Ignore unjust messages when fuzzing.
			}
			t.Logf("Unjust: %#v", msg)
			cancel()
		},
		Nodes:     n,
		FIFOLimit: fifoLimit,
	}

	for i := int64(1); i <= n; i++ {
		receive := make(chan Msg[int64, int64], 1000)
		receives[i] = receive
		trans := Transport[int64, int64]{
			Broadcast: func(ctx context.Context, typ MsgType, instance int64, source int64, round int64, value int64,
				pr int64, pv int64, justify []Msg[int64, int64],
			) error {
				if round > maxRound {
					return errors.New("max round reach")
				}
				t.Logf("%s %v => %v@%d", clock.NowStr(), source, typ, round)
				msg := newMsg(typ, instance, source, round, value, pr, pv, justify)
				receive <- msg // Always send to self first (no jitter, no drops).
				bcast(t, broadcast, msg, test.BCastJitterMS, clock)

				return nil
			},
			Receive: receive,
		}

		go func(i int64) {
			if d, ok := test.StartDelay[i]; ok {
				t.Logf("%s Node %d start delay %s", clock.NowStr(), i, d)
				ch, _ := clock.NewTimer(d)
				<-ch
				t.Logf("%s Node %d starting %s", clock.NowStr(), i, d)

				// Drain any buffered messages
				for {
					select {
					case <-receive:
						continue
					default:
					}

					break
				}
			}

			err := Run(ctx, defs, trans, test.Instance, i, i)
			if err != nil {
				errChan <- err
				return
			}
		}(i)
	}

	if test.Fuzz {
		go fuzz(ctx, clock, broadcast, test.Instance, 1)
	}

	results := make(map[int64]Msg[int64, int64])
	var count int

	for {
		select {
		case msg := <-broadcast:
			for target, out := range receives {
				if target == msg.Source() {
					continue // Do not broadcast to self, we sent to self already.
				}
				if p, ok := test.DropProb[msg.Source()]; ok {
					if rand.Float64() < p {
						t.Logf("%s %v => %v@%d => %d (dropped)", clock.NowStr(), msg.Source(), msg.Type(), msg.Round(), target)
						continue // Drop
					}
				}
				out <- msg
				if rand.Float64() < 0.1 { // Send 10% messages twice
					out <- msg
				}
			}
		case qCommit := <-resultChan:
			for _, commit := range qCommit {
				// Ensure that all results are the same
				for _, previous := range results {
					require.EqualValues(t, previous.Value(), commit.Value())
				}
				if !test.RandomRound {
					require.EqualValues(t, test.DecideRound, commit.Round())
				}
				results[commit.Source()] = commit
			}

			count++
			if count == n {
				round := qCommit[0].Round()
				t.Logf("Got all results in round %d after %s: %#v", round, clock.SinceT0(), results)

				return
			}
		case err := <-errChan:
			require.Fail(t, err.Error())
		default:
			time.Sleep(time.Microsecond)
			clock.Advance(time.Millisecond * 1)
		}
	}
}

// fuzz broadcasts random messages from the peer every 100ms (10/round).
func fuzz(ctx context.Context, clock *fakeClock, broadcast chan Msg[int64, int64], instance, peerIdx int64) {
	for {
		timer, stop := clock.NewTimer(time.Millisecond * 100)
		select {
		case <-ctx.Done():
			return
		case <-timer:
			broadcast <- randomMsg(instance, peerIdx)
		}
		stop()
	}
}

func randomMsg(instance, peerIdx int64) msg {
	return msg{
		msgType:  1 + MsgType(rand.Intn(int(MsgDecided))),
		instance: instance,
		peerIdx:  peerIdx,
		round:    int64(rand.Intn(10)),
		value:    int64(rand.Intn(10)),
		pr:       int64(rand.Intn(10)),
		pv:       int64(rand.Intn(10)),
		justify:  nil,
	}
}

// bcast delays the message broadcast by between 1x and 2x jitterMS and drops messages.
func bcast(t *testing.T, broadcast chan Msg[int64, int64], msg Msg[int64, int64], jitterMS int, clock *fakeClock) {
	t.Helper()

	if jitterMS == 0 {
		broadcast <- msg
		return
	}

	go func() {
		deltaMS := int(float64(jitterMS) * rand.Float64())
		delay := time.Duration(jitterMS+deltaMS) * time.Millisecond
		t.Logf("%s %v => %v@%d (bcast delay %s)", clock.NowStr(), msg.Source(), msg.Type(), msg.Round(), delay)
		ch, _ := clock.NewTimer(delay)
		<-ch
		broadcast <- msg
	}()
}

// newMsg returns a new message to be broadcast.
func newMsg(typ MsgType, instance int64, source int64, round int64, value int64,
	pr int64, pv int64, justify []Msg[int64, int64],
) Msg[int64, int64] {
	var msgs []msg
	for _, j := range justify {
		m := j.(msg)
		m.justify = nil // Clear nested justifications.
		msgs = append(msgs, m)
	}

	return msg{
		msgType:  typ,
		instance: instance,
		peerIdx:  source,
		round:    round,
		value:    value,
		pr:       pr,
		pv:       pv,
		justify:  msgs,
	}
}

var _ Msg[int64, int64] = msg{}

type msg struct {
	msgType  MsgType
	instance int64
	peerIdx  int64
	round    int64
	value    int64
	pr       int64
	pv       int64
	justify  []msg
}

func (m msg) Type() MsgType {
	return m.msgType
}

func (m msg) Instance() int64 {
	return m.instance
}

func (m msg) Source() int64 {
	return m.peerIdx
}

func (m msg) Round() int64 {
	return m.round
}

func (m msg) Value() int64 {
	return m.value
}

func (m msg) PreparedRound() int64 {
	return m.pr
}

func (m msg) PreparedValue() int64 {
	return m.pv
}

func (m msg) Justification() []Msg[int64, int64] {
	var resp []Msg[int64, int64]
	for _, msg := range m.justify {
		resp = append(resp, msg)
	}

	return resp
}

func TestIsJustifiedPrePrepare(t *testing.T) {
	const (
		n        = 4
		instance = 1
	)

	// Preprepare with identical Pr but different Pvs.
	preprepare := msg{msgType: 1, instance: 1, peerIdx: 3, round: 6, value: 2, pr: 0, pv: 0, justify: []msg{
		{msgType: 4, instance: 1, peerIdx: 2, round: 6, value: 0, pr: 2, pv: 3},
		{msgType: 4, instance: 1, peerIdx: 3, round: 6, value: 0, pr: 2, pv: 3},
		{msgType: 4, instance: 1, peerIdx: 1, round: 6, value: 0, pr: 2, pv: 2},
		{msgType: 2, instance: 1, peerIdx: 3, round: 2, value: 2, pr: 0, pv: 0},
		{msgType: 2, instance: 1, peerIdx: 4, round: 2, value: 2, pr: 0, pv: 0},
		{msgType: 2, instance: 1, peerIdx: 1, round: 2, value: 2, pr: 0, pv: 0},
		{msgType: 2, instance: 1, peerIdx: 2, round: 2, value: 2, pr: 0, pv: 0},
	}}

	def := Definition[int64, int64]{
		IsLeader: makeIsLeader(n),
		Nodes:    n,
	}

	ok := isJustifiedPrePrepare[int64, int64](def, instance, preprepare)
	require.True(t, ok)
}

func TestFormulas(t *testing.T) {
	// assert given N asserts Q and F.
	assert := func(t *testing.T, n, q, f int) {
		t.Helper()
		d := Definition[any, int64]{Nodes: n}
		require.Equalf(t, q, d.Quorum(), "Quorum given N=%d", n)
		require.Equalf(t, f, d.Faulty(), "Faulty given N=%d", n)
	}

	assert(t, 1, 1, 0)
	assert(t, 2, 2, 0)
	assert(t, 3, 2, 0)
	assert(t, 4, 3, 1)
	assert(t, 5, 4, 1)
	assert(t, 6, 4, 1)
	assert(t, 7, 5, 2)
	assert(t, 8, 6, 2)
	assert(t, 9, 6, 2)
	assert(t, 10, 7, 3)
	assert(t, 11, 8, 3)
	assert(t, 12, 8, 3)
	assert(t, 13, 9, 4)
	assert(t, 15, 10, 4)
	assert(t, 17, 12, 5)
	assert(t, 19, 13, 6)
	assert(t, 21, 14, 6)
}

// makeIsLeader returns a leader election function.
func makeIsLeader(n int64) func(int64, int64, int64) bool {
	return func(instance int64, round int64, process int64) bool {
		return (instance+round)%n == process
	}
}
