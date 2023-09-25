// Copyright (C) 2011 Werner Dittmann
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.
//
// Authors: Werner Dittmann <Werner.Dittmann@t-online.de>
//

package rtp

/*
 * This source file contains the global types, constants, methods and functions for the Session type
 */

import (
	"net"
	"sync"
	"time"
)

// Session contols and manages the resources and actions of a RTP session.
type Session struct {
	sync.RWMutex

	RtcpTransmission        // Data structure to control and manage RTCP reports.
	MaxNumberOutStreams int // Applications may set this to increase the number of supported output streams
	MaxNumberInStreams  int // Applications may set this to increase the number of supported input streams

	dataReceiveChan DataReceiveChan
	ctrlEventChan   CtrlEventChan

	streamsMapMutex sync.RWMutex // synchronize activities on stream maps
	streamsOut      streamOutMap
	streamsIn       streamInMap
	remotes         remoteMap
	conflicts       conflictMap

	activeSenders,
	streamOutIndex,
	streamInIndex,
	remoteIndex,
	conflictIndex uint32

	weSent            bool // is true if an output stream sent some RTP data
	rtcpServiceActive bool // true if an input stream received RTP packets after last RR
	rtcpCtrlChan      rtcpCtrlChan
	transportEnd      TransportEnd
	transportEndUpper TransportEnd
	transportWrite    TransportWrite
	transportRecv     TransportRecv
}

// Remote stores a remote addess in a transport independent way.
//
// The transport implementations construct UDP or TCP addresses and use them to send the data.
type Address struct {
	IpAddr             net.IP
	DataPort, CtrlPort int
	Zone               string
}

// The RTP stack sends CtrlEvent to the application if it creates a new input stream or receives RTCP packets.
//
// A RTCP compound may contain several RTCP packets. The RTP stack creates a CtrlEvent structure for each RTCP
// packet (SDES, BYE, etc) or report and stores them in a slice of CtrlEvent pointers and sends
// this slice to the application after all RTCP packets and reports are processed. The application may now loop
// over the slice and select the events that it may process.
type CtrlEvent struct {
	EventType int    // Either a Stream event or a Rtcp* packet type event, e.g. RtcpSR, RtcpRR, RtcpSdes, RtcpBye
	Ssrc      uint32 // the input stream's SSRC
	Index     uint32 // and its index
	Reason    string // Resaon string if it was available, empty otherwise
}

// Use a channel to signal if the transports are really closed.
type TransportEnd chan int

// Use a channel to send RTP data packets to the upper layer application.
type DataReceiveChan chan *DataPacket

// Use a channel to send RTCP control events to the upper layer application.
type CtrlEventChan chan []*CtrlEvent

// RTCP values to manage RTCP transmission intervals
type RtcpTransmission struct {
	tprev, // the last time an RTCP packet was transmitted
	tnext int64 // next scheduled transmission time
	RtcpSessionBandwidth float64 // Applications may (should) set this to bits/sec for RTCP traffic.
	// If not set RTP stack makes an educated guess.
	avrgPacketLength float64
}

// Returned in case of an error.
type Error string

func (s Error) Error() string {
	return string(s)
}

// Specific control event type that signal that a new input stream was created.
//
// If the RTP stack receives a data or control packet for a yet unknown input stream
// (SSRC not known) the stack creates a new input stream and signals this action to the application.
const (
	NewStreamData             = iota // Input stream creation triggered by a RTP data packet
	NewStreamCtrl                    // Input stream creation triggered by a RTCP control packet
	MaxNumInStreamReachedData        // Maximum number of input streams reached while receiving an RTP packet
	MaxNumInStreamReachedCtrl        // Maximum number of input streams reached while receiving an RTCP packet
	WrongStreamStatusData            // Received RTP packet for an inactive stream
	WrongStreamStatusCtrl            // Received RTCP packet for an inactive stream
	StreamCollisionLoopData          // Detected a collision or loop processing an RTP packet
	StreamCollisionLoopCtrl          // Detected a collision or loop processing an RTCP packet
)

