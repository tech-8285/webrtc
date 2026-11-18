// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package webrtc

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedPacket is the per-binding view of a single written RTP packet.
type recordedPacket struct {
	ssrc        SSRC
	payloadType PayloadType
	sequence    uint16
	timestamp   uint32
}

// recordingWriter is a TrackLocalWriter that records every packet written to
// one binding. It can be configured to fail on demand so that partial send
// failures can be exercised without involving a real PeerConnection.
type recordingWriter struct {
	mu       sync.Mutex
	packets  []recordedPacket
	failNext error
}

func (w *recordingWriter) WriteRTP(header *rtp.Header, _ []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.failNext != nil {
		err := w.failNext
		w.failNext = nil

		return 0, err
	}

	w.packets = append(w.packets, recordedPacket{
		ssrc:        SSRC(header.SSRC),
		payloadType: PayloadType(header.PayloadType),
		sequence:    header.SequenceNumber,
		timestamp:   header.Timestamp,
	})

	return 0, nil
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	packet := &rtp.Packet{}
	if err := packet.Unmarshal(b); err != nil {
		return 0, err
	}

	return w.WriteRTP(&packet.Header, packet.Payload)
}

func (w *recordingWriter) snapshot() []recordedPacket {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]recordedPacket, len(w.packets))
	copy(out, w.packets)

	return out
}

// mockTrackLocalContext is a configurable TrackLocalContext so a sample track
// can be bound to several in-process receivers, each negotiating its own SSRC
// and payload type, without standing up PeerConnections.
type mockTrackLocalContext struct {
	id          string
	ssrc        SSRC
	payloadType PayloadType
	writer      TrackLocalWriter
}

func (m mockTrackLocalContext) ID() string               { return m.id }
func (m mockTrackLocalContext) SSRC() SSRC               { return m.ssrc }
func (m mockTrackLocalContext) SSRCRetransmission() SSRC { return 0 }
func (m mockTrackLocalContext) SSRCForwardErrorCorrection() SSRC {
	return 0
}
func (m mockTrackLocalContext) WriteStream() TrackLocalWriter { return m.writer }
func (m mockTrackLocalContext) HeaderExtensions() []RTPHeaderExtensionParameter {
	return nil
}
func (m mockTrackLocalContext) RTCPReader() interceptor.RTCPReader { return nil }
func (m mockTrackLocalContext) CodecParameters() []RTPCodecParameters {
	return []RTPCodecParameters{{
		RTPCodecCapability: RTPCodecCapability{
			MimeType:  MimeTypeVP8,
			ClockRate: 90000,
		},
		PayloadType: m.payloadType,
	}}
}

const (
	testInitialRTPTimestamp uint32 = 10000
	testInitialRTPSequence  uint16 = 500
	testClockRate                  = 90000
	testFrameDuration              = 20 * time.Millisecond
	// 20ms at 90kHz: 0.02 * 90000 = 1800 RTP timestamp ticks.
	testFrameTicks uint32 = 1800
)

// newBoundSampleTrack builds a TrackLocalStaticSample whose packetizer uses a
// fixed initial timestamp/sequence number and binds it to the given contexts.
func newBoundSampleTrack(t *testing.T, contexts ...mockTrackLocalContext) *TrackLocalStaticSample {
	t.Helper()

	track, err := NewTrackLocalStaticSample(
		RTPCodecCapability{MimeType: MimeTypeVP8, ClockRate: testClockRate},
		"video",
		"pion",
		WithRTPTimestamp(testInitialRTPTimestamp),
		WithRTPSequenceNumber(testInitialRTPSequence),
	)
	require.NoError(t, err)

	for _, ctx := range contexts {
		_, err = track.Bind(ctx)
		require.NoError(t, err)
	}

	// Bind lazily creates the packetizer on the first Bind; assert the fixed
	// sequencer/timestamp configuration actually took effect.
	track.rtpTrack.mu.RLock()
	require.NotNil(t, track.packetizer)
	require.NotNil(t, track.sequencer)
	track.rtpTrack.mu.RUnlock()

	return track
}

// rtpTimestampDiff returns a-b accounting for 32bit timestamp wraparound.
func rtpTimestampDiff(a, b uint32) uint32 { return a - b }

