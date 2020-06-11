/*
 * NETCAP - Traffic Analysis Framework
 * Copyright (c) 2017-2020 Philipp Mieden <dreadl0ck [at] protonmail [dot] ch>
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

// This code is based on the gopacket/examples/reassemblydump/main.go example.
// The following license is provided:
// Copyright (c) 2012 Google, Inc. All rights reserved.
// Copyright (c) 2009-2011 Andreas Krennmair. All rights reserved.

// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:

//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Andreas Krennmair, Google, nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.

// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package encoder

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/mgutz/ansi"
	"sync"

	"log"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/dreadl0ck/gopacket"
	"github.com/dreadl0ck/gopacket/layers"
	"github.com/dreadl0ck/netcap"
	"github.com/dreadl0ck/netcap/reassembly"
	"github.com/dreadl0ck/netcap/utils"
	"github.com/evilsocket/islazy/tui"
)

var (
	numErrors uint

	requests  = 0
	responses = 0
	// synchronizes access to stats
	statsMutex sync.Mutex

	count          = 0
	dataBytes      = int64(0)
	start          = time.Now()
	errorsMap      = make(map[string]uint)
	errorsMapMutex sync.Mutex
)

var reassemblyStats struct {
	ipdefrag            int64
	missedBytes         int64
	pkt                 int64
	sz                  int64
	totalsz             int64
	rejectFsm           int64
	rejectOpt           int64
	rejectConnFsm       int64
	reassembled         int64
	outOfOrderBytes     int64
	outOfOrderPackets   int64
	biggestChunkBytes   int64
	biggestChunkPackets int64
	overlapBytes        int64
	overlapPackets      int64
	savedTCPConnections int64
	savedUDPConnections int64
	numSoftware         int64
	numServices         int64
}

func NumSavedTCPConns() int64 {
	statsMutex.Lock()
	defer statsMutex.Unlock()
	return reassemblyStats.savedTCPConnections
}

func NumSavedUDPConns() int64 {
	statsMutex.Lock()
	defer statsMutex.Unlock()
	return reassemblyStats.savedUDPConnections
}

/*
 * TCP Connection
 */

// internal structure that describes a bi-directional TCP connection
// It implements the reassembly.Stream interface to handle the incoming data
// and manage the stream lifecycle
type tcpConnection struct {
	tcpstate   *reassembly.TCPSimpleFSM
	optchecker reassembly.TCPOptionCheck

	net, transport gopacket.Flow

	fsmerr bool

	isHTTP  bool
	isHTTPS bool
	isPOP3  bool
	isSSH   bool

	ident string

	decoder StreamDecoder
	client  StreamReader
	server  StreamReader

	firstPacket time.Time

	conversationRaw     bytes.Buffer
	conversationColored bytes.Buffer

	// if set, indicates that either client or server stream reader was closed already
	last bool

	merged StreamDataSlice

	sync.Mutex
}

// Accept decides whether the TCP packet should be accepted
// start could be modified to force a start even if no SYN have been seen
func (t *tcpConnection) Accept(tcp *layers.TCP, ci gopacket.CaptureInfo, dir reassembly.TCPFlowDirection, nextSeq reassembly.Sequence, start *bool, ac reassembly.AssemblerContext) bool {

	// Finite State Machine
	if !t.tcpstate.CheckState(tcp, dir) {
		logReassemblyError("FSM", "%s: Packet rejected by FSM (state:%s)\n", t.ident, t.tcpstate.String())
		statsMutex.Lock()
		reassemblyStats.rejectFsm++
		if !t.fsmerr {
			t.fsmerr = true
			reassemblyStats.rejectConnFsm++
		}
		statsMutex.Unlock()
		if !c.IgnoreFSMerr {
			return false
		}
	}

	// TCP Options
	err := t.optchecker.Accept(tcp, ci, dir, nextSeq, start)
	if err != nil {
		logReassemblyError("OptionChecker", "%s: Packet rejected by OptionChecker: %s\n", t.ident, err)
		statsMutex.Lock()
		reassemblyStats.rejectOpt++
		statsMutex.Unlock()
		if !c.NoOptCheck {
			return false
		}
	}

	// TCP Checksum
	accept := true
	if c.Checksum {
		c, err := tcp.ComputeChecksum()
		if err != nil {
			logReassemblyError("ChecksumCompute", "%s: Got error computing checksum: %s\n", t.ident, err)
			accept = false
		} else if c != 0x0 {
			logReassemblyError("Checksum", "%s: Invalid checksum: 0x%x\n", t.ident, c)
			accept = false
		}
	}

	// stats
	if !accept {
		statsMutex.Lock()
		reassemblyStats.rejectOpt++
		statsMutex.Unlock()
	}
	return accept
}