// The receiver transports return these vaules via the TransportEnd channel when they are
// done stopping the data or control receivers.
const (
	DataTransportRecvStopped = 0x1
	CtrlTransportRecvStopped = 0x2
)

// Global Session functions.

// NewSession creates a new RTP session.
//
// A RTP session requires two transports:
//
//	tpw - a transport that implements the RtpTransportWrite interface
//	tpr - a transport that implements the RtpTransportRecv interface
func NewSession(tpw TransportWrite, tpr TransportRecv) *Session {
	rs := new(Session)

	// Maps grow dynamically, set size to avoid resizing in normal cases.
	rs.streamsOut = make(streamOutMap, maxNumberOutStreams)
	rs.streamsIn = make(streamInMap, maxNumberInStreams)
	rs.remotes = make(remoteMap, 2)
	rs.conflicts = make(conflictMap, 2)

	rs.MaxNumberOutStreams = maxNumberOutStreams
	rs.MaxNumberInStreams = maxNumberInStreams

	rs.transportWrite = tpw
	rs.transportRecv = tpr

	rs.transportEnd = make(TransportEnd, 2)
	rs.rtcpCtrlChan = make(rtcpCtrlChan, 1)

	tpr.SetCallUpper(rs)
	tpr.SetEndChannel(rs.transportEnd)

	return rs
}

// AddRemote adds the address and RTP port number of an additional remote peer.
//
// The port number must be even. The socket with the even port number sends and receives
// RTP packets. The socket with next odd port number sends and receives RTCP packets.
//
//	remote - the RTP address of the remote peer. The RTP data port number must be even.
func (rs *Session) AddRemote(remote *Address) (index uint32, err error) {
	rs.Lock()
	if (remote.DataPort & 0x1) == 0x1 {
		return 0, Error("RTP data port number is not an even number.")
	}
	rs.remotes[rs.remoteIndex] = remote
	index = rs.remoteIndex
	rs.remoteIndex++
	rs.Unlock()

	return
}

// RemoveRemote removes the address at the specified index.
func (rs *Session) RemoveRemote(index uint32) {
	rs.Lock()
	delete(rs.remotes, index)
	rs.Unlock()
}

// NewOutputStream creates a new RTP output stream and returns its index.
//
// A RTP session may have several output streams. The first output stream (stream with index 0)
// is the standard output stream. To use other output streams the application must use the
// the "*ForStream(...)" methods and specifiy the correct index of the stream.
//
// The index does not change for the lifetime of the stream and will not be reused during the lifetime of this session.
// (up to 2^64 streams per session :-) )
//
//	own        - Output stream's own address. Required to detect collisions and loops.
//	ssrc       - If not zero then this is the SSRC of the output stream. If zero then
//	             the method generates a random SSRC according to RFC 3550.
//	sequenceNo - If not zero then this is the starting sequence number of the output stream.
//	             If zero then the method generates a random starting sequence number according
//	             to RFC 3550
func (rs *Session) NewSsrcStreamOut(own *Address, ssrc uint32, sequenceNo uint16) (index uint32, err Error) {
	rs.RLock()
	rs.streamsMapMutex.RLock()
	if len(rs.streamsOut) > rs.MaxNumberOutStreams {
		rs.streamsMapMutex.RUnlock()
		rs.RUnlock()
		return 0, Error("Maximum number of output streams reached.")
	}
	rs.streamsMapMutex.RUnlock()
	rs.RUnlock()

	str := newSsrcStreamOut(own, ssrc, sequenceNo)
	str.streamStatus = active

	// Synchronize - may be called from several Go application functions in parallel
	// Don't reuse an existing SSRC
	for _, _, exists := rs.lookupSsrcMap(str.Ssrc()); exists; _, _, exists = rs.lookupSsrcMap(str.Ssrc()) {
		str.newSsrc()
	}

	rs.Lock()
	rs.streamsMapMutex.Lock()
	rs.streamsOut[rs.streamOutIndex] = str
	rs.streamsMapMutex.Unlock()
	index = rs.streamOutIndex
	rs.streamOutIndex++
	rs.Unlock()

	return
}