// writeTestFrame sends one small VP8 sample that fits in a single RTP packet.
func writeTestFrame(t *testing.T, track *TrackLocalStaticSample, dropped uint16) {
	t.Helper()

	err := track.WriteSample(media.Sample{
		Data:               []byte{0x10, 0x20, 0x30},
		Duration:           testFrameDuration,
		PrevDroppedPackets: dropped,
	})
	require.NoError(t, err)
}

// While the producer is paused (no WriteSample calls) RTP time must not move:
// timestamps are derived purely from accumulated sample durations, so an idle
// gap leaves the next packet on the exact timestamp the last packet had.
func TestTrackLocalStaticSample_PauseKeepsRTPTimestampFrozen(t *testing.T) {
	emitThreeFrames := func(paused bool) []recordedPacket {
		writer := &recordingWriter{}
		track := newBoundSampleTrack(t, mockTrackLocalContext{
			id:          "binding-1",
			ssrc:        0x1111,
			payloadType: 96,
			writer:      writer,
		})

		writeTestFrame(t, track, 0)
		writeTestFrame(t, track, 0)

		if paused {
			// Simulate a pause: the producer simply stops writing for a while.
			time.Sleep(30 * time.Millisecond)
		}

		writeTestFrame(t, track, 0)

		return writer.snapshot()
	}

	packets := emitThreeFrames(true)
	require.Len(t, packets, 3)

	// The control track emitted the same three frames back-to-back without any
	// wall-clock gap; its timestamps and sequence numbers must be identical.
	control := emitThreeFrames(false)
	require.Equal(t, control, packets)

	assert.Equal(t, testInitialRTPTimestamp, packets[0].timestamp)
	// Each emitted frame advances the shared clock by exactly one frame.
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(packets[1].timestamp, packets[0].timestamp))
	// The pause consumed wall-clock time but no media time, so the frame after
	// the pause is exactly one frame-tick after the previous frame, never more.
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(packets[2].timestamp, packets[1].timestamp))
	// Sequence numbers are contiguous: nothing was sent during the pause.
	assert.Equal(t, uint16(1), packets[1].sequence-packets[0].sequence)
	assert.Equal(t, uint16(1), packets[2].sequence-packets[1].sequence)
}

// After declared packet loss, both the lost media and the transmitted media
// each occupy one playout interval: the RTP timestamp jumps by the dropped
// interval first, then advances by the current sample's interval, while the
// RTP sequence number carries a gap of exactly PrevDroppedPackets.
func TestTrackLocalStaticSample_DroppedPacketsAdvanceTimeAndSequence(t *testing.T) {
	writer := &recordingWriter{}
	track := newBoundSampleTrack(t, mockTrackLocalContext{
		id:          "binding-1",
		ssrc:        0x2222,
		payloadType: 96,
		writer:      writer,
	})

	writeTestFrame(t, track, 0) // frame 1: establishes baseline

	const dropped uint16 = 2
	writeTestFrame(t, track, dropped) // frame 2: two frames were lost before it
	writeTestFrame(t, track, 0)       // frame 3: loss recovered, normal cadence

	packets := writer.snapshot()
	require.Len(t, packets, 3)

	// Frame 2's timestamp includes the two lost frames plus its own duration.
	expectedLostGap := testFrameTicks * (uint32(dropped) + 1)
	assert.Equal(t,
		expectedLostGap,
		rtpTimestampDiff(packets[1].timestamp, packets[0].timestamp),
		"lost and transmitted media must each consume one playout interval",
	)

	// Two sequence numbers were consumed by the dropped packets only.
	assert.Equal(t, uint16(dropped+1), packets[1].sequence-packets[0].sequence)

	// After recovery the cadence is back to a single frame interval.
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(packets[2].timestamp, packets[1].timestamp))
	assert.Equal(t, uint16(1), packets[2].sequence-packets[1].sequence)
}