func (t *tcpConnection) updateStats(sg reassembly.ScatterGather, skip int, length int, saved int, start bool, end bool, dir reassembly.TCPFlowDirection) {

	sgStats := sg.Stats()

	statsMutex.Lock()
	if skip > 0 {
		reassemblyStats.missedBytes += int64(skip)
	}

	reassemblyStats.sz += int64(length - saved)
	reassemblyStats.pkt += int64(sgStats.Packets)
	if sgStats.Chunks > 1 {
		reassemblyStats.reassembled++
	}
	reassemblyStats.outOfOrderPackets += int64(sgStats.QueuedPackets)
	reassemblyStats.outOfOrderBytes += int64(sgStats.QueuedBytes)
	if int64(length) > reassemblyStats.biggestChunkBytes {
		reassemblyStats.biggestChunkBytes = int64(length)
	}
	if int64(sgStats.Packets) > reassemblyStats.biggestChunkPackets {
		reassemblyStats.biggestChunkPackets = int64(sgStats.Packets)
	}
	if sgStats.OverlapBytes != 0 && sgStats.OverlapPackets == 0 {
		utils.ReassemblyLog.Println("ReassembledSG: invalid overlap, bytes:", sgStats.OverlapBytes, "packets:", sgStats.OverlapPackets)
	}
	reassemblyStats.overlapBytes += int64(sgStats.OverlapBytes)
	reassemblyStats.overlapPackets += int64(sgStats.OverlapPackets)
	statsMutex.Unlock()

	var ident string
	if dir == reassembly.TCPDirClientToServer {
		ident = fmt.Sprintf("%v %v(%s): ", t.net, t.transport, dir)
	} else {
		ident = fmt.Sprintf("%v %v(%s): ", t.net.Reverse(), t.transport.Reverse(), dir)
	}

	logReassemblyDebug("%s: SG reassembled packet with %d bytes (start:%v,end:%v,skip:%d,saved:%d,nb:%d,%d,overlap:%d,%d)\n", ident, length, start, end, skip, saved, sgStats.Packets, sgStats.Chunks, sgStats.OverlapBytes, sgStats.OverlapPackets)
}

func (t *tcpConnection) feedData(dir reassembly.TCPFlowDirection, data []byte, ac reassembly.AssemblerContext) {

	// Copy the data before passing it to the handler
	// Because the passed in buffer can be reused as soon as the ReassembledSG function returned
	dataCpy := make([]byte, len(data))
	l := copy(dataCpy, data)

	if l != len(data) {
		log.Fatal("l != len(data): ", l, " != ", len(data), " ident:", t.ident)
	}

	// pass data either to client or server
	if dir == reassembly.TCPDirClientToServer {
		t.client.DataChan() <- &StreamData{
			raw: dataCpy,
			ac:  ac,
			dir: dir,
		}
	} else {
		t.server.DataChan() <- &StreamData{
			raw: dataCpy,
			ac:  ac,
			dir: dir,
		}
	}
}

//
//func (t *tcpConnection) feedDataTimeout(dir reassembly.TCPFlowDirection, data []byte, ac reassembly.AssemblerContext) {
//
//	// Copy the data before passing it to the handler
//	// Because the passed in buffer can be reused as soon as the ReassembledSG function returned
//	dataCpy := make([]byte, len(data))
//	l := copy(dataCpy, data)
//
//	if l != len(data) {
//		log.Fatal("l != len(data): ", l, " != ", len(data), " ident:", t.ident)
//	}
//
//	if dir == reassembly.TCPDirClientToServer {
//		select {
//		case t.client.DataChan() <- &StreamData{
//			raw: dataCpy,
//			ac:  ac,
//			dir: dir,
//		}:
//		case <-time.After(100 * time.Millisecond):
//			//fmt.Println(t.ident, "timeout")
//		}
//	} else {
//		select {
//		case t.server.DataChan() <- &StreamData{
//			raw: dataCpy,
//			ac:  ac,
//			dir: dir,
//		}:
//		case <-time.After(100 * time.Millisecond):
//			//fmt.Println(t.ident, "timeout")
//		}
//	}
//}

