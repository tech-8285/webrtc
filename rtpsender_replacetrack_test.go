// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package webrtc

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/transport/v5/test"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Integration coverage for RTPSender.ReplaceTrack.
//
// The assertions spell out which sender state is negotiated once and therefore
// survives a replacement, and which state is derived from the newly attached
// track and therefore rebound:
//
//   - Preserved: the RTP stream identity (SSRC, RTX/FEC SSRC, sender binding ID,
//     negotiated header extensions and the negotiated codec list) and the SDP.
//   - Rebound: the payload type and the whole packetization state (sequence
//     numbers, RTP timestamps and clock rate) follow the track that is attached
//     now; nothing is transplanted from the outgoing track.
//   - Failed replacement: the previously attached, sendable track stays bound and
//     keeps emitting, and the returned error stays attributable via errors.Is.

// errReplaceTestUnbind is returned by a track that refuses to Unbind.
var errReplaceTestUnbind = errors.New("replace test: unbind refused")

const (
	// replaceTestH264Fmtp is the H264 profile the default media engine negotiates.
	replaceTestH264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"
	// replaceTestSubnet is the virtual network the pair runs on.
	replaceTestSubnet = "1.2.3.0/24"
)

// replaceBindSnapshot copies a TrackLocalContext at Bind time. ReplaceTrack
// reuses and mutates the stored context object, so values read later would not
// describe the bind that actually happened.
type replaceBindSnapshot struct {
	codecs           []RTPCodecParameters
	headerExtensions []RTPHeaderExtensionParameter
	bound            RTPCodecParameters
	ssrc             SSRC
	ssrcRTX          SSRC
	ssrcFEC          SSRC
	id               string
}

func snapshotBindContext(ctx TrackLocalContext) replaceBindSnapshot {
	return replaceBindSnapshot{
		codecs:           append([]RTPCodecParameters{}, ctx.CodecParameters()...),
		headerExtensions: append([]RTPHeaderExtensionParameter{}, ctx.HeaderExtensions()...),
		ssrc:             ctx.SSRC(),
		ssrcRTX:          ctx.SSRCRetransmission(),
		ssrcFEC:          ctx.SSRCForwardErrorCorrection(),
		id:               ctx.ID(),
	}
}

// replaceTestTrack is a TrackLocalStaticSample that records every Bind and
// Unbind and can be told to reject one direction of the pair.
type replaceTestTrack struct {
	*TrackLocalStaticSample

	rejectBind   bool
	rejectUnbind bool

	mu       sync.Mutex
	snapshot *replaceBindSnapshot
	binds    int
	unbinds  int
}

func newReplaceTestTrack(t *testing.T, capability RTPCodecCapability, id, streamID string,
	options ...func(*TrackLocalStaticRTP),
) *replaceTestTrack {
	t.Helper()

	track, err := NewTrackLocalStaticSample(capability, id, streamID, options...)
	require.NoError(t, err)

	return &replaceTestTrack{TrackLocalStaticSample: track}
}

func (r *replaceTestTrack) Bind(ctx TrackLocalContext) (RTPCodecParameters, error) {
	r.mu.Lock()
	r.binds++
	reject := r.rejectBind
	r.mu.Unlock()

	if reject {
		return RTPCodecParameters{}, ErrUnsupportedCodec
	}

	codec, err := r.TrackLocalStaticSample.Bind(ctx)
	if err != nil {
		return codec, err
	}

	snapshot := snapshotBindContext(ctx)
	snapshot.bound = codec
	r.mu.Lock()
	r.snapshot = &snapshot
	r.mu.Unlock()

	return codec, nil
}

func (r *replaceTestTrack) Unbind(ctx TrackLocalContext) error {
	r.mu.Lock()
	r.unbinds++
	reject := r.rejectUnbind
	r.mu.Unlock()

	if reject {
		return errReplaceTestUnbind
	}

	return r.TrackLocalStaticSample.Unbind(ctx)
}

func (r *replaceTestTrack) setRejectBind(reject bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectBind = reject
}

func (r *replaceTestTrack) setRejectUnbind(reject bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectUnbind = reject
}

func (r *replaceTestTrack) bindCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.binds
}

func (r *replaceTestTrack) unbindCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.unbinds
}

// lastSnapshot returns the context recorded by the most recent successful Bind
// and whether a Bind has succeeded at all.
func (r *replaceTestTrack) lastSnapshot() (replaceBindSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.snapshot == nil {
		return replaceBindSnapshot{}, false
	}

	return *r.snapshot, true
}