// StartSession activates the transports and starts the RTCP service.
//
// An application must have created an output stream that the session can use to send RTCP data. This
// is true even if the application is in "listening" mode only. An application must send receiver
// reports to it's remote peers.
func (rs *Session) StartSession() (err error) {
	err = rs.ListenOnTransports() // activate the transports
	if err != nil {
		return
	}
	// compute first transmission interval
	rs.Lock()
	rs.streamsMapMutex.RLock()
	if rs.RtcpSessionBandwidth == 0.0 { // If not set by application try to guess a value
		for _, str := range rs.streamsOut {
			format := PayloadFormatMap[int(str.PayloadType())]
			if format == nil {
				rs.RtcpSessionBandwidth += 64000. / 20.0 // some standard: 5% of a 64000 bit connection
			}
			// Assumption: fixed codec used, 8 byte per sample, one channel
			rs.RtcpSessionBandwidth += float64(format.ClockRate) * 8.0 / 20.
		}
	}
	rs.avrgPacketLength = float64(len(rs.streamsOut)*senderInfoLen + reportBlockLen + 20) // 28 for SDES
	rs.streamsMapMutex.RUnlock()

	// initial call: members, senders, RTCP bandwidth,   packet length,     weSent, initial
	ti, td := rtcpInterval(1, 0, rs.RtcpSessionBandwidth, rs.avrgPacketLength, false, true)
	rs.tnext = ti + time.Now().UnixNano()
	rs.Unlock()

	go rs.rtcpService(ti, td)

	return
}

// CloseSession closes the complete RTP session immediately.
//
// The methods stops the RTCP service, sends a BYE to all remaining active output streams, and
// closes the receiver transports,
func (rs *Session) CloseSession() {
	rs.RLock()
	if rs.rtcpServiceActive {
		ch := rs.rtcpCtrlChan
		rs.RUnlock()

		ch <- rtcpStopService

		rs.streamsMapMutex.RLock()
		for idx := range rs.streamsOut {
			rs.streamsMapMutex.RUnlock()
			rs.SsrcStreamCloseForIndex(idx)
			rs.streamsMapMutex.RLock()
		}
		rs.streamsMapMutex.RUnlock()

		rs.CloseRecv() // de-activate the transports
	} else {
		rs.RUnlock()
	}

	return
}

// NewDataPacket creates a new RTP packet suitable for use with the standard output stream.
//
// This method returns an initialized RTP packet that contains the correct SSRC, sequence
// number, the updated timestamp, and payload type if payload type was set in the stream.
//
// The application computes the next stamp based on the payload's frequency. The stamp usually
// advances by the number of samples contained in the RTP packet.
//
// For example PCMU with a 8000Hz frequency sends 160 samples every 20m - thus the timestamp
// must adavance by 160 for each following packet. For fixed codecs, for example PCMU, the
// number of samples correspond to the payload length. For variable codecs the number of samples
// has no direct relationship with the payload length.
//
//	stamp - the RTP timestamp for this packet.
func (rs *Session) NewDataPacket(stamp uint32) *DataPacket {
	rs.streamsMapMutex.RLock()
	str := rs.streamsOut[0]
	rs.streamsMapMutex.RUnlock()

	return str.newDataPacket(stamp)
}

// NewDataPacketForStream creates a new RTP packet suitable for use with the specified output stream.
//
// This method returns an initialized RTP packet that contains the correct SSRC, sequence
// number, and payload type if payload type was set in the stream. See also documentation of
// NewDataPacket.
//
//	streamindex - the index of the output stream as returned by NewSsrcStreamOut
//	stamp       - the RTP timestamp for this packet.
func (rs *Session) NewDataPacketForStream(streamIndex uint32, stamp uint32) *DataPacket {
	rs.streamsMapMutex.RLock()
	str := rs.streamsOut[streamIndex]
	rs.streamsMapMutex.RUnlock()

	return str.newDataPacket(stamp)
}