// ReassembledSG is called zero or more times and delivers the data for a stream
// The ScatterGather buffer is reused after each Reassembled call
// so it's important to copy anything you need out of it (or use KeepFrom())
func (t *tcpConnection) ReassembledSG(sg reassembly.ScatterGather, ac reassembly.AssemblerContext) {

	length, saved := sg.Lengths()
	dir, start, end, skip := sg.Info()

	// update stats
	t.updateStats(sg, skip, length, saved, start, end, dir)

	if skip == -1 && c.AllowMissingInit {
		// this is allowed
	} else if skip != 0 {
		// Missing bytes in stream: do not even try to parse it
		return
	}

	data := sg.Fetch(length)

	// dont process stream if protocol is disabled
	if t.isSSH && !streamFactory.decodeSSH {
		return
	}
	if t.isHTTP && !streamFactory.decodeHTTP {
		return
	}
	if t.isPOP3 && !streamFactory.decodePOP3 {
		return
	}

	// do not process encrypted HTTP streams for now
	if t.isHTTPS {
		return
	}

	//fmt.Println("got raw data:", len(data), ac.GetCaptureInfo().Timestamp, "\n", hex.Dump(data))

	if length > 0 {
		if c.HexDump {
			logReassemblyDebug("Feeding stream reader with:\n%s", hex.Dump(data))
		}
		t.feedData(dir, data, ac)
	}
}

// ReassemblyComplete is called when assembly decides there is
// no more data for this Stream, either because a FIN or RST packet
// was seen, or because the stream has timed out without any new
// packet data (due to a call to FlushCloseOlderThan).
// It should return true if the connection should be removed from the pool
// It can return false if it want to see subsequent packets with Accept(), e.g. to
// see FIN-ACK, for deeper state-machine analysis.
func (t *tcpConnection) ReassemblyComplete(ac reassembly.AssemblerContext, flow gopacket.Flow) bool {

	//fmt.Println(t.ident, "t.firstPacket:", t.firstPacket, "ac.Timestamp", ac.GetCaptureInfo().Timestamp)

	// is this packet older than the oldest packet we saw for this connection?
	// if yes, if check the direction of the client is correct
	if !t.firstPacket.Equal(ac.GetCaptureInfo().Timestamp) && t.firstPacket.After(ac.GetCaptureInfo().Timestamp) {

		// update first packet timestamp on connection
		t.Lock()
		t.firstPacket = ac.GetCaptureInfo().Timestamp
		t.Unlock()

		if t.client != nil && t.server != nil {
			// check if flow is identical or needs to be flipped
			if !(t.client.Network() == flow) {

				// flip
				t.client.SetClient(false)
				t.server.SetClient(true)

				t.Lock()
				t.ident = reverseIdent(t.ident)
				//fmt.Println("flip! new", ansi.Red + t.ident + ansi.Reset)

				t.client, t.server = t.server, t.client
				t.transport, t.net = t.transport.Reverse(), t.net.Reverse()

				// fix directions for all data fragments
				for _, d := range t.client.DataSlice() {
					d.dir = reassembly.TCPDirClientToServer
				}
				for _, d := range t.server.DataSlice() {
					d.dir = reassembly.TCPDirServerToClient
				}
				t.Unlock()
			}
		}
	}

	utils.DebugLog.Println("ReassemblyComplete", t.ident)
	//fmt.Println("ReassemblyComplete", t.ident)

	// save data for the current stream
	if t.client != nil {

		t.client.MarkSaved()

		// client
		err := saveConnection(t.ConversationRaw(), t.ConversationColored(), t.client.Ident(), t.client.FirstPacket(), t.client.Transport())
		if err != nil {
			fmt.Println("failed to save stream", err)
		}
	}

	if t.server != nil {

		t.server.MarkSaved()

		// server
		saveTCPServiceBanner(t.server)
	}

	// channels don't have to be closed.
	// they will be garbage collected if no goroutines reference them any more
	// we will attempt to close anyway to free up so some resources if possible
	// in case one is already closed there will be a panic
	// we need to recover from that and do the same for the server
	// by using two anonymous functions this is possible
	// I created a snippet to verify: https://goplay.space/#m8-zwTuGrgS
	func() {
		defer recovery()
		close(t.client.DataChan())
	}()
	func() {
		defer recovery()
		close(t.server.DataChan())
	}()

	if t.decoder != nil {

		// create streams
		var (
			// client to server
			c2s = Stream{t.net, t.transport}
			// server to client
			s2c = Stream{t.net.Reverse(), t.transport.Reverse()}
		)

		// try to determine what type of raw tcp stream and update decoder
		// a first selection has been made based on the ports
		// now lets peek into the data stream to make a guess
		// TODO: always identify the decoder at this point, and treat everything as a raw TCP conn first?
		if _, ok := t.decoder.(*tcpReader); ok {
			switch {
			case bytes.Contains(t.server.ServiceBanner(), []byte("HTTP")):
				t.decoder = &httpReader{
					parent: t.client.(*tcpStreamReader).parent,
				}
			case bytes.Contains(t.server.ServiceBanner(), []byte("SSH")):
				t.decoder = &sshReader{
					parent: t.client.(*tcpStreamReader).parent,
				}
			case bytes.Contains(t.server.ServiceBanner(), []byte("POP server ready")):
				t.decoder = &pop3Reader{
					parent: t.client.(*tcpStreamReader).parent,
				}
			}
		}

		// call the associated decoder
		t.decoder.Decode(s2c, c2s)
	}

	logReassemblyDebug("%s: Stream closed\n", t.ident)

	// do not remove the connection to allow last ACK
	return false
}