// Declared packet loss must be reflected identically on every bound receiver's
// shared clock while each keeps its own negotiated identity: no receiver may
// see a smaller timestamp gap than the loss dictates or inherit another
// receiver's SSRC/payload type.
func TestTrackLocalStaticSample_DroppedPacketsAcrossMultipleBindings(t *testing.T) {
	writerOne := &recordingWriter{}
	writerTwo := &recordingWriter{}
	track := newBoundSampleTrack(t,
		mockTrackLocalContext{id: "binding-1", ssrc: 0x7777, payloadType: 96, writer: writerOne},
		mockTrackLocalContext{id: "binding-2", ssrc: 0x8888, payloadType: 97, writer: writerTwo},
	)

	writeTestFrame(t, track, 0) // frame 1: baseline

	const dropped uint16 = 3
	writeTestFrame(t, track, dropped) // frame 2: three lost frames before it
	writeTestFrame(t, track, 0)       // frame 3: recovered

	one := writerOne.snapshot()
	two := writerTwo.snapshot()
	require.Len(t, one, 3)
	require.Len(t, two, 3)

	expectedLostGap := testFrameTicks * (uint32(dropped) + 1)
	for i := range one {
		// The loss moves the single shared clock, so both receivers observe the
		// exact same timestamp and sequence number on every packet.
		assert.Equal(t, one[i].timestamp, two[i].timestamp)
		assert.Equal(t, one[i].sequence, two[i].sequence)

		// The media identity stays per-binding despite the shared timing.
		assert.Equal(t, SSRC(0x7777), one[i].ssrc)
		assert.Equal(t, PayloadType(96), one[i].payloadType)
		assert.Equal(t, SSRC(0x8888), two[i].ssrc)
		assert.Equal(t, PayloadType(97), two[i].payloadType)
	}

	assert.Equal(t, expectedLostGap, rtpTimestampDiff(one[1].timestamp, one[0].timestamp))
	assert.Equal(t, expectedLostGap, rtpTimestampDiff(two[1].timestamp, two[0].timestamp))
	assert.Equal(t, uint16(dropped+1), one[1].sequence-one[0].sequence)
	assert.Equal(t, uint16(dropped+1), two[1].sequence-two[0].sequence)

	// Recovery restores the normal stride on every receiver independently.
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(one[2].timestamp, one[1].timestamp))
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(two[2].timestamp, two[1].timestamp))
}

// Fractional tick remainders (durations that do not convert to a whole number
// of clock ticks) must be carried from one sample to the next instead of being
// rounded away or double-counted, keeping the RTP clock within one tick of the
// ideal real-valued media time.
func TestTrackLocalStaticSample_FractionalRemainderCarriedAcrossSamples(t *testing.T) {
	writer := &recordingWriter{}
	track := newBoundSampleTrack(t, mockTrackLocalContext{
		id:          "binding-1",
		ssrc:        0x3333,
		payloadType: 96,
		writer:      writer,
	})

	// 999us at 90kHz = 89.91 ticks per sample: no sample yields a whole number
	// of ticks, so the sub-tick fraction must be carried between samples
	// instead of being rounded away or double counted.
	oddDuration := 999 * time.Microsecond
	const samples = 10

	for range samples {
		require.NoError(t, track.WriteSample(media.Sample{Data: []byte{0x01}, Duration: oddDuration}))
	}

	packets := writer.snapshot()
	require.Len(t, packets, samples)

	// Use the clock rate the track actually negotiated during Bind rather than
	// assuming the test constant, so the expectation matches production state.
	track.rtpTrack.mu.RLock()
	clockRate := track.clockRate
	track.rtpTrack.mu.RUnlock()

	// Recompute the exact expectation with the same arithmetic WriteSample
	// uses: each sample computes floor(tickF + carried remainder) and rolls the
	// un-emitted fraction into the next sample. The packetizer stamps a packet
	// with the timestamp it currently holds and only then advances by the
	// sample's ticks, so packet n carries the ticks accumulated by the first
	// n-1 samples (the first packet therefore keeps the initial timestamp).
	var remainder float64
	perSampleTicks := make([]uint32, samples)
	for i := range samples {
		tickF := oddDuration.Seconds() * clockRate
		curTotal := tickF + remainder
		curTicks := uint32(curTotal)
		remainder = curTotal - float64(curTicks)
		perSampleTicks[i] = curTicks
	}

	var accumulatedBeforeLast uint32
	for i := 0; i < samples-1; i++ {
		accumulatedBeforeLast += perSampleTicks[i]
	}

	expected := testInitialRTPTimestamp + accumulatedBeforeLast
	got := packets[samples-1].timestamp
	assert.Equal(t, expected, got, "sub-tick remainders must be carried across samples")
	assert.Equal(t, testInitialRTPTimestamp, packets[0].timestamp,
		"the packetizer stamps before advancing, so the first packet keeps the initial timestamp")

	// Independent cross-check: the last packet's timestamp is the ideal media
	// time of the first n-1 samples (the phase offset), rounded down by the
	// carried fractions. Its error against that ideal value must be below one
	// tick, proving no fraction is ever rounded away or double counted.
	idealBeforeLast := oddDuration.Seconds() * clockRate * (samples - 1)
	drift := idealBeforeLast - float64(accumulatedBeforeLast)
	assert.GreaterOrEqual(t, drift, 0.0)
	assert.Less(t, drift, 1.0, "remainder handling must keep sub-tick error below one tick")

	// Every adjacent pair must advance by either 89 or 90 ticks, never zero or
	// a two-tick jump, proving the fraction propagates from sample to sample.
	for i := 1; i < samples; i++ {
		delta := rtpTimestampDiff(packets[i].timestamp, packets[i-1].timestamp)
		assert.Contains(t, []uint32{89, 90}, delta)
	}
}