// CreateDataReceivedChan creates the data received channel and returns it to the caller.
//
// An application shall listen on this channel to get received RTP data packets.
// If the channel is full then the RTP receiver discards the data packets.
func (rs *Session) CreateDataReceiveChan() DataReceiveChan {
	ch := make(DataReceiveChan, dataReceiveChanLen)

	rs.Lock()
	rs.dataReceiveChan = ch
	rs.Unlock()

	return ch
}

// RemoveDataReceivedChan deletes the data received channel.
//
// The receiver discards all received packets.
func (rs *Session) RemoveDataReceiveChan() {
	rs.Lock()
	rs.dataReceiveChan = nil
	rs.Unlock()
}

// CreateCtrlEventChan creates the control event channel and returns it to the caller.
//
// An application shall listen on this channel to get control events.
// If the channel is full then the RTCP receiver does not send control events.
func (rs *Session) CreateCtrlEventChan() CtrlEventChan {
	ch := make(CtrlEventChan, ctrlEventChanLen)

	rs.Lock()
	rs.ctrlEventChan = ch
	rs.Unlock()

	return ch
}

// RemoveCtrlEventChan deletes the control event channel.
func (rs *Session) RemoveCtrlEventChan() {
	rs.Lock()
	rs.ctrlEventChan = nil
	rs.Unlock()
}

// SsrcStreamOut gets the standard output stream.
func (rs *Session) SsrcStreamOut() *SsrcStream {
	rs.streamsMapMutex.RLock()
	defer rs.streamsMapMutex.RUnlock()

	return rs.streamsOut[0]
}

// SsrcStreamOut gets the output stream at streamIndex.
//
//	streamindex - the index of the output stream as returned by NewSsrcStreamOut
func (rs *Session) SsrcStreamOutForIndex(streamIndex uint32) *SsrcStream {
	rs.streamsMapMutex.RLock()
	defer rs.streamsMapMutex.RUnlock()

	return rs.streamsOut[streamIndex]
}

// SsrcStreamIn gets the standard input stream.
func (rs *Session) SsrcStreamIn() *SsrcStream {
	rs.streamsMapMutex.RLock()
	defer rs.streamsMapMutex.RUnlock()

	return rs.streamsIn[0]
}

// SsrcStreamInForIndex Get the input stream with index.
//
//	streamindex - the index of the output stream as returned by NewSsrcStreamOut
func (rs *Session) SsrcStreamInForIndex(streamIndex uint32) *SsrcStream {
	rs.streamsMapMutex.RLock()
	defer rs.streamsMapMutex.RUnlock()

	return rs.streamsIn[streamIndex]
}

// SsrcStreamClose sends a RTCP BYE to the standard output stream (index 0).
//
// The method does not close the stream immediately but marks it as 'is closing'.
// In this state the stream stops its activities, does not send any new data or
// control packets. Eventually it will be in the state "is closed" and its resources
// are returned to the system. An application must not re-use a session.
func (rs *Session) SsrcStreamClose() {
	rs.SsrcStreamOutForIndex(0)
}

// SsrcStreamCloseForIndex sends a RTCP BYE to the stream at index index.
//
// See description for SsrcStreamClose above.
//
//	streamindex - the index of the output stream as returned by NewSsrcStreamOut
func (rs *Session) SsrcStreamCloseForIndex(streamIndex uint32) {
	rs.RLock()
	if rs.rtcpServiceActive {
		rs.RUnlock()

		rs.streamsMapMutex.RLock()
		str := rs.streamsOut[streamIndex]
		rs.streamsMapMutex.RUnlock()

		rc := rs.buildRtcpByePkt(str, "Go RTP says good-bye")
		rs.WriteCtrl(rc)

		str.Lock()
		str.streamStatus = isClosing
		str.Unlock()
	} else {
		rs.RUnlock()
	}
}

/*
 *** The following methods implement the rtp.TransportRecv interface.
 */