func ReassemblePacket(packet gopacket.Packet, assembler *reassembly.Assembler) {

	// prevent passing any non TCP packets in here
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {

		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if udpLayer != nil {
			handleUDP(packet, udpLayer)
		}

		return
	}

	data := packet.Data()

	// lock to sync with read on destroy
	statsMutex.Lock()
	count++
	dataBytes += int64(len(data))
	statsMutex.Unlock()

	// defrag the IPv4 packet if required
	if !c.NoDefrag {
		ip4Layer := packet.Layer(layers.LayerTypeIPv4)
		if ip4Layer == nil {
			return
		}

		var (
			ip4         = ip4Layer.(*layers.IPv4)
			l           = ip4.Length
			newip4, err = defragger.DefragIPv4(ip4)
		)
		if err != nil {
			log.Fatalln("Error while de-fragmenting", err)
		} else if newip4 == nil {
			logReassemblyDebug("Fragment...\n")
			return
		}
		if newip4.Length != l {
			reassemblyStats.ipdefrag++
			logReassemblyDebug("Decoding re-assembled packet: %s\n", newip4.NextLayerType())
			pb, ok := packet.(gopacket.PacketBuilder)
			if !ok {
				panic("Not a PacketBuilder")
			}
			nextDecoder := newip4.NextLayerType()
			if err := nextDecoder.Decode(newip4.Payload, pb); err != nil {
				fmt.Println("failed to decode ipv4:", err)
			}
		}
	}

	tcp := tcpLayer.(*layers.TCP)
	if c.Checksum {
		err := tcp.SetNetworkLayerForChecksum(packet.NetworkLayer())
		if err != nil {
			log.Fatalf("Failed to set network layer for checksum: %s\n", err)
		}
	}
	statsMutex.Lock()
	reassemblyStats.totalsz += int64(len(tcp.Payload))
	statsMutex.Unlock()

	// for debugging:
	//AssembleWithContextTimeout(packet, assembler, tcp)

	assembler.AssembleWithContext(packet.NetworkLayer().NetworkFlow(), tcp, &Context{
		CaptureInfo: packet.Metadata().CaptureInfo,
	})

	statsMutex.Lock()
	doFlush := count%c.FlushEvery == 0
	statsMutex.Unlock()

	// flush connections in interval
	if doFlush {
		ref := packet.Metadata().CaptureInfo.Timestamp
		flushed, closed := assembler.FlushWithOptions(reassembly.FlushOptions{T: ref.Add(-c.ClosePendingTimeOut), TC: ref.Add(-c.CloseInactiveTimeOut)})
		utils.DebugLog.Printf("Forced flush: %d flushed, %d closed (%s)\n", flushed, closed, ref)
	}
}