// bindingCount reports how many live bindings the underlying static track holds.
func (r *replaceTestTrack) bindingCount() int {
	staticRTP := r.TrackLocalStaticSample.rtpTrack
	staticRTP.mu.RLock()
	defer staticRTP.mu.RUnlock()

	return len(staticRTP.bindings)
}

// observedRTP is one RTP packet as seen by the receiving PeerConnection.
type observedRTP struct {
	payloadType  PayloadType
	ssrc         SSRC
	sequence     uint16
	timestamp    uint32
	codecMime    string
	payloadBytes []byte
}

func (o observedRTP) String() string {
	return fmt.Sprintf("pt=%d ssrc=%d seq=%d ts=%d codec=%s payload=%x",
		o.payloadType, o.ssrc, o.sequence, o.timestamp, o.codecMime, o.payloadBytes)
}

// tailByte returns the last payload byte, which the tests use as a marker for
// which sample produced the packet. Codec payloaders prepend their own headers,
// so the full payload is not the raw sample.
func (o observedRTP) tailByte() byte {
	if len(o.payloadBytes) == 0 {
		return 0
	}

	return o.payloadBytes[len(o.payloadBytes)-1]
}

// sortedHeaderExtensions returns the negotiated extensions ordered by ID so that
// the media engine's map iteration order cannot make the comparison flaky.
func sortedHeaderExtensions(extensions []RTPHeaderExtensionParameter) []RTPHeaderExtensionParameter {
	sorted := append([]RTPHeaderExtensionParameter{}, extensions...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}

		return sorted[i].URI < sorted[j].URI
	})

	return sorted
}

// replaceTrackPair is a connected sender/answerer pair on a lossless virtual
// network with SRTP/SRTCP replay protection disabled.
//
// Replay protection has to be off because the tests deliberately reuse the same
// SSRC while moving sequence numbers (and therefore RTP packet indices) between
// tracks. With replay protection on, the receiver silently drops those packets
// and a legitimate replacement looks like a stall.
type replaceTrackPair struct {
	sender   *PeerConnection
	answerer *PeerConnection

	mu      sync.Mutex
	tracks  map[SSRC]chan observedRTP
	onTrack chan *TrackRemote
}

// newReplaceTrackPair builds a connected-pair helper. The returned close
// function must be deferred by the caller so that every PeerConnection and the
// router are torn down before the routine checker runs; t.Cleanup runs too late
// for that check.
func newReplaceTrackPair(t *testing.T) (*replaceTrackPair, func()) {
	t.Helper()

	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          replaceTestSubnet,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)

	newPeerConnection := func(ip string) *PeerConnection {
		net, netErr := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{ip}})
		require.NoError(t, netErr)
		require.NoError(t, wan.AddNet(net))

		settings := SettingEngine{}
		settings.SetNet(net)
		settings.SetICETimeouts(time.Second, time.Second, 200*time.Millisecond)
		settings.DisableSRTPReplayProtection(true)
		settings.DisableSRTCPReplayProtection(true)
		pc, pcErr := NewAPI(WithSettingEngine(settings)).NewPeerConnection(Configuration{})
		require.NoError(t, pcErr)

		return pc
	}

	pair := &replaceTrackPair{
		sender:   newPeerConnection("1.2.3.4"),
		answerer: newPeerConnection("1.2.3.5"),
		tracks:   map[SSRC]chan observedRTP{},
		onTrack:  make(chan *TrackRemote, 16),
	}

	require.NoError(t, wan.Start())

	pair.answerer.OnTrack(func(track *TrackRemote, _ *RTPReceiver) {
		pair.onTrack <- track

		for {
			packet, _, readErr := track.ReadRTP()
			if readErr != nil {
				return
			}

			observed := observedRTP{
				payloadType:  PayloadType(packet.PayloadType),
				ssrc:         SSRC(packet.SSRC),
				sequence:     packet.SequenceNumber,
				timestamp:    packet.Timestamp,
				codecMime:    track.Codec().MimeType,
				payloadBytes: append([]byte{}, packet.Payload...),
			}

			pair.mu.Lock()
			sink := pair.tracks[observed.ssrc]
			pair.mu.Unlock()

			if sink != nil {
				select {
				case sink <- observed:
				default:
				}
			}
		}
	})

	closePair := func() {
		assert.NoError(t, pair.sender.Close())
		assert.NoError(t, pair.answerer.Close())
		assert.NoError(t, wan.Stop())
	}

	return pair, closePair
}