// SetCallUpper implements the rtp.RtpTransportRecv SetCallUpper method.
//
// Normal application don't use this method. Only if an application implements its own idea
// of the rtp.TransportRecv interface it may enable the call to upper layer.
//
// Currently this is a No-Op - delegating is not yet implemented.
func (rs *Session) SetCallUpper(upper TransportRecv) {
}

// ListenOnTransports implements the rtp.TransportRecv ListenOnTransports method.
//
// The session just forwards this to the appropriate transport receiver.
//
// Only relevant if an application uses "simple RTP".
func (rs *Session) ListenOnTransports() (err error) {
	return rs.transportRecv.ListenOnTransports()
}

// OnRecvData implements the rtp.TransportRecv OnRecvData method.
//
// Normal application don't use this method. Only if an application implements its own idea
// of the rtp.TransportRecv interface it must implement this function.
//
// Delegating is not yet implemented. Applications receive data via the DataReceiveChan.
func (rs *Session) OnRecvData(rp *DataPacket) bool {
	if !rp.IsValid() {
		rp.FreePacket()
		return false
	}

	rs.RLock()
	rtcpServiceActive := rs.rtcpServiceActive
	rs.RUnlock()

	// Check here if SRTP is enabled for the SSRC of the packet - a stream attribute
	if rtcpServiceActive {
		ssrc := rp.Ssrc()

		now := time.Now().UnixNano()
		str, _, existing := rs.lookupSsrcMap(ssrc)
		// if not found in the input stream then create a new SSRC input stream
		if !existing {
			str = newSsrcStreamIn(&rp.fromAddr, ssrc)

			rs.streamsMapMutex.RLock()
			if len(rs.streamsIn) > rs.MaxNumberInStreams {
				rs.streamsMapMutex.RUnlock()

				rs.sendDataCtrlEvent(MaxNumInStreamReachedData, ssrc, 0)
				rp.FreePacket()

				return false
			} else {
				rs.streamsMapMutex.RUnlock()
			}

			rs.streamsMapMutex.Lock()
			rs.streamsIn[rs.streamInIndex] = str
			rs.streamInIndex++
			rs.streamsMapMutex.Unlock()

			str.Lock()
			str.streamMutex.Lock()
			str.streamStatus = active
			str.statistics.initialDataTime = now // First packet arrival time.
			str.streamMutex.Unlock()
			str.Unlock()

			rs.streamsMapMutex.RLock()
			idx := rs.streamInIndex - 1
			rs.streamsMapMutex.RUnlock()

			rs.sendDataCtrlEvent(NewStreamData, ssrc, idx)
		} else {
			// Check if an existing stream is active
			str.RLock()
			if str.streamStatus != active {
				str.RUnlock()

				rs.streamsMapMutex.RLock()
				idx := rs.streamInIndex - 1
				rs.streamsMapMutex.RUnlock()

				rs.sendDataCtrlEvent(WrongStreamStatusData, ssrc, idx)
				rp.FreePacket()

				return false

			} else {
				str.RUnlock()
			}
			// Test if RTCP packets had been received but this is the first data packet from this source.
			str.Lock()
			if str.DataPort == 0 {
				str.DataPort = rp.fromAddr.DataPort
			}
			str.Unlock()
		}

		// Before forwarding packet to next upper layer (application) for further processing:
		// 1) check for collisions and loops. If the packet cannot be assigned to a source, it will be rejected.
		// 2) check the source is a sufficiently well known source
		// TODO: also check CSRC identifiers.
		if !str.checkSsrcIncomingData(existing, rs, rp) || !str.recordReceptionData(rp, rs, now) {
			// must be discarded due to collision or loop or invalid source
			rs.sendDataCtrlEvent(StreamCollisionLoopData, ssrc, rs.streamInIndex-1)
			rp.FreePacket()

			return false
		}
	}

	rs.RLock()
	dataCh := rs.dataReceiveChan
	rs.RUnlock()

	select {
	case dataCh <- rp: // forwarded packet, that's all folks
	default:
		rp.FreePacket() // either channel full or not created - free packet
	}
	return true
}