// AssembleWithContextTimeout is a function that times out with a log message after a specified interval
// when the stream reassembly gets stuck
// used for debugging
func AssembleWithContextTimeout(packet gopacket.Packet, assembler *reassembly.Assembler, tcp *layers.TCP) {

	done := make(chan bool, 1)
	go func() {
		assembler.AssembleWithContext(packet.NetworkLayer().NetworkFlow(), tcp, &Context{
			CaptureInfo: packet.Metadata().CaptureInfo,
		})
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		spew.Dump(packet.Metadata().CaptureInfo)
		fmt.Println("HTTP AssembleWithContext timeout", packet.NetworkLayer().NetworkFlow(), packet.TransportLayer().TransportFlow())
		fmt.Println(assembler.Dump())
	}
}

func CleanupReassembly(wait bool, assemblers []*reassembly.Assembler) {

	cMu.Lock()
	if c.Debug {
		utils.ReassemblyLog.Println("StreamPool:")
		utils.ReassemblyLog.Println(StreamPool.DumpString())
	}
	cMu.Unlock()

	// wait for stream reassembly to finish
	if c.WaitForConnections || wait {
		if !Quiet {
			fmt.Print("\nwaiting for last streams to finish processing...")
		}

		// wait for remaining connections to finish processing
		select {
		case <-waitForConns():
			if !Quiet {
				fmt.Println(" done!")
			}
		case <-time.After(netcap.DefaultReassemblyTimeout):
			if !Quiet {
				fmt.Println(" timeout after", netcap.DefaultReassemblyTimeout)
			}
		}

		// flush assemblers
		// must be done after waiting for connections or there might be data loss
		for _, a := range assemblers {
			utils.ReassemblyLog.Printf("assembler flush: %d closed\n", a.FlushAll())
		}

		streamFactory.Lock()
		numTotal := len(streamFactory.streamReaders)
		streamFactory.Unlock()

		sp := new(tcpStreamProcessor)
		sp.initWorkers()
		sp.numTotal = numTotal

		// flush the remaining streams to disk
		for _, s := range streamFactory.streamReaders {
			if s != nil {
				sp.handleStream(s)
			}
		}
		if !Quiet {
			fmt.Println()
		}
		sp.wg.Wait()

		// process UDP streams
		if c.SaveConns {
			saveAllUDPConnections()
		}
	}

	// create a memory snapshot for debugging
	if c.MemProfile != "" {
		f, err := os.Create(c.MemProfile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("failed to write heap profile:", err)
		}
		if err := f.Close(); err != nil {
			log.Fatal("failed to close heap profile file:", err)
		}
	}

	// print stats if not quiet
	if !Quiet {
		errorsMapMutex.Lock()
		utils.ReassemblyLog.Printf("HTTPEncoder: Processed %v packets (%v bytes) in %v (errors: %v, type:%v)\n", count, dataBytes, time.Since(start), numErrors, len(errorsMap))
		errorsMapMutex.Unlock()

		// print configuration
		// print configuration as table
		tui.Table(utils.ReassemblyLogFileHandle, []string{"Reassembly Setting", "Value"}, [][]string{
			{"FlushEvery", strconv.Itoa(c.FlushEvery)},
			{"CloseInactiveTimeout", c.CloseInactiveTimeOut.String()},
			{"ClosePendingTimeout", c.ClosePendingTimeOut.String()},
			{"AllowMissingInit", strconv.FormatBool(c.AllowMissingInit)},
			{"IgnoreFsmErr", strconv.FormatBool(c.IgnoreFSMerr)},
			{"NoOptCheck", strconv.FormatBool(c.NoOptCheck)},
			{"Checksum", strconv.FormatBool(c.Checksum)},
			{"NoDefrag", strconv.FormatBool(c.NoDefrag)},
			{"WriteIncomplete", strconv.FormatBool(c.WriteIncomplete)},
		})

		printProgress(1, 1)

		statsMutex.Lock()
		rows := [][]string{}
		if !c.NoDefrag {
			rows = append(rows, []string{"IPdefrag", strconv.FormatInt(reassemblyStats.ipdefrag, 10)})
		}
		rows = append(rows, []string{"missed bytes", strconv.FormatInt(reassemblyStats.missedBytes, 10)})
		rows = append(rows, []string{"total packets", strconv.FormatInt(reassemblyStats.pkt, 10)})
		rows = append(rows, []string{"rejected FSM", strconv.FormatInt(reassemblyStats.rejectFsm, 10)})
		rows = append(rows, []string{"rejected Options", strconv.FormatInt(reassemblyStats.rejectOpt, 10)})
		rows = append(rows, []string{"reassembled bytes", strconv.FormatInt(reassemblyStats.sz, 10)})
		rows = append(rows, []string{"total TCP bytes", strconv.FormatInt(reassemblyStats.totalsz, 10)})
		rows = append(rows, []string{"conn rejected FSM", strconv.FormatInt(reassemblyStats.rejectConnFsm, 10)})
		rows = append(rows, []string{"reassembled chunks", strconv.FormatInt(reassemblyStats.reassembled, 10)})
		rows = append(rows, []string{"out-of-order packets", strconv.FormatInt(reassemblyStats.outOfOrderPackets, 10)})
		rows = append(rows, []string{"out-of-order bytes", strconv.FormatInt(reassemblyStats.outOfOrderBytes, 10)})
		rows = append(rows, []string{"biggest-chunk packets", strconv.FormatInt(reassemblyStats.biggestChunkPackets, 10)})
		rows = append(rows, []string{"biggest-chunk bytes", strconv.FormatInt(reassemblyStats.biggestChunkBytes, 10)})
		rows = append(rows, []string{"overlap packets", strconv.FormatInt(reassemblyStats.overlapPackets, 10)})
		rows = append(rows, []string{"overlap bytes", strconv.FormatInt(reassemblyStats.overlapBytes, 10)})
		rows = append(rows, []string{"saved TCP connections", strconv.FormatInt(reassemblyStats.savedTCPConnections, 10)})
		rows = append(rows, []string{"saved UDP connections", strconv.FormatInt(reassemblyStats.savedUDPConnections, 10)})
		rows = append(rows, []string{"numSoftware", strconv.FormatInt(reassemblyStats.numSoftware, 10)})
		rows = append(rows, []string{"numServices", strconv.FormatInt(reassemblyStats.numServices, 10)})
		statsMutex.Unlock()

		tui.Table(utils.ReassemblyLogFileHandle, []string{"TCP Stat", "Value"}, rows)

		errorsMapMutex.Lock()
		statsMutex.Lock()
		if numErrors != 0 {
			rows = [][]string{}
			for e := range errorsMap {
				rows = append(rows, []string{e, strconv.FormatUint(uint64(errorsMap[e]), 10)})
			}
			tui.Table(utils.ReassemblyLogFileHandle, []string{"Error Subject", "Count"}, rows)
		}
		utils.ReassemblyLog.Println("\nencountered", numErrors, "errors during processing.", "HTTP requests", requests, " responses", responses)
		statsMutex.Unlock()
		errorsMapMutex.Unlock()
	}
}