// connect signals the pair and waits until both sides report connected.
func (p *replaceTrackPair) connect(t *testing.T) {
	t.Helper()

	connected := untilConnectionState(PeerConnectionStateConnected, p.sender, p.answerer)
	require.NoError(t, signalPair(p.sender, p.answerer))

	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		assert.FailNow(t, "peer connections never connected")
	}
}

// awaitSSRC blocks until the answerer reports the track for ssrc.
func (p *replaceTrackPair) awaitSSRC(t *testing.T, ssrc SSRC) *TrackRemote {
	t.Helper()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case track := <-p.onTrack:
			if track.SSRC() != ssrc {
				continue
			}

			p.sinkFor(ssrc)

			return track
		case <-deadline:
			assert.FailNow(t, "answerer never reported the track", "ssrc=%d", ssrc)

			return nil
		}
	}
}

// sinkFor returns the channel that receives the RTP observed for ssrc, creating
// it on first use so tests can subscribe before or after OnTrack fires.
func (p *replaceTrackPair) sinkFor(ssrc SSRC) chan observedRTP {
	p.mu.Lock()
	defer p.mu.Unlock()

	sink, ok := p.tracks[ssrc]
	if !ok {
		sink = make(chan observedRTP, 256)
		p.tracks[ssrc] = sink
	}

	return sink
}

// replaceSampleWriter is the sample-level write surface shared by the tracks
// under test.
type replaceSampleWriter interface {
	WriteSample(media.Sample) error
}