// OnRecvCtrl implements the rtp.TransportRecv OnRecvCtrl method.
//
// Normal application don't use this method. Only if an application implements its own idea
// of the rtp.TransportRecv interface it must implement this function.
//
// Delegating is not yet implemented. Applications may receive control events via
// the CtrlEventChan.
func (rs *Session) OnRecvCtrl(rp *CtrlPacket) bool {
	rs.RLock()
	rtcpServiceActive := rs.rtcpServiceActive
	rs.RUnlock()

	if !rtcpServiceActive {
		return true
	}

	if pktType := rp.Type(0); pktType != RtcpSR && pktType != RtcpRR {
		rp.FreePacket()
		return false
	}
	// Check here if SRTCP is enabled for the SSRC of the packet - a stream attribute

	ctrlEvArr := make([]*CtrlEvent, 0, 10)

	offset := 0
	for offset < rp.inUse {
		pktLen := int((rp.Length(offset) + 1) * 4)

		switch rp.Type(offset) {
		case RtcpSR:
			rrCnt := rp.Count(offset)
			if offset+pktLen > len(rp.Buffer()) {
				return false
			}
			// Always check sender's SSRC first in case of RR or SR
			str, strIdx, existing := rs.rtcpSenderCheck(rp, offset)
			if str == nil {
				// Below commented line was causing segmentation-fault. Calling str.Ssrc() for nil str
				// Probably because of out-of-order coming UDP packets like receiving RTCP packets for already closed streams.
				// So I preferred to discard such RTCP packets. --LS
				rp.FreePacket()
				return false
				//ctrlEvArr = append(ctrlEvArr, newCrtlEvent(int(strIdx), str.Ssrc(), 0))
			} else {
				if !existing {
					rs.streamsMapMutex.RLock()
					ctrlEvArr = append(ctrlEvArr, newCrtlEvent(NewStreamCtrl, str.Ssrc(), rs.streamInIndex-1))
					rs.streamsMapMutex.RUnlock()
				}

				str.streamMutex.Lock()
				str.statistics.lastRtcpSrTime = str.statistics.lastRtcpPacketTime
				str.streamMutex.Unlock()

				str.readSenderInfo(rp.toSenderInfo(rtcpHeaderLength + rtcpSsrcLength + offset))

				ctrlEvArr = append(ctrlEvArr, newCrtlEvent(RtcpSR, str.Ssrc(), strIdx))

				// Offset to first RR block: offset to SR + fixed Header length for SR + length of sender info
				rrOffset := offset + rtcpHeaderLength + rtcpSsrcLength + senderInfoLen

				for i := 0; i < rrCnt; i++ {
					rr := rp.toRecvReport(rrOffset)
					strOut, idx, exists := rs.lookupSsrcMapOut(rr.ssrc())
					// Process Receive Reports that match own output streams (SSRC).
					if exists {
						strOut.readRecvReport(rr)
						ctrlEvArr = append(ctrlEvArr, newCrtlEvent(RtcpRR, rr.ssrc(), idx))
					}
					rrOffset += reportBlockLen
				}
			}
			// Advance to the next packet in the compound.
			offset += pktLen

		case RtcpRR:
			if offset+pktLen > len(rp.Buffer()) {
				return false
			}
			// Always check sender's SSRC first in case of RR or SR
			str, _, existing := rs.rtcpSenderCheck(rp, offset)
			if str == nil {
				// Below commented line was causing segmentation-fault. Calling str.Ssrc() for nil str
				// Probably because of out-of-order coming UDP packets like receiving RTCP packets for already closed streams.
				// So I preferred to discard such RTCP packets. --LS
				rp.FreePacket()
				return false
				//ctrlEvArr = append(ctrlEvArr, newCrtlEvent(int(strIdx), str.Ssrc(), 0))
			} else {
				if !existing {
					rs.streamsMapMutex.Lock()
					ctrlEvArr = append(ctrlEvArr, newCrtlEvent(NewStreamCtrl, str.Ssrc(), rs.streamInIndex-1))
					rs.streamsMapMutex.Unlock()
				}

				rrCnt := rp.Count(offset)
				// Offset to first RR block: offset to RR + fixed Header length for RR
				rrOffset := offset + rtcpHeaderLength + rtcpSsrcLength
				for i := 0; i < rrCnt; i++ {
					rr := rp.toRecvReport(rrOffset)
					strOut, idx, exists := rs.lookupSsrcMapOut(rr.ssrc())
					// Process Receive Reports that match own output streams (SSRC)
					if exists {
						strOut.readRecvReport(rr)
						ctrlEvArr = append(ctrlEvArr, newCrtlEvent(RtcpRR, rr.ssrc(), idx))
					}
					rrOffset += reportBlockLen
				}
			}
			// Advance to the next packet in the compound.
			offset += pktLen

		case RtcpSdes:
			if offset+pktLen > len(rp.Buffer()) {
				return false
			}
			sdesChunkCnt := rp.Count(offset)
			sdesPktLen := int(rp.Length(offset) * 4) // length excl. header word
			// Offset to first SDES chunk: offset to SDES + Header word for SDES
			sdesChunkOffset := offset + 4
			for i := 0; i < sdesChunkCnt; i++ {
				chunk := rp.toSdesChunk(sdesChunkOffset, sdesPktLen)
				if chunk == nil {
					break
				}
				chunkLen, idx, ok := rs.processSdesChunk(chunk, rp)
				if !ok {
					break
				}
				ctrlEvArr = append(ctrlEvArr, newCrtlEvent(RtcpSdes, chunk.ssrc(), idx))
				sdesChunkOffset += chunkLen
				sdesPktLen -= chunkLen
			}
			// Advance to the next packet in the compound, is also index after SDES packet
			offset += pktLen

		case RtcpBye:
			if offset+pktLen > len(rp.Buffer()) {
				return false
			}
			// Currently the method suports only one SSRC per BYE packet. To enhance this we need
			// to return an array of SSRC/CSRC values.
			//
			byeCnt := rp.Count(offset)
			byePkt := rp.toByeData(offset+4, pktLen-4)
			if byePkt != nil {
				// Send BYE control event only for known input streams.
				if st, idx, ok := rs.lookupSsrcMapIn(byePkt.ssrc(0)); ok {
					ctrlEv := newCrtlEvent(RtcpBye, byePkt.ssrc(0), idx)
					ctrlEv.Reason = byePkt.getReason(byeCnt)
					ctrlEvArr = append(ctrlEvArr, ctrlEv)
					st.Lock()
					st.streamStatus = isClosing
					st.Unlock()
				}
				// Recompute time intervals, see chapter 6.3.4
				// TODO: not len(rs.streamsIn) but get number of members with streamStatus == active
				rs.streamsMapMutex.RLock()
				pmembers := float64(len(rs.streamsOut) + len(rs.streamsIn))
				rs.streamsMapMutex.RUnlock()
				members := pmembers - 1.0 // received a BYE for one input channel
				tc := float64(time.Now().UnixNano())
				rs.Lock()
				tn := tc + members/pmembers*(float64(rs.tnext)-tc)
				rs.tnext = int64(tn)
				rs.Unlock()
			}
			// Advance to the next packet in the compound.
			offset += pktLen

		case RtcpApp:
			// Advance to the next packet in the compound.
			offset += pktLen
		case RtcpRtpfb:
			// Advance to the next packet in the compound.
			offset += pktLen
		case RtcpPsfb:
			// Advance to the next packet in the compound.
			offset += pktLen
		case RtcpXr:
			// Advance to the next packet in the compound.
			offset += pktLen

		}
	}

	rs.RLock()
	ctrlCh := rs.ctrlEventChan
	rs.RUnlock()

	select {
	case ctrlCh <- ctrlEvArr: // send control event
	default:
	}
	// re-compute average packet size. Don't re-compute RTCP interval time, will be done on next RTCP report
	// interval. The timing is not affected that much by delaying the interval re-computation.
	size := float64(rp.InUse() + 20 + 8) // TODO: get real values for IP and transport from transport module

	rs.Lock()
	rs.avrgPacketLength = (1.0/16.0)*size + (15.0/16.0)*rs.avrgPacketLength
	rs.Unlock()

	rp.FreePacket()
	ctrlEvArr = nil

	return true
}

