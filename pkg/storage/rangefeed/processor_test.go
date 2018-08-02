// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package rangefeed

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

func makeLogicalOp(val interface{}) enginepb.MVCCLogicalOp {
	var op enginepb.MVCCLogicalOp
	if !op.SetValue(val) {
		panic(fmt.Sprintf("unknown logical mvcc op: %v", val))
	}
	return op
}

func writeValueOpWithKV(key roachpb.Key, ts hlc.Timestamp, val []byte) enginepb.MVCCLogicalOp {
	return makeLogicalOp(&enginepb.MVCCWriteValueOp{
		Key:       key,
		Timestamp: ts,
		Value:     val,
	})
}

func writeValueOp(ts hlc.Timestamp) enginepb.MVCCLogicalOp {
	return writeValueOpWithKV(nil /* key */, ts, nil /* val */)
}

func writeIntentOp(txnID uuid.UUID, ts hlc.Timestamp) enginepb.MVCCLogicalOp {
	return makeLogicalOp(&enginepb.MVCCWriteIntentOp{
		TxnID:     txnID,
		Timestamp: ts,
	})
}

func updateIntentOp(txnID uuid.UUID, ts hlc.Timestamp) enginepb.MVCCLogicalOp {
	return makeLogicalOp(&enginepb.MVCCUpdateIntentOp{
		TxnID:     txnID,
		Timestamp: ts,
	})
}

func commitIntentOpWithKV(
	txnID uuid.UUID, key roachpb.Key, ts hlc.Timestamp, val []byte,
) enginepb.MVCCLogicalOp {
	return makeLogicalOp(&enginepb.MVCCCommitIntentOp{
		TxnID:     txnID,
		Key:       key,
		Timestamp: ts,
		Value:     val,
	})
}

func commitIntentOp(txnID uuid.UUID, ts hlc.Timestamp) enginepb.MVCCLogicalOp {
	return commitIntentOpWithKV(txnID, nil /* key */, ts, nil /* val */)
}

func abortIntentOp(txnID uuid.UUID) enginepb.MVCCLogicalOp {
	return makeLogicalOp(&enginepb.MVCCAbortIntentOp{
		TxnID: txnID,
	})
}

func makeRangeFeedEvent(val interface{}) *roachpb.RangeFeedEvent {
	var event roachpb.RangeFeedEvent
	if !event.SetValue(val) {
		panic(fmt.Sprintf("unknown rangefeed event: %v", val))
	}
	return &event
}

func rangeFeedValue(key roachpb.Key, val roachpb.Value) *roachpb.RangeFeedEvent {
	return makeRangeFeedEvent(&roachpb.RangeFeedValue{
		Key:   key,
		Value: val,
	})
}

func rangeFeedCheckpoint(span roachpb.Span, ts hlc.Timestamp) *roachpb.RangeFeedEvent {
	return makeRangeFeedEvent(&roachpb.RangeFeedCheckpoint{
		Span:       span,
		ResolvedTS: ts,
	})
}

func newTestProcessor() (*Processor, *stop.Stopper) {
	stopper := stop.NewStopper()
	p := NewProcessor(Config{
		AmbientContext:       log.AmbientContext{Tracer: tracing.NewTracer()},
		Clock:                hlc.NewClock(hlc.UnixNano, time.Nanosecond),
		Span:                 roachpb.RSpan{Key: roachpb.RKeyMin, EndKey: roachpb.RKeyMax},
		EventChanCap:         16,
		PushIntentsInterval:  0, // disable
		CheckStreamsInterval: 10 * time.Millisecond,
	})
	p.Start(stopper)
	return p, stopper
}

func TestProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	p, stopper := newTestProcessor()
	defer stopper.Stop(context.Background())

	// Test processor without registrations.
	require.Equal(t, 0, p.Len())
	require.NotPanics(t, func() { p.ConsumeLogicalOps() })
	require.NotPanics(t, func() { p.ConsumeLogicalOps([]enginepb.MVCCLogicalOp{}...) })
	require.NotPanics(t, func() {
		txn1, txn2 := uuid.MakeV4(), uuid.MakeV4()
		p.ConsumeLogicalOps(
			writeValueOp(hlc.Timestamp{WallTime: 1}),
			writeIntentOp(txn1, hlc.Timestamp{WallTime: 2}),
			updateIntentOp(txn1, hlc.Timestamp{WallTime: 3}),
			commitIntentOp(txn1, hlc.Timestamp{WallTime: 4}),
			writeIntentOp(txn2, hlc.Timestamp{WallTime: 5}),
			abortIntentOp(txn2),
		)
		p.syncEventC()
		require.Equal(t, 0, p.rts.intentQ.Len())
	})
	require.NotPanics(t, func() { p.ForwardClosedTS(hlc.Timestamp{}) })
	require.NotPanics(t, func() { p.ForwardClosedTS(hlc.Timestamp{WallTime: 1}) })

	// Add a registration.
	r1Stream := newTestStream()
	r1ErrC := make(chan *roachpb.Error, 1)
	p.Register(
		roachpb.RSpan{Key: roachpb.RKey("a"), EndKey: roachpb.RKey("m")},
		hlc.Timestamp{WallTime: 1},
		r1Stream,
		r1ErrC,
	)
	require.Equal(t, 1, p.Len())

	// Test checkpoint with one registration.
	p.ForwardClosedTS(hlc.Timestamp{WallTime: 5})
	p.syncEventC()
	require.Equal(t,
		[]*roachpb.RangeFeedEvent{rangeFeedCheckpoint(
			roachpb.Span{Key: roachpb.KeyMin, EndKey: roachpb.KeyMax},
			hlc.Timestamp{WallTime: 5},
		)},
		r1Stream.Events(),
	)

	// Test value with one registration.
	p.ConsumeLogicalOps(
		writeValueOpWithKV(roachpb.Key("c"), hlc.Timestamp{WallTime: 6}, []byte("val")),
	)
	p.syncEventC()
	require.Equal(t,
		[]*roachpb.RangeFeedEvent{rangeFeedValue(
			roachpb.Key("c"),
			roachpb.Value{
				RawBytes:  []byte("val"),
				Timestamp: hlc.Timestamp{WallTime: 6},
			},
		)},
		r1Stream.Events(),
	)

	// Test value to non-overlapping key with one registration.
	p.ConsumeLogicalOps(
		writeValueOpWithKV(roachpb.Key("s"), hlc.Timestamp{WallTime: 6}, []byte("val")),
	)
	p.syncEventC()
	require.Equal(t, []*roachpb.RangeFeedEvent(nil), r1Stream.Events())

	// Test intent that is aborted with one registration.
	txn1 := uuid.MakeV4()
	// Write intent.
	p.ConsumeLogicalOps(writeIntentOp(txn1, hlc.Timestamp{WallTime: 6}))
	p.syncEventC()
	require.Equal(t, []*roachpb.RangeFeedEvent(nil), r1Stream.Events())
	// Abort.
	p.ConsumeLogicalOps(abortIntentOp(txn1))
	p.syncEventC()
	require.Equal(t, []*roachpb.RangeFeedEvent(nil), r1Stream.Events())
	require.Equal(t, 0, p.rts.intentQ.Len())

	// Test intent that is committed with one registration.
	txn2 := uuid.MakeV4()
	// Write intent.
	p.ConsumeLogicalOps(writeIntentOp(txn2, hlc.Timestamp{WallTime: 10}))
	p.syncEventC()
	require.Equal(t, []*roachpb.RangeFeedEvent(nil), r1Stream.Events())
	// Forward closed timestamp. Should now be stuck on intent.
	p.ForwardClosedTS(hlc.Timestamp{WallTime: 15})
	p.syncEventC()
	require.Equal(t,
		[]*roachpb.RangeFeedEvent{rangeFeedCheckpoint(
			roachpb.Span{Key: roachpb.KeyMin, EndKey: roachpb.KeyMax},
			hlc.Timestamp{WallTime: 9},
		)},
		r1Stream.Events(),
	)
	// Update the intent. Should forward resolved timestamp.
	p.ConsumeLogicalOps(updateIntentOp(txn2, hlc.Timestamp{WallTime: 12}))
	p.syncEventC()
	require.Equal(t,
		[]*roachpb.RangeFeedEvent{rangeFeedCheckpoint(
			roachpb.Span{Key: roachpb.KeyMin, EndKey: roachpb.KeyMax},
			hlc.Timestamp{WallTime: 11},
		)},
		r1Stream.Events(),
	)
	// Commit intent. Should forward resolved timestamp to closed timestamp.
	p.ConsumeLogicalOps(
		commitIntentOpWithKV(txn2, roachpb.Key("e"), hlc.Timestamp{WallTime: 13}, []byte("ival")),
	)
	p.syncEventC()
	require.Equal(t,
		[]*roachpb.RangeFeedEvent{
			rangeFeedValue(
				roachpb.Key("e"),
				roachpb.Value{
					RawBytes:  []byte("ival"),
					Timestamp: hlc.Timestamp{WallTime: 13},
				},
			),
			rangeFeedCheckpoint(
				roachpb.Span{Key: roachpb.KeyMin, EndKey: roachpb.KeyMax},
				hlc.Timestamp{WallTime: 15},
			),
		},
		r1Stream.Events(),
	)

	// Add another registration.
	r2Stream := newTestStream()
	r2ErrC := make(chan *roachpb.Error, 1)
	p.Register(
		roachpb.RSpan{Key: roachpb.RKey("c"), EndKey: roachpb.RKey("z")},
		hlc.Timestamp{WallTime: 1},
		r2Stream,
		r2ErrC,
	)
	require.Equal(t, 2, p.Len())

	// Both registrations should see checkpoint.
	p.ForwardClosedTS(hlc.Timestamp{WallTime: 20})
	p.syncEventC()
	chEvent := []*roachpb.RangeFeedEvent{rangeFeedCheckpoint(
		roachpb.Span{Key: roachpb.KeyMin, EndKey: roachpb.KeyMax},
		hlc.Timestamp{WallTime: 20},
	)}
	require.Equal(t, chEvent, r1Stream.Events())
	require.Equal(t, chEvent, r2Stream.Events())

	// Test value with two registration that overlaps both.
	p.ConsumeLogicalOps(
		writeValueOpWithKV(roachpb.Key("k"), hlc.Timestamp{WallTime: 22}, []byte("val2")),
	)
	p.syncEventC()
	valEvent := []*roachpb.RangeFeedEvent{rangeFeedValue(
		roachpb.Key("k"),
		roachpb.Value{
			RawBytes:  []byte("val2"),
			Timestamp: hlc.Timestamp{WallTime: 22},
		},
	)}
	require.Equal(t, valEvent, r1Stream.Events())
	require.Equal(t, valEvent, r2Stream.Events())

	// Test value that only overlaps the second registration.
	p.ConsumeLogicalOps(
		writeValueOpWithKV(roachpb.Key("v"), hlc.Timestamp{WallTime: 23}, []byte("val3")),
	)
	p.syncEventC()
	valEvent2 := []*roachpb.RangeFeedEvent{rangeFeedValue(
		roachpb.Key("v"),
		roachpb.Value{
			RawBytes:  []byte("val3"),
			Timestamp: hlc.Timestamp{WallTime: 23},
		},
	)}
	require.Equal(t, []*roachpb.RangeFeedEvent(nil), r1Stream.Events())
	require.Equal(t, valEvent2, r2Stream.Events())

	// Cancel the first registration.
	r1Stream.Cancel()
	require.NotNil(t, <-r1ErrC)

	// Stop the processor with an error.
	pErr := roachpb.NewErrorf("stop err")
	p.StopWithErr(pErr)
	require.NotNil(t, <-r2ErrC)
}

func TestNilProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var p *Processor

	// All of the following should be no-ops.
	require.Equal(t, 0, p.Len())
	require.NotPanics(t, func() { p.Stop() })
	require.NotPanics(t, func() { p.StopWithErr(nil) })
	require.NotPanics(t, func() { p.ConsumeLogicalOps() })
	require.NotPanics(t, func() { p.ConsumeLogicalOps(make([]enginepb.MVCCLogicalOp, 5)...) })
	require.NotPanics(t, func() { p.ForwardClosedTS(hlc.Timestamp{}) })
	require.NotPanics(t, func() { p.ForwardClosedTS(hlc.Timestamp{WallTime: 1}) })

	// The following should panic because they are not safe
	// to call on a nil Processor.
	require.Panics(t, func() { p.Start(stop.NewStopper()) })
	require.Panics(t, func() { p.Register(roachpb.RSpan{}, hlc.Timestamp{}, nil, nil) })
}

// TestProcessorConcurrentStop tests that all methods in Processor's API
// correctly handle the processor concurrently shutting down. If they did
// not then it would be possible for them to deadlock.
func TestProcessorConcurrentStop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const trials = 10
	for i := 0; i < trials; i++ {
		p, stopper := newTestProcessor()

		var wg sync.WaitGroup
		wg.Add(6)
		go func() {
			defer wg.Done()
			runtime.Gosched()
			s := newTestStream()
			errC := make(chan<- *roachpb.Error, 1)
			p.Register(p.Span, hlc.Timestamp{}, s, errC)
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			p.Len()
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			p.ConsumeLogicalOps(
				writeValueOpWithKV(roachpb.Key("s"), hlc.Timestamp{WallTime: 6}, []byte("val")),
			)
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			p.ForwardClosedTS(hlc.Timestamp{WallTime: 2})
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			p.Stop()
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			stopper.Stop(context.Background())
		}()
		wg.Wait()
	}
}