// writeUntilObserved keeps writing samples until one packet arrives, the write
// path reports an error, or the deadline passes.
func writeUntilObserved(
	t *testing.T,
	track replaceSampleWriter,
	sink <-chan observedRTP,
	data []byte,
) (observedRTP, bool) {
	t.Helper()

	deadline := time.After(10 * time.Second)
	for {
		if err := track.WriteSample(media.Sample{Data: data, Duration: time.Second}); err != nil {
			t.Logf("WriteSample reported %v", err)

			return observedRTP{}, false
		}

		select {
		case observed := <-sink:
			return observed, true
		case <-deadline:
			return observedRTP{}, false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// expectSilence asserts that no packet arrives within a bounded window.
func expectSilence(t *testing.T, track replaceSampleWriter, sink <-chan observedRTP, writeData []byte) {
	t.Helper()

	for range 5 {
		assert.NoError(t, track.WriteSample(media.Sample{Data: writeData, Duration: time.Second}))
	}

	select {
	case observed := <-sink:
		assert.Fail(t, "unexpected RTP arrived", observed.String())
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRTPSender_ReplaceTrack_PreservesSSRCAndRebindsPayloadization covers a
// replacement where the two tracks have different packetizers and different
// negotiated payload types.
func TestRTPSender_ReplaceTrack_PreservesSSRCAndRebindsPayloadization(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	// The sequences and timestamps are fixed so the test can tell preserved
	// negotiation state apart from per-track packetization state.
	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(1000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video", "pion",
		WithRTPSequenceNumber(7000), WithRTPTimestamp(900000))

	transceiver, err := pair.sender.AddTransceiverFromTrack(trackA)
	require.NoError(t, err)

	assert.NoError(t, transceiver.SetCodecPreferences([]RTPCodecParameters{
		{RTPCodecCapability: RTPCodecCapability{MimeType: MimeTypeVP8}, PayloadType: 54},
		{
			RTPCodecCapability: RTPCodecCapability{
				MimeType:    MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: replaceTestH264Fmtp,
			},
			PayloadType: 48,
		},
	}))

	rtpSender := transceiver.Sender()

	pair.connect(t)

	parametersBefore := rtpSender.GetParameters()
	require.Len(t, parametersBefore.Encodings, 1)
	ssrcBefore := parametersBefore.Encodings[0].SSRC
	rtxBefore := parametersBefore.Encodings[0].RTX.SSRC
	sdpBefore := pair.sender.LocalDescription().SDP
	require.False(t, pair.sender.checkNegotiationNeeded(), "negotiation must already be complete")

	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	firstA, ok := writeUntilObserved(t, trackA, sink, []byte{0x11, 0x22})
	require.True(t, ok, "the original track must reach the wire")
	assert.Equal(t, PayloadType(54), firstA.payloadType)
	assert.Equal(t, MimeTypeVP8, firstA.codecMime)
	assert.Equal(t, uint16(1000), firstA.sequence)
	assert.Equal(t, uint32(0), firstA.timestamp)

	// The sender has exactly one live binding for the attached track.
	assert.Equal(t, 1, trackA.bindingCount())
	assert.Equal(t, 0, trackB.bindingCount())

	require.NoError(t, rtpSender.ReplaceTrack(trackB))

	// Negotiated state is untouched: same SSRC/RTX SSRC, same codec list, same SDP,
	// and no renegotiation is scheduled.
	parametersAfter := rtpSender.GetParameters()
	assert.Equal(t, ssrcBefore, parametersAfter.Encodings[0].SSRC, "SSRC must be retained")
	assert.Equal(t, rtxBefore, parametersAfter.Encodings[0].RTX.SSRC, "RTX SSRC must be retained")
	assert.Equal(t, parametersBefore.Codecs, parametersAfter.Codecs, "negotiated codecs must be retained")
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())

	// The old binding is gone, the new one exists exactly once.
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 1, trackB.bindingCount())
	assert.Equal(t, 1, trackA.unbindCount(), "the outgoing track must be unbound exactly once")
	assert.Same(t, TrackLocal(trackB), rtpSender.Track())

	// Packetization is rebound: B supplies its own sequence numbers, timestamps
	// and payloader, while the SSRC stays the negotiated one.
	firstB, ok := writeUntilObserved(t, trackB, sink, []byte{0x65, 0x01, 0x02})
	require.True(t, ok, "the replacement track must reach the wire")
	assert.Equal(t, ssrcBefore, firstB.ssrc, "SSRC must be retained on the wire")
	assert.Equal(t, PayloadType(48), firstB.payloadType, "payload type must follow the new track")
	assert.Equal(t, MimeTypeH264, firstB.codecMime)
	assert.Equal(t, uint16(7000), firstB.sequence, "sequence numbers must come from the new track")
	assert.Equal(t, uint32(900000), firstB.timestamp, "timestamps must come from the new track")
	assert.Equal(t, []byte{0x65, 0x01, 0x02}, firstB.payloadBytes,
		"the H264 payloader passes the sample through, so the payload is the new track's bytes")

	// Replace back to A. The binding and the SSRC survive, and A resumes its own
	// packetization at the next sequence number it would have used.
	require.NoError(t, rtpSender.ReplaceTrack(trackA))
	assert.Equal(t, 1, trackA.bindingCount())
	assert.Equal(t, 0, trackB.bindingCount())

	backA, ok := writeUntilObserved(t, trackA, sink, []byte{0x33, 0x44})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, backA.ssrc)
	assert.Equal(t, PayloadType(54), backA.payloadType)
	assert.Equal(t, firstA.sequence+1, backA.sequence, "returning to a track resumes its own sequencer")

	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())

	// The sender exposes one encoding with the negotiated payload type the whole
	// time, regardless of which track is attached.
	assert.Equal(t, ssrcBefore, rtpSender.GetParameters().Encodings[0].SSRC)
}

// TestRTPSender_ReplaceTrack_PauseAndResume covers ReplaceTrack(nil) followed by
// re-attaching a track, with an existing negotiation in force.
func TestRTPSender_ReplaceTrack_PauseAndResume(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(2000), WithRTPTimestamp(1000))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video", "pion",
		WithRTPSequenceNumber(9000), WithRTPTimestamp(500000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)

	pair.connect(t)

	sdpBefore := pair.sender.LocalDescription().SDP
	parametersBefore := rtpSender.GetParameters()
	ssrcBefore := parametersBefore.Encodings[0].SSRC
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	first, ok := writeUntilObserved(t, trackA, sink, []byte{0x01, 0x02})
	require.True(t, ok)
	assert.Equal(t, uint16(2000), first.sequence)

	// Pause: the sender is detached from the track and releases its binding, but
	// the negotiated stream identity stays valid and no renegotiation is queued.
	require.NoError(t, rtpSender.ReplaceTrack(nil))
	assert.Nil(t, rtpSender.Track())
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 1, trackA.unbindCount())
	assert.False(t, pair.sender.checkNegotiationNeeded(), "pausing must not require renegotiation")
	assert.Equal(t, ssrcBefore, rtpSender.GetParameters().Encodings[0].SSRC)
	assert.Equal(t, parametersBefore.Codecs, rtpSender.GetParameters().Codecs)
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)

	// While paused, writing to the detached track must not reach the wire.
	expectSilence(t, trackA, sink, []byte{0x03, 0x04})

	// Resume with the same track: the original SSRC is rebound and packetization
	// continues from where the track left off.
	require.NoError(t, rtpSender.ReplaceTrack(trackA))
	assert.Same(t, TrackLocal(trackA), rtpSender.Track())
	assert.Equal(t, 1, trackA.bindingCount())
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)

	resumed, ok := writeUntilObserved(t, trackA, sink, []byte{0x05, 0x06})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, resumed.ssrc)
	assert.Equal(t, first.payloadType, resumed.payloadType)

	// The packetizer belongs to the track, not to the sender: the samples written
	// while paused advanced it even though nothing was transmitted.
	const pausedSampleWrites = 5
	assert.Equal(t, first.sequence+pausedSampleWrites+1, resumed.sequence,
		"the track's packetizer keeps advancing while detached")
	assert.Greater(t, resumed.sequence, first.sequence)

	// Pause again, then resume with a different track. The stream identity is
	// still the negotiated one, while the payload type and packetizer come from B.
	require.NoError(t, rtpSender.ReplaceTrack(nil))
	expectSilence(t, trackA, sink, []byte{0x07, 0x08})

	require.NoError(t, rtpSender.ReplaceTrack(trackB))
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 1, trackB.bindingCount())

	swapped, ok := writeUntilObserved(t, trackB, sink, []byte{0x65, 0x01})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, swapped.ssrc, "the negotiated SSRC survives the pause")
	assert.Equal(t, uint16(9000), swapped.sequence, "the new track brings its own sequencer")
	assert.Equal(t, uint32(500000), swapped.timestamp)
	assert.NotEqual(t, resumed.payloadType, swapped.payloadType)
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())
}