// CloseRecv implements the rtp.TransportRecv CloseRecv method.
//
// The method calls the registered transport's CloseRecv() method and waits for the Stopped
// signal data for RTP and RTCP.
//
// If a upper layer application has registered a transportEnd channel forward the signal to it.
//
// Only relevant if an application uses "simple RTP".
func (rs *Session) CloseRecv() {
	rs.RLock()
	if rs.transportRecv != nil {
		recv := rs.transportRecv
		ch := rs.transportEnd
		rs.RUnlock()

		recv.CloseRecv()
		for allClosed := 0; allClosed != (DataTransportRecvStopped | CtrlTransportRecvStopped); {
			allClosed |= <-ch
		}

		rs.RLock()
	}
	if rs.transportEndUpper != nil {
		ch := rs.transportEndUpper
		rs.RUnlock()

		ch <- (DataTransportRecvStopped | CtrlTransportRecvStopped)

		rs.RLock()
	}
	rs.RUnlock()
}

// SetEndChannel implements the rtp.TransportRecv SetEndChannel method.
//
// An application may register a specific control channel to get information after
// all receiver transports were closed.
//
// Only relevant if an application uses "simple RTP".
func (rs *Session) SetEndChannel(ch TransportEnd) {
	rs.Lock()
	rs.transportEndUpper = ch
	rs.Unlock()
}

