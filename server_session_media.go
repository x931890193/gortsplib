package gortsplib

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/liberrors"
)

type serverSessionMedia struct {
	ss           *ServerSession
	media        *description.Media
	onPacketRTCP OnPacketRTCPFunc

	tcpChannel             int
	udpRTPReadPort         int
	udpRTPWriteAddr        *net.UDPAddr
	udpRTCPReadPort        int
	udpRTCPWriteAddr       *net.UDPAddr
	formats                map[uint8]*serverSessionFormat // record only
	writePacketRTPInQueue  func([]byte) error
	writePacketRTCPInQueue func([]byte) error
}

func (sm *serverSessionMedia) initialize() {
	if sm.ss.state == ServerSessionStatePreRecord {
		sm.formats = make(map[uint8]*serverSessionFormat)
		for _, forma := range sm.media.Formats {
			sm.formats[forma.PayloadType()] = &serverSessionFormat{
				sm:          sm,
				format:      forma,
				onPacketRTP: func(*rtp.Packet) {},
			}
		}
	}
}

func (sm *serverSessionMedia) start() {
	// allocate udpRTCPReceiver before udpRTCPListener
	// otherwise udpRTCPReceiver.LastSSRC() can't be called.
	for _, sf := range sm.formats {
		sf.start()
	}

	switch *sm.ss.setuppedTransport {
	case TransportUDP, TransportUDPMulticast:
		sm.writePacketRTPInQueue = sm.writePacketRTPInQueueUDP
		sm.writePacketRTCPInQueue = sm.writePacketRTCPInQueueUDP

		if *sm.ss.setuppedTransport == TransportUDP {
			if sm.ss.state == ServerSessionStatePlay {
				// firewall opening is performed with RTCP sender reports generated by ServerStream

				// readers can send RTCP packets only
				sm.ss.s.udpRTCPListener.addClient(sm.ss.author.ip(), sm.udpRTCPReadPort, sm.readRTCPUDPPlay)
			} else {
				// open the firewall by sending empty packets to the counterpart.
				byts, _ := (&rtp.Packet{Header: rtp.Header{Version: 2}}).Marshal()
				sm.ss.s.udpRTPListener.write(byts, sm.udpRTPWriteAddr) //nolint:errcheck

				byts, _ = (&rtcp.ReceiverReport{}).Marshal()
				sm.ss.s.udpRTCPListener.write(byts, sm.udpRTCPWriteAddr) //nolint:errcheck

				sm.ss.s.udpRTPListener.addClient(sm.ss.author.ip(), sm.udpRTPReadPort, sm.readRTPUDPRecord)
				sm.ss.s.udpRTCPListener.addClient(sm.ss.author.ip(), sm.udpRTCPReadPort, sm.readRTCPUDPRecord)
			}
		}

	case TransportTCP:
		sm.writePacketRTPInQueue = sm.writePacketRTPInQueueTCP
		sm.writePacketRTCPInQueue = sm.writePacketRTCPInQueueTCP

		if sm.ss.tcpCallbackByChannel == nil {
			sm.ss.tcpCallbackByChannel = make(map[int]readFunc)
		}

		if sm.ss.state == ServerSessionStatePlay {
			sm.ss.tcpCallbackByChannel[sm.tcpChannel] = sm.readRTPTCPPlay
			sm.ss.tcpCallbackByChannel[sm.tcpChannel+1] = sm.readRTCPTCPPlay
		} else {
			sm.ss.tcpCallbackByChannel[sm.tcpChannel] = sm.readRTPTCPRecord
			sm.ss.tcpCallbackByChannel[sm.tcpChannel+1] = sm.readRTCPTCPRecord
		}
	}
}

func (sm *serverSessionMedia) stop() {
	if *sm.ss.setuppedTransport == TransportUDP {
		sm.ss.s.udpRTPListener.removeClient(sm.ss.author.ip(), sm.udpRTPReadPort)
		sm.ss.s.udpRTCPListener.removeClient(sm.ss.author.ip(), sm.udpRTCPReadPort)
	}

	for _, sf := range sm.formats {
		sf.stop()
	}
}

func (sm *serverSessionMedia) findFormatWithSSRC(ssrc uint32) *serverSessionFormat {
	for _, format := range sm.formats {
		tssrc, ok := format.rtcpReceiver.SenderSSRC()
		if ok && tssrc == ssrc {
			return format
		}
	}
	return nil
}