// TestRTPSender_ReplaceTrack_RejectedCodecKeepsOriginalSending covers the common
// failure: the replacement track's codec was not negotiated.
func TestRTPSender_ReplaceTrack_RejectedCodecKeepsOriginalSending(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(3000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP9}, "video", "pion",
		WithRTPSequenceNumber(6000))

	transceiver, err := pair.sender.AddTransceiverFromTrack(trackA)
	require.NoError(t, err)

	// Only VP8 is negotiated with the answerer, so VP9 cannot be sent.
	assert.NoError(t, transceiver.SetCodecPreferences([]RTPCodecParameters{
		{RTPCodecCapability: RTPCodecCapability{MimeType: MimeTypeVP8}, PayloadType: 54},
	}))

	rtpSender := transceiver.Sender()
	pair.connect(t)

	sdpBefore := pair.sender.LocalDescription().SDP
	parametersBefore := rtpSender.GetParameters()
	ssrcBefore := parametersBefore.Encodings[0].SSRC
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	before, ok := writeUntilObserved(t, trackA, sink, []byte{0x11})
	require.True(t, ok)

	replaceErr := rtpSender.ReplaceTrack(trackB)
	require.Error(t, replaceErr)
	assert.True(t, errors.Is(replaceErr, ErrUnsupportedCodec),
		"the failure must stay attributable, got %v", replaceErr)
	assert.False(t, errors.Is(replaceErr, ErrNoPayloaderForCodec),
		"the error must describe the negotiation failure, not packetization")

	// The original track is still the sender's track, still bound exactly once,
	// and the rejected track was never attached.
	assert.Same(t, TrackLocal(trackA), rtpSender.Track())
	assert.Equal(t, 1, trackA.bindingCount())
	assert.Equal(t, 0, trackB.bindingCount())
	assert.Equal(t, 1, trackA.unbindCount(), "the original track is unbound once and rebound")
	assert.Equal(t, 2, trackA.bindCount(), "the original track must be rebound after the rollback")

	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())
	assert.Equal(t, ssrcBefore, rtpSender.GetParameters().Encodings[0].SSRC)
	assert.Equal(t, parametersBefore.Codecs, rtpSender.GetParameters().Codecs)

	// Media continuity: the original track keeps emitting on the unchanged SSRC,
	// continuing its own sequence numbers.
	after, ok := writeUntilObserved(t, trackA, sink, []byte{0x22})
	require.True(t, ok, "the original track must still send after a failed replacement")
	assert.Equal(t, ssrcBefore, after.ssrc)
	assert.Equal(t, before.sequence+1, after.sequence)
	assert.Equal(t, before.payloadType, after.payloadType)
	assert.Equal(t, byte(0x22), after.tailByte(),
		"the packet must still carry the original track's sample")

	// The rejected track cannot reach the wire in any form, including the RTP
	// path that bypasses the sample packetizer.
	expectSilence(t, trackB, sink, []byte{0x65, 0x01})
	assert.NoError(t, trackB.rtpTrack.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 4242, SSRC: uint32(ssrcBefore)},
		Payload: []byte{0x65, 0x01},
	}))
	select {
	case observed := <-sink:
		assert.Fail(t, "the rejected track published through a leaked binding", observed.String())
	case <-time.After(300 * time.Millisecond):
	}

	// Retrying with a codec that is negotiated still works, so the sender is not
	// stuck in a half-bound state.
	recovered := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(9500), WithRTPTimestamp(12345))
	require.NoError(t, rtpSender.ReplaceTrack(recovered))
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 1, recovered.bindingCount())

	recoveredPacket, ok := writeUntilObserved(t, recovered, sink, []byte{0x77})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, recoveredPacket.ssrc)
	assert.Equal(t, uint16(9500), recoveredPacket.sequence)
	assert.Equal(t, uint32(12345), recoveredPacket.timestamp)
	assert.Equal(t, byte(0x77), recoveredPacket.tailByte())
}