func waitForConns() chan struct{} {
	out := make(chan struct{})

	go func() {
		streamFactory.WaitGoRoutines()
		out <- struct{}{}
	}()

	return out
}

func (h *tcpConnection) ConversationRaw() []byte {

	h.Lock()
	defer h.Unlock()

	// do this only once, this method will be called once for each side of a connection
	if len(h.conversationRaw.Bytes()) == 0 {

		// concatenate both client and server data fragments
		h.merged = append(h.client.DataSlice(), h.server.DataSlice()...)

		// sort based on their timestamps
		sort.Sort(h.merged)

		// create the buffer with the entire conversation
		for _, d := range h.merged {

			//fmt.Println(h.ident, d.ac.GetCaptureInfo().Timestamp, d.ac.GetCaptureInfo().Length)

			h.conversationRaw.Write(d.raw)
			if d.dir == reassembly.TCPDirClientToServer {
				if c.Debug {
					var ts string
					if d.ac != nil {
						ts = "\n[" + d.ac.GetCaptureInfo().Timestamp.String() + "]\n"
					}
					h.conversationColored.WriteString(ansi.Red + string(d.raw) + ansi.Reset + ts)
				} else {
					h.conversationColored.WriteString(ansi.Red + string(d.raw) + ansi.Reset)
				}
			} else {
				if c.Debug {
					var ts string
					if d.ac != nil {
						ts = "\n[" + d.ac.GetCaptureInfo().Timestamp.String() + "]\n"
					}
					h.conversationColored.WriteString(ansi.Blue + string(d.raw) + ansi.Reset + ts)
				} else {
					h.conversationColored.WriteString(ansi.Blue + string(d.raw) + ansi.Reset)
				}
			}
		}
	}

	return h.conversationRaw.Bytes()
}

func (h *tcpConnection) ConversationColored() []byte {
	h.Lock()
	defer h.Unlock()
	return h.conversationColored.Bytes()
}