// Every bound receiver must see its own negotiated SSRC and payload type,
// while sharing the identical RTP timestamp/sequence number stream: the media
// identity is rewritten per binding, the shared sampling clock is not.
func TestTrackLocalStaticSample_MultipleBindingsUseNegotiatedIdentity(t *testing.T) {
	writerOne := &recordingWriter{}
	writerTwo := &recordingWriter{}
	track := newBoundSampleTrack(t,
		mockTrackLocalContext{id: "binding-1", ssrc: 0xAAAA, payloadType: 96, writer: writerOne},
		mockTrackLocalContext{id: "binding-2", ssrc: 0xBBBB, payloadType: 97, writer: writerTwo},
	)

	writeTestFrame(t, track, 0)
	writeTestFrame(t, track, 0)

	one := writerOne.snapshot()
	two := writerTwo.snapshot()
	require.Len(t, one, 2)
	require.Len(t, two, 2)

	for i := range one {
		// Per-binding negotiated media identity.
		assert.Equal(t, SSRC(0xAAAA), one[i].ssrc)
		assert.Equal(t, PayloadType(96), one[i].payloadType)
		assert.Equal(t, SSRC(0xBBBB), two[i].ssrc)
		assert.Equal(t, PayloadType(97), two[i].payloadType)

		// Shared sampling time and sequence number space.
		assert.Equal(t, one[i].timestamp, two[i].timestamp)
		assert.Equal(t, one[i].sequence, two[i].sequence)
	}

	assert.Equal(t, testInitialRTPTimestamp, one[0].timestamp)
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(one[1].timestamp, one[0].timestamp))
	assert.Equal(t, testInitialRTPSequence, one[0].sequence)
	assert.Equal(t, testInitialRTPSequence+1, one[1].sequence)
}

// The shared RTP clock advances exactly once per WriteSample regardless of how
// many bindings receive the media: adding a second receiver must not double the
// timestamp increment compared with a single-receiver track.
func TestTrackLocalStaticSample_TimestampAdvanceIndependentOfBindingCount(t *testing.T) {
	const frameCount = 5

	buildAndSend := func(bindingCount int) []recordedPacket {
		contexts := make([]mockTrackLocalContext, 0, bindingCount)
		writers := make([]*recordingWriter, 0, bindingCount)

		for i := range bindingCount {
			writer := &recordingWriter{}
			writers = append(writers, writer)
			contexts = append(contexts, mockTrackLocalContext{
				id:          bindingID(i),
				ssrc:        SSRC(0x4000 + i),
				payloadType: PayloadType(96 + i),
				writer:      writer,
			})
		}

		track := newBoundSampleTrack(t, contexts...)
		for range frameCount {
			writeTestFrame(t, track, 0)
		}

		return writers[0].snapshot()
	}

	single := buildAndSend(1)
	multi := buildAndSend(3)
	require.Len(t, single, frameCount)
	require.Len(t, multi, frameCount)

	// Same starting point and same per-frame stride: binding count is irrelevant
	// to the shared sampling clock.
	assert.Equal(t, single[0].timestamp, multi[0].timestamp)
	assert.Equal(t, single[frameCount-1].timestamp, multi[frameCount-1].timestamp)

	for i := 1; i < frameCount; i++ {
		assert.Equal(t,
			testFrameTicks,
			rtpTimestampDiff(single[i].timestamp, single[i-1].timestamp),
		)
		assert.Equal(t,
			testFrameTicks,
			rtpTimestampDiff(multi[i].timestamp, multi[i-1].timestamp),
		)
	}
}