/*
 *** The following methods implement the rtp.TransportWrite interface.
 */

// WriteData implements the rtp.TransportWrite WriteData method and sends an RTP packet.
//
// The method writes the packet of an active output stream to all known remote destinations.
// This functions updates some statistical values to enable RTCP processing.
func (rs *Session) WriteData(rp *DataPacket) (n int, err error) {
	strOut, _, _ := rs.lookupSsrcMapOut(rp.Ssrc())

	strOut.Lock()
	if strOut.streamStatus != active {
		strOut.Unlock()
		return 0, nil
	}

	strOut.sentPktCnt++
	strOut.sentOctCnt += uint32(len(rp.Payload()))

	rs.RLock()
	ch := rs.rtcpCtrlChan
	rs.RUnlock()

	if !strOut.sender && ch != nil {
		strOut.Unlock()

		ch <- rtcpIncrementSender

		strOut.Lock()
		strOut.sender = true
	}
	strOut.Unlock()

	strOut.streamMutex.Lock()
	strOut.statistics.lastPacketTime = time.Now().UnixNano()
	strOut.streamMutex.Unlock()

	rs.Lock()
	rs.weSent = true
	rs.Unlock()

	// Check here if SRTP is enabled for the SSRC of the packet - a stream attribute
	rs.RLock()
	for _, remote := range rs.remotes {
		_, err := rs.transportWrite.WriteDataTo(rp, remote)
		if err != nil {
			rs.RUnlock()
			return 0, err
		}
	}
	rs.RUnlock()

	return n, nil
}

// WriteCtrl implements the rtp.TransportWrite WriteCtrl method and sends an RTCP packet.
//
// The method sends an RTCP packet of an active output stream to all known remote destinations.
// Usually normal applications don't use this function, RTCP is handled internally.
func (rs *Session) WriteCtrl(rp *CtrlPacket) (n int, err error) {
	// Check here if SRTCP is enabled for the SSRC of the packet - a stream attribute
	strOut, _, _ := rs.lookupSsrcMapOut(rp.Ssrc(0))

	strOut.RLock()
	if strOut.streamStatus != active {
		strOut.RUnlock()

		return 0, nil
	}
	strOut.RUnlock()

	rs.RLock()
	for _, remote := range rs.remotes {
		_, err := rs.transportWrite.WriteCtrlTo(rp, remote)
		if err != nil {
			rs.RUnlock()
			return 0, err
		}
	}
	rs.RUnlock()

	return n, nil
}