func (sm *serverSessionMedia) writePacketRTPInQueueUDP(payload []byte) error {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	return sm.ss.s.udpRTPListener.write(payload, sm.udpRTPWriteAddr)
}

func (sm *serverSessionMedia) writePacketRTCPInQueueUDP(payload []byte) error {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	return sm.ss.s.udpRTCPListener.write(payload, sm.udpRTCPWriteAddr)
}

func (sm *serverSessionMedia) writePacketRTPInQueueTCP(payload []byte) error {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.ss.tcpFrame.Channel = sm.tcpChannel
	sm.ss.tcpFrame.Payload = payload
	sm.ss.tcpConn.nconn.SetWriteDeadline(time.Now().Add(sm.ss.s.WriteTimeout))
	return sm.ss.tcpConn.conn.WriteInterleavedFrame(sm.ss.tcpFrame, sm.ss.tcpBuffer)
}

func (sm *serverSessionMedia) writePacketRTCPInQueueTCP(payload []byte) error {
	atomic.AddUint64(sm.ss.bytesSent, uint64(len(payload)))
	sm.ss.tcpFrame.Channel = sm.tcpChannel + 1
	sm.ss.tcpFrame.Payload = payload
	sm.ss.tcpConn.nconn.SetWriteDeadline(time.Now().Add(sm.ss.s.WriteTimeout))
	return sm.ss.tcpConn.conn.WriteInterleavedFrame(sm.ss.tcpFrame, sm.ss.tcpBuffer)
}

func (sm *serverSessionMedia) readRTCPUDPPlay(payload []byte) bool {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBigUDP{})
		return false
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	for _, pkt := range packets {
		sm.onPacketRTCP(pkt)
	}

	return true
}

func (sm *serverSessionMedia) readRTPUDPRecord(payload []byte) bool {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketTooBigUDP{})
		return false
	}

	pkt := &rtp.Packet{}
	err := pkt.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	forma, ok := sm.formats[pkt.PayloadType]
	if !ok {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketUnknownPayloadType{PayloadType: pkt.PayloadType})
		return false
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	forma.readRTPUDP(pkt, now)

	return true
}

func (sm *serverSessionMedia) readRTCPUDPRecord(payload []byte) bool {
	plen := len(payload)

	atomic.AddUint64(sm.ss.bytesReceived, uint64(plen))

	if plen == (udpMaxPayloadSize + 1) {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBigUDP{})
		return false
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	now := sm.ss.s.timeNow()
	atomic.StoreInt64(sm.ss.udpLastPacketTime, now.Unix())

	for _, pkt := range packets {
		if sr, ok := pkt.(*rtcp.SenderReport); ok {
			format := sm.findFormatWithSSRC(sr.SSRC)
			if format != nil {
				format.rtcpReceiver.ProcessSenderReport(sr, now)
			}
		}

		sm.onPacketRTCP(pkt)
	}

	return true
}

func (sm *serverSessionMedia) readRTPTCPPlay(_ []byte) bool {
	return false
}

func (sm *serverSessionMedia) readRTCPTCPPlay(payload []byte) bool {
	if len(payload) > udpMaxPayloadSize {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBig{L: len(payload), Max: udpMaxPayloadSize})
		return false
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	for _, pkt := range packets {
		sm.onPacketRTCP(pkt)
	}

	return true
}

func (sm *serverSessionMedia) readRTPTCPRecord(payload []byte) bool {
	pkt := &rtp.Packet{}
	err := pkt.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	forma, ok := sm.formats[pkt.PayloadType]
	if !ok {
		sm.ss.onDecodeError(liberrors.ErrServerRTPPacketUnknownPayloadType{PayloadType: pkt.PayloadType})
		return false
	}

	forma.readRTPTCP(pkt)

	return true
}

func (sm *serverSessionMedia) readRTCPTCPRecord(payload []byte) bool {
	if len(payload) > udpMaxPayloadSize {
		sm.ss.onDecodeError(liberrors.ErrServerRTCPPacketTooBig{L: len(payload), Max: udpMaxPayloadSize})
		return false
	}

	packets, err := rtcp.Unmarshal(payload)
	if err != nil {
		sm.ss.onDecodeError(err)
		return false
	}

	now := sm.ss.s.timeNow()

	for _, pkt := range packets {
		if sr, ok := pkt.(*rtcp.SenderReport); ok {
			format := sm.findFormatWithSSRC(sr.SSRC)
			if format != nil {
				format.rtcpReceiver.ProcessSenderReport(sr, now)
			}
		}

		sm.onPacketRTCP(pkt)
	}

	return true
}