func bindingID(i int) string {
	return "binding-" + strconv.Itoa(i+1)
}

// A failing binding must neither stop the other bindings from receiving the
// media nor cause the shared clock to advance twice: packetization (and thus
// timestamp/sequence progression) happens once, before the per-binding fan-out.
func TestTrackLocalStaticSample_PartialWriteFailureAdvancesClockOnce(t *testing.T) {
	failingWriter := &recordingWriter{}
	healthyWriter := &recordingWriter{}
	track := newBoundSampleTrack(t,
		mockTrackLocalContext{id: "binding-fail", ssrc: 0xCCCC, payloadType: 96, writer: failingWriter},
		mockTrackLocalContext{id: "binding-ok", ssrc: 0xDDDD, payloadType: 97, writer: healthyWriter},
	)

	failingWriter.failNext = errWriteBoom

	err := track.WriteSample(media.Sample{Data: []byte{0x01}, Duration: testFrameDuration})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errWriteBoom))

	// The healthy binding still received the frame.
	healthy := healthyWriter.snapshot()
	require.Len(t, healthy, 1)
	assert.Equal(t, testInitialRTPTimestamp, healthy[0].timestamp)
	assert.Equal(t, SSRC(0xDDDD), healthy[0].ssrc)
	assert.Equal(t, PayloadType(97), healthy[0].payloadType)

	// The failed binding recorded nothing for the failed write.
	assert.Empty(t, failingWriter.snapshot())

	// Next frame advances the clock exactly once from the original baseline:
	// the failed fan-out did not re-packetize or re-advance the shared clock.
	writeTestFrame(t, track, 0)

	healthy = healthyWriter.snapshot()
	require.Len(t, healthy, 2)
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(healthy[1].timestamp, healthy[0].timestamp))
	assert.Equal(t, uint16(1), healthy[1].sequence-healthy[0].sequence)

	// The previously failing binding has recovered and joins at the current
	// clock position with the same shared timestamp.
	recovered := failingWriter.snapshot()
	require.Len(t, recovered, 1)
	assert.Equal(t, healthy[1].timestamp, recovered[0].timestamp)
	assert.Equal(t, healthy[1].sequence, recovered[0].sequence)
	assert.Equal(t, SSRC(0xCCCC), recovered[0].ssrc)
}

// When the set of bindings changes mid-stream, newly added and removed
// receivers must observe a coherent single clock: a late binding joins at the
// current timestamp position, and a removed binding simply stops receiving,
// while the remaining bindings keep a gap-free, correctly identified stream.
func TestTrackLocalStaticSample_BindingChangesPreserveClockAndIdentity(t *testing.T) {
	writerOne := &recordingWriter{}
	writerTwo := &recordingWriter{}
	track := newBoundSampleTrack(t,
		mockTrackLocalContext{id: "binding-1", ssrc: 0x1111, payloadType: 96, writer: writerOne},
	)

	writeTestFrame(t, track, 0)
	writeTestFrame(t, track, 0)

	// A second receiver binds while the stream is already running.
	lateContext := mockTrackLocalContext{id: "binding-2", ssrc: 0x2222, payloadType: 97, writer: writerTwo}
	_, err := track.Bind(lateContext)
	require.NoError(t, err)

	writeTestFrame(t, track, 0) // frame 3: both bindings receive it

	// The first receiver leaves mid-stream.
	require.NoError(t, track.Unbind(mockTrackLocalContext{id: "binding-1"}))

	writeTestFrame(t, track, 0) // frame 4: only the late binding receives it

	one := writerOne.snapshot()
	two := writerTwo.snapshot()
	require.Len(t, one, 3, "unbound receiver stops at frame 3")
	require.Len(t, two, 2, "late receiver joins at frame 3 and continues")

	// The late binding's first packet is exactly at the shared clock position
	// the ongoing stream had reached; its own negotiated identity is used.
	assert.Equal(t, one[2].timestamp, two[0].timestamp)
	assert.Equal(t, one[2].sequence, two[0].sequence)
	assert.Equal(t, SSRC(0x2222), two[0].ssrc)
	assert.Equal(t, PayloadType(97), two[0].payloadType)
	assert.Equal(t, SSRC(0x1111), one[2].ssrc)
	assert.Equal(t, PayloadType(96), one[2].payloadType)

	// The shared clock kept a single-frame stride across every change.
	for i := 1; i < len(one); i++ {
		assert.Equal(t, testFrameTicks, rtpTimestampDiff(one[i].timestamp, one[i-1].timestamp))
		assert.Equal(t, uint16(1), one[i].sequence-one[i-1].sequence)
	}
	assert.Equal(t, testFrameTicks, rtpTimestampDiff(two[1].timestamp, two[0].timestamp))
	assert.Equal(t, uint16(1), two[1].sequence-two[0].sequence)
}