// TestRTPSender_ReplaceTrack_BindFailureKeepsOriginalSending covers a
// replacement track that rejects its own Bind and must not disturb the sender.
func TestRTPSender_ReplaceTrack_BindFailureKeepsOriginalSending(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(4000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(8000))
	trackB.setRejectBind(true)

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)
	pair.connect(t)

	ssrcBefore := rtpSender.GetParameters().Encodings[0].SSRC
	sdpBefore := pair.sender.LocalDescription().SDP
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	before, ok := writeUntilObserved(t, trackA, sink, []byte{0x11})
	require.True(t, ok)

	replaceErr := rtpSender.ReplaceTrack(trackB)
	require.Error(t, replaceErr)
	assert.True(t, errors.Is(replaceErr, ErrUnsupportedCodec))

	assert.Same(t, TrackLocal(trackA), rtpSender.Track())
	assert.Equal(t, 1, trackA.bindingCount(), "the original track must be rebound after the rollback")
	assert.Equal(t, 0, trackB.bindingCount(), "a rejected Bind must not leave a binding behind")
	assert.Equal(t, 1, trackB.bindCount())

	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())

	after, ok := writeUntilObserved(t, trackA, sink, []byte{0x22})
	require.True(t, ok, "the original track must keep sending")
	assert.Equal(t, before.sequence+1, after.sequence)
	assert.Equal(t, before.payloadType, after.payloadType)

	// Repeated failures must not accumulate state on the rejected track.
	for range 3 {
		require.Error(t, rtpSender.ReplaceTrack(trackB))
	}
	assert.Equal(t, 0, trackB.bindingCount(), "failed attempts must not accumulate bindings")
	assert.Equal(t, 1, trackA.bindingCount())

	third, ok := writeUntilObserved(t, trackA, sink, []byte{0x33})
	require.True(t, ok)
	assert.Equal(t, after.sequence+1, third.sequence)
}

// TestRTPSender_ReplaceTrack_UnbindFailureKeepsOriginalSending covers a failure
// that happens before any rebinding: the outgoing track refuses to Unbind, so the
// sender must report the error and leave the outgoing track attached and live.
func TestRTPSender_ReplaceTrack_UnbindFailureKeepsOriginalSending(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(5000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video", "pion",
		WithRTPSequenceNumber(11000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)
	pair.connect(t)

	ssrcBefore := rtpSender.GetParameters().Encodings[0].SSRC
	sdpBefore := pair.sender.LocalDescription().SDP
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	before, ok := writeUntilObserved(t, trackA, sink, []byte{0x11})
	require.True(t, ok)

	trackA.setRejectUnbind(true)
	replaceErr := rtpSender.ReplaceTrack(trackB)
	require.Error(t, replaceErr)
	assert.True(t, errors.Is(replaceErr, errReplaceTestUnbind),
		"the error must be the track's own error, got %v", replaceErr)

	// The switch is abandoned before the new track is bound.
	assert.Same(t, TrackLocal(trackA), rtpSender.Track())
	assert.Equal(t, 1, trackA.bindingCount())
	assert.Equal(t, 0, trackB.bindingCount())
	assert.Equal(t, 0, trackB.bindCount(), "the new track must not be bound when the old one cannot be released")
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)
	assert.False(t, pair.sender.checkNegotiationNeeded())

	// The original track was never released, so it keeps sending.
	trackA.setRejectUnbind(false)
	after, ok := writeUntilObserved(t, trackA, sink, []byte{0x22})
	require.True(t, ok, "the original track must keep sending after the abandoned switch")
	assert.Equal(t, before.sequence+1, after.sequence)
	assert.Equal(t, before.payloadType, after.payloadType)
	assert.Equal(t, ssrcBefore, after.ssrc)

	// Once the track allows Unbind, the replacement succeeds normally.
	require.NoError(t, rtpSender.ReplaceTrack(trackB))
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 1, trackB.bindingCount())

	swapped, ok := writeUntilObserved(t, trackB, sink, []byte{0x65, 0x01})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, swapped.ssrc)
	assert.Equal(t, uint16(11000), swapped.sequence)
}

