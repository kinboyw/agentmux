package control

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"private/agentmux/internal/protocol"
	"private/agentmux/internal/ws"
)

type directTransportController struct {
	conn      *ws.Conn
	sessionID string
	streamID  string

	mu       sync.Mutex
	grantID  string
	peer     *webrtc.PeerConnection
	channel  *webrtc.DataChannel
	fallback *time.Timer
}

func newDirectTransportController(conn *ws.Conn, sessionID, streamID string) *directTransportController {
	return &directTransportController{conn: conn, sessionID: sessionID, streamID: streamID}
}

func (d *directTransportController) Start(grant protocol.P2PGrant) {
	grantID := strings.TrimSpace(grant.GrantID)
	if grantID == "" {
		return
	}
	slog.Default().Info("tui p2p start", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "ice_servers", len(grant.ICEServers), "fallback_after_ms", grant.FallbackAfterMs)
	d.mu.Lock()
	if d.grantID == grantID {
		d.mu.Unlock()
		return
	}
	d.grantID = grantID
	d.mu.Unlock()
	if err := d.createOffer(grantID); err != nil {
		slog.Default().Warn("tui p2p offer failed", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "error", err)
		d.send(protocol.P2PSignal{
			GrantID: grantID,
			From:    "control",
			To:      "worker",
			Signal:  "fallback",
			State:   protocol.TerminalChannelP2PFallback,
			Reason:  "tui_webrtc_offer_failed",
			Message: err.Error(),
		})
		return
	}
	fallbackAfter := time.Duration(grant.FallbackAfterMs) * time.Millisecond
	if fallbackAfter <= 0 {
		fallbackAfter = 10 * time.Second
	}
	if fallbackAfter > 30*time.Second {
		fallbackAfter = 30 * time.Second
	}
	d.mu.Lock()
	if d.fallback != nil {
		d.fallback.Stop()
	}
	d.fallback = time.AfterFunc(fallbackAfter, func() {
		slog.Default().Info("tui p2p fallback timeout", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "fallback_after_ms", fallbackAfter.Milliseconds())
		d.send(protocol.P2PSignal{
			GrantID: grantID,
			From:    "control",
			To:      "worker",
			Signal:  "fallback",
			State:   protocol.TerminalChannelP2PFallback,
			Reason:  "p2p_negotiation_timeout",
			Message: "TUI P2P negotiation timed out; continuing over Hub relay.",
		})
	})
	d.mu.Unlock()
}

func (d *directTransportController) AcceptSignal(signal protocol.P2PSignal) {
	slog.Default().Debug("tui p2p signal received", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID, "signal", signal.Signal, "state", signal.State, "reason", signal.Reason)
	switch signal.Signal {
	case "answer":
		d.acceptAnswer(signal)
	case "candidate":
		d.acceptCandidate(signal)
	case "direct", "fallback", "unsupported", "close":
		d.stopFallback()
	}
}

func (d *directTransportController) Close() {
	d.stopFallback()
	d.mu.Lock()
	channel := d.channel
	peer := d.peer
	d.channel = nil
	d.peer = nil
	d.grantID = ""
	d.mu.Unlock()
	if channel != nil {
		_ = channel.Close()
	}
	if peer != nil {
		_ = peer.Close()
	}
}

func (d *directTransportController) createOffer(grantID string) error {
	d.Close()
	d.mu.Lock()
	d.grantID = grantID
	d.mu.Unlock()
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	slog.Default().Info("tui p2p peer connection created", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID)
	channel, err := peer.CreateDataChannel("agentmux-terminal", nil)
	if err != nil {
		_ = peer.Close()
		return err
	}
	d.mu.Lock()
	d.peer = peer
	d.channel = channel
	d.mu.Unlock()
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		slog.Default().Debug("tui p2p local candidate gathered", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "candidate", init.Candidate, "sdp_mid", stringValue(init.SDPMid), "sdp_mline_index", init.SDPMLineIndex)
		d.send(protocol.P2PSignal{
			GrantID:       grantID,
			From:          "control",
			To:            "worker",
			Signal:        "candidate",
			State:         "negotiating",
			Candidate:     init.Candidate,
			SDPMid:        stringValue(init.SDPMid),
			SDPMLineIndex: init.SDPMLineIndex,
		})
	})
	peer.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		slog.Default().Info("tui p2p ice gathering state", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "state", state.String())
	})
	peer.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Default().Info("tui p2p ice connection state", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "state", state.String())
	})
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Default().Info("tui p2p peer connection state", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "state", state.String())
	})
	channel.OnOpen(func() {
		d.stopFallback()
		slog.Default().Info("tui p2p data channel open", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID)
		d.send(protocol.P2PSignal{
			GrantID: grantID,
			From:    "control",
			To:      "worker",
			Signal:  "direct",
			State:   protocol.TerminalChannelP2PDirect,
			Message: "TUI WebRTC data channel opened.",
		})
	})
	channel.OnClose(func() {
		slog.Default().Info("tui p2p data channel closed", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID)
	})
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		return err
	}
	slog.Default().Info("tui p2p offer created", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", grantID, "sdp_type", offer.Type.String(), "sdp_len", len(offer.SDP))
	d.send(protocol.P2PSignal{
		GrantID: grantID,
		From:    "control",
		To:      "worker",
		Signal:  "offer",
		State:   "negotiating",
		SDPType: "offer",
		SDP:     offer.SDP,
		Message: "TUI WebRTC offer.",
	})
	return nil
}

func (d *directTransportController) acceptAnswer(signal protocol.P2PSignal) {
	d.mu.Lock()
	peer := d.peer
	d.mu.Unlock()
	if peer == nil || signal.SDP == "" {
		return
	}
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: signal.SDP}); err != nil {
		slog.Default().Debug("tui p2p answer failed", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID, "error", err)
		return
	}
	slog.Default().Info("tui p2p answer applied", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID, "sdp_len", len(signal.SDP))
}

func (d *directTransportController) acceptCandidate(signal protocol.P2PSignal) {
	d.mu.Lock()
	peer := d.peer
	d.mu.Unlock()
	if peer == nil || signal.Candidate == "" {
		return
	}
	if err := peer.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:     signal.Candidate,
		SDPMid:        emptyStringAsNil(signal.SDPMid),
		SDPMLineIndex: signal.SDPMLineIndex,
	}); err != nil {
		slog.Default().Debug("tui p2p candidate failed", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID, "error", err)
		return
	}
	slog.Default().Debug("tui p2p candidate applied", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID)
}

func (d *directTransportController) stopFallback() {
	d.mu.Lock()
	timer := d.fallback
	d.fallback = nil
	d.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

func (d *directTransportController) send(signal protocol.P2PSignal) {
	env, err := protocol.NewEnvelope(protocol.TypeP2PSignal, signal)
	if err != nil {
		return
	}
	env.SessionID = d.sessionID
	env.StreamID = d.streamID
	if err := writeEnvelope(d.conn, env); err != nil {
		slog.Default().Debug("tui p2p signal send failed", "session_id", d.sessionID, "stream_id", d.streamID, "grant_id", signal.GrantID, "signal", signal.Signal, "error", err)
	}
}

func emptyStringAsNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