// Unbind of an unknown context must fail and leave the existing bindings
// untouched so an erroneous teardown cannot silently redirect media.
func TestTrackLocalStaticSample_UnbindUnknownContextFails(t *testing.T) {
	writer := &recordingWriter{}
	track := newBoundSampleTrack(t, mockTrackLocalContext{
		id:          "binding-1",
		ssrc:        0x5555,
		payloadType: 96,
		writer:      writer,
	})

	err := track.Unbind(mockTrackLocalContext{id: "does-not-exist"})
	require.ErrorIs(t, err, ErrUnbindFailed)

	writeTestFrame(t, track, 0)
	packets := writer.snapshot()
	require.Len(t, packets, 1)
	assert.Equal(t, SSRC(0x5555), packets[0].ssrc)
	assert.Equal(t, testInitialRTPTimestamp, packets[0].timestamp)
}

// packetizeSpy counts packetization calls to prove the shared clock machinery
// runs once per WriteSample no matter how large the fan-out is.
type packetizeSpy struct {
	rtp.Packetizer
	mu               sync.Mutex
	packetizeCalls   int
	skipSamplesCalls int
	lastSamples      uint32
}

func (s *packetizeSpy) Packetize(payload []byte, samples uint32) []*rtp.Packet {
	s.mu.Lock()
	s.packetizeCalls++
	s.lastSamples = samples
	s.mu.Unlock()

	return s.Packetizer.Packetize(payload, samples)
}

func (s *packetizeSpy) SkipSamples(samples uint32) {
	s.mu.Lock()
	s.skipSamplesCalls++
	s.lastSamples = samples
	s.mu.Unlock()

	s.Packetizer.SkipSamples(samples)
}

// Packetize/SkipSamples must be invoked exactly once per WriteSample even with
// several bindings: timestamp progression is shared work, not per-receiver work.
func TestTrackLocalStaticSample_PacketizerRunsOncePerSample(t *testing.T) {
	contexts := make([]mockTrackLocalContext, 0, 3)
	for i := range 3 {
		contexts = append(contexts, mockTrackLocalContext{
			id:          bindingID(i),
			ssrc:        SSRC(0x6000 + i),
			payloadType: PayloadType(96 + i),
			writer:      &recordingWriter{},
		})
	}

	track := newBoundSampleTrack(t, contexts...)

	spy := &packetizeSpy{Packetizer: track.packetizer}
	track.rtpTrack.mu.Lock()
	track.packetizer = spy
	track.rtpTrack.mu.Unlock()

	for range 4 {
		writeTestFrame(t, track, 0)
	}

	// One dropped sample exercises SkipSamples alongside Packetize, each once.
	require.NoError(t, track.WriteSample(media.Sample{
		Data:               []byte{0x01},
		Duration:           testFrameDuration,
		PrevDroppedPackets: 1,
	}))

	assert.Equal(t, 5, spy.packetizeCalls)
	assert.Equal(t, 1, spy.skipSamplesCalls)
	assert.Equal(t, testFrameTicks, spy.lastSamples)
}