// TestRTPSender_ReplaceTrack_DifferentTrackIdentityKeepsNegotiatedStream covers a
// replacement whose MSID differs from the negotiated one. The RTP stream stays the
// negotiated one; only the MSID bookkeeping changes, and that is a local change.
func TestRTPSender_ReplaceTrack_DifferentTrackIdentityKeepsNegotiatedStream(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video-a", "stream-a",
		WithRTPSequenceNumber(12000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video-b", "stream-b",
		WithRTPSequenceNumber(15000), WithRTPTimestamp(90000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)
	pair.connect(t)

	ssrcBefore := rtpSender.GetParameters().Encodings[0].SSRC
	sdpBefore := pair.sender.LocalDescription().SDP
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	before, ok := writeUntilObserved(t, trackA, sink, []byte{0x11, 0x22})
	require.True(t, ok)

	require.NoError(t, rtpSender.ReplaceTrack(trackB))

	after, ok := writeUntilObserved(t, trackB, sink, []byte{0x65, 0x01})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, after.ssrc, "the negotiated SSRC must be retained even for a new MSID")
	assert.NotEqual(t, before.payloadType, after.payloadType, "the payload type follows the new track")
	assert.Equal(t, uint16(15000), after.sequence)

	// The SDP is not rewritten, so the negotiated stream identity on the wire is
	// still the one the answerer agreed to.
	assert.Equal(t, sdpBefore, pair.sender.LocalDescription().SDP)

	// The answerer keeps reporting the negotiated MSID, because the identity in
	// the SDP was never renegotiated.
	assert.Equal(t, "stream-a video-a", pair.awaitSSRC(t, ssrcBefore).Msid())
}

// TestRTPSender_ReplaceTrack_PreservesNegotiatedHeaderExtensions pins the binding
// identity handed to the new track — binding ID, SSRCs and negotiated header
// extensions — while showing that the codec is re-selected for the new track.
func TestRTPSender_ReplaceTrack_PreservesNegotiatedHeaderExtensions(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(13000))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video", "pion",
		WithRTPSequenceNumber(16000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)
	pair.connect(t)

	snapshotA, ok := trackA.lastSnapshot()
	require.True(t, ok)
	require.NotEmpty(t, snapshotA.headerExtensions, "the sender must have negotiated header extensions")
	require.NotEqual(t, SSRC(0), snapshotA.ssrc)

	sink := pair.sinkFor(snapshotA.ssrc)
	require.NotNil(t, sink)
	_, ok = writeUntilObserved(t, trackA, sink, []byte{0x11})
	require.True(t, ok)

	require.NoError(t, rtpSender.ReplaceTrack(trackB))

	snapshotB, ok := trackB.lastSnapshot()
	require.True(t, ok)

	// The replacement is bound through the sender's stored context, so it sees the
	// very same binding ID, SSRCs and negotiated extensions.
	assert.Equal(t, snapshotA.id, snapshotB.id, "the sender binding ID must be reused")
	assert.Equal(t, snapshotA.ssrc, snapshotB.ssrc, "the SSRC must be reused")
	assert.Equal(t, snapshotA.ssrcRTX, snapshotB.ssrcRTX, "the RTX SSRC must be reused")
	assert.Equal(t, snapshotA.ssrcFEC, snapshotB.ssrcFEC, "the FEC SSRC must be reused")
	assert.Equal(t, sortedHeaderExtensions(snapshotA.headerExtensions),
		sortedHeaderExtensions(snapshotB.headerExtensions),
		"negotiated header extensions must be preserved")

	// The negotiated list is handed to the new track unchanged, and the track picks
	// the codec it needs out of it.
	assert.Equal(t, snapshotA.codecs, snapshotB.codecs,
		"the negotiated codec list must be presented unchanged")
	assert.Equal(t, MimeTypeH264, snapshotB.bound.MimeType)
	assert.Equal(t, PayloadType(102), snapshotB.bound.PayloadType,
		"the new track must select its own codec from the negotiated list")
	assert.Equal(t, MimeTypeVP8, snapshotA.bound.MimeType)
	assert.Equal(t, PayloadType(96), snapshotA.bound.PayloadType)
}

// TestRTPSender_ReplaceTrack_AudioClockRateChange checks that a replacement across
// codecs with different clock rates rebinds the packetizer rather than carrying
// timestamps over.
func TestRTPSender_ReplaceTrack_AudioClockRateChange(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "pion", WithRTPSequenceNumber(17000), WithRTPTimestamp(48000))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypePCMU, ClockRate: 8000},
		"audio", "pion", WithRTPSequenceNumber(19000), WithRTPTimestamp(16000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)
	pair.connect(t)

	parametersBefore := rtpSender.GetParameters()
	ssrcBefore := parametersBefore.Encodings[0].SSRC
	sink := pair.sinkFor(ssrcBefore)
	require.NotNil(t, sink)

	first, ok := writeUntilObserved(t, trackA, sink, []byte{0xAA})
	require.True(t, ok)
	assert.Equal(t, MimeTypeOpus, first.codecMime)
	assert.Equal(t, uint32(48000), first.timestamp)

	require.NoError(t, rtpSender.ReplaceTrack(trackB))

	parametersAfter := rtpSender.GetParameters()
	assert.Equal(t, ssrcBefore, parametersAfter.Encodings[0].SSRC)
	assert.Equal(t, parametersBefore.Codecs, parametersAfter.Codecs,
		"both audio codecs were negotiated, so the negotiated list is unchanged")

	second, ok := writeUntilObserved(t, trackB, sink, []byte{0xBB})
	require.True(t, ok)
	assert.Equal(t, ssrcBefore, second.ssrc)
	assert.Equal(t, MimeTypePCMU, second.codecMime)
	assert.Equal(t, uint16(19000), second.sequence)
	assert.Equal(t, uint32(16000), second.timestamp,
		"the new clock rate must start from the new track's timestamp, not the old one")
	assert.NotEqual(t, first.payloadType, second.payloadType)
	assert.False(t, pair.sender.checkNegotiationNeeded())
}

// TestRTPSender_ReplaceTrack_BeforeNegotiation replaces the track before the
// sender is bound. No binding exists yet, so the switch is purely bookkeeping and
// the negotiated stream must be built from the track that is attached at the time
// negotiation completes.
func TestRTPSender_ReplaceTrack_BeforeNegotiation(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pair, closePair := newReplaceTrackPair(t)
	defer closePair()

	trackA := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "pion",
		WithRTPSequenceNumber(21000), WithRTPTimestamp(0))
	trackB := newReplaceTestTrack(t, RTPCodecCapability{MimeType: MimeTypeH264}, "video", "pion",
		WithRTPSequenceNumber(24000), WithRTPTimestamp(90000))

	rtpSender, err := pair.sender.AddTrack(trackA)
	require.NoError(t, err)

	// Nothing has been bound yet.
	assert.Equal(t, 0, trackA.bindingCount())
	assert.Equal(t, 0, trackA.bindCount())

	require.NoError(t, rtpSender.ReplaceTrack(trackB))
	assert.Same(t, TrackLocal(trackB), rtpSender.Track())
	assert.Equal(t, 0, trackA.bindCount(), "an unbound sender must not Bind the outgoing track")
	assert.Equal(t, 0, trackA.unbindCount(), "an unbound sender must not Unbind the outgoing track")

	pair.connect(t)

	// The negotiation that follows binds only the attached track.
	assert.Equal(t, 1, trackB.bindCount())
	assert.Equal(t, 0, trackA.bindCount())
	assert.Equal(t, 1, trackB.bindingCount())

	snapshotB, ok := trackB.lastSnapshot()
	require.True(t, ok)
	require.NotEqual(t, SSRC(0), snapshotB.ssrc)

	packet, ok := writeUntilObserved(t, trackB, pair.sinkFor(snapshotB.ssrc), []byte{0x65, 0x01})
	require.True(t, ok, "the track attached before negotiation never sent")
	assert.Equal(t, snapshotB.ssrc, packet.ssrc)
	assert.Equal(t, MimeTypeH264, packet.codecMime)
	assert.Equal(t, uint16(24000), packet.sequence)
	assert.Equal(t, uint32(90000), packet.timestamp)
}
