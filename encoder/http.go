/*
 * NETCAP - Traffic Analysis Framework
 * Copyright (c) 2017 Philipp Mieden <dreadl0ck [at] protonmail [dot] ch>
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package encoder

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dreadl0ck/netcap/types"
	"github.com/evilsocket/islazy/tui"
	"github.com/golang/protobuf/proto"
	"github.com/google/gopacket"
	"github.com/google/gopacket/ip4defrag"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/reassembly"
)

var (
	defragger     = ip4defrag.NewIPv4Defragmenter()
	streamFactory = &tcpStreamFactory{doHTTP: !*nohttp}
	streamPool    = reassembly.NewStreamPool(streamFactory)
	assembler     = reassembly.NewAssembler(streamPool)

	count     = 0
	dataBytes = int64(0)
	start     = time.Now()

	errorsMap      = make(map[string]uint)
	errorsMapMutex sync.Mutex

	HTTPActive bool
	reqMutex   sync.Mutex
	httpReqMap = make(map[Stream][]*http.Request)

	resMutex   sync.Mutex
	httpResMap = make(map[Stream][]*http.Response)
)

type Stream struct {
	a gopacket.Flow
	b gopacket.Flow
}

func (s Stream) Reverse() Stream {
	return Stream{
		s.a.Reverse(),
		s.b.Reverse(),
	}
}

func (s Stream) String() string {
	return s.a.String() + " : " + s.b.String()
}

// DecodeHTTP passes TCP packets to the TCP stream reassembler
// in order to decode HTTP request and responses
// CAUTION: this function must be called sequentially,
// because the stream reassembly implementation currently does not handle out of order packets
func DecodeHTTP(packet gopacket.Packet) {

	count++
	data := packet.Data()

	// lock to sync with read on destroy
	errorsMapMutex.Lock()
	dataBytes += int64(len(data))
	errorsMapMutex.Unlock()

	// defrag the IPv4 packet if required
	if !*nodefrag {
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
			Debug("Fragment...\n")
			return
		}
		if newip4.Length != l {
			reassemblyStats.ipdefrag++
			Debug("Decoding re-assembled packet: %s\n", newip4.NextLayerType())
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

	tcp := packet.Layer(layers.LayerTypeTCP)
	if tcp != nil {
		tcp := tcp.(*layers.TCP)
		if *checksum {
			err := tcp.SetNetworkLayerForChecksum(packet.NetworkLayer())
			if err != nil {
				log.Fatalf("Failed to set network layer for checksum: %s\n", err)
			}
		}
		c := Context{
			CaptureInfo: packet.Metadata().CaptureInfo,
		}
		reassemblyStats.totalsz += len(tcp.Payload)
		assembler.AssembleWithContext(packet.NetworkLayer().NetworkFlow(), tcp, &c)
	}

	// flush connections in interval
	if count%*flushevery == 0 {
		ref := packet.Metadata().CaptureInfo.Timestamp
		// flushed, closed :=
		assembler.FlushWithOptions(reassembly.FlushOptions{T: ref.Add(-timeout), TC: ref.Add(-closeTimeout)})
		// fmt.Printf("Forced flush: %d flushed, %d closed (%s)\n", flushed, closed, ref, ref.Add(-timeout))
	}
}

var httpEncoder = CreateCustomEncoder(types.Type_NC_HTTP, "HTTP", func(d *CustomEncoder) error {

	if *debug {
		outputLevel = 2
	} else if *verbose {
		outputLevel = 1
	} else if *quiet {
		outputLevel = -1
	}

	HTTPActive = true

	return nil
}, func(packet gopacket.Packet) proto.Message {
	return nil
}, func(d *CustomEncoder) error {

	errorsMapMutex.Lock()
	fmt.Fprintf(os.Stderr, "HTTPEncoder: Processed %v packets (%v bytes) in %v (errors: %v, type:%v)\n", count, dataBytes, time.Since(start), numErrors, len(errorsMap))
	errorsMapMutex.Unlock()

	closed := assembler.FlushAll()
	fmt.Printf("Final flush: %d closed\n", closed)
	if outputLevel >= 2 {
		streamPool.Dump()
	}

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			return err
		}
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatal("failed to write heap profile:", err)
		}
		if err := f.Close(); err != nil {
			log.Fatal("failed to close heap profile file:", err)
		}
	}

	streamFactory.WaitGoRoutines()
	Debug("%s\n", assembler.Dump())

	reqMutex.Lock()
	resMutex.Lock()

	var (
		total int64 = 0
		sum         = int64(len(httpResMap) + len(httpReqMap))
	)

	if sum == 0 {
		goto done
	}

	// range responses
	for s, resArr := range httpResMap {

		printProgress(total, sum)

		var newRes []*http.Response
		for _, res := range resArr {

			// populate types.HTTP with all infos from request
			h := &types.HTTP{
				ResContentLength: int32(res.ContentLength),
				ContentType:      res.Header.Get("Content-Type"),
				StatusCode:       int32(res.StatusCode),
			}

			if res.Request != nil {
				setRequest(h, res.Request)

				atomic.AddInt64(&d.numRecords, 1)

				if d.csv {
					_, err := d.csvWriter.WriteRecord(h)
					if err != nil {
						errorMap.Inc(err.Error())
					}
				} else {
					err := d.aWriter.PutProto(h)
					if err != nil {
						errorMap.Inc(err.Error())
					}
				}

				total++
			} else {
				newRes = append(newRes, res)
			}
		}

		// newRes are all responses for which there is no request
		httpResMap[s] = newRes
	}

	// process all leftover responses
	// for _, resArr := range httpResMap {
	// 	printProgress(total, sum)
	// 	for _, res := range resArr {

	// 		// populate types.HTTP with all infos from response
	// 		h := &types.HTTP{
	// 			ResContentLength: int32(res.ContentLength),
	// 			ContentType:      res.Header.Get("Content-Type"),
	// 			StatusCode:       int32(res.StatusCode),
	// 		}

	// 		err := d.aWriter.PutProto(h)
	// 		if err != nil {
	// 			errorMap.Inc(err.Error())
	// 		}
	// 		total++
	// 	}
	// }

	// process all leftover requests
	for _, reqArr := range httpReqMap {
		printProgress(total, sum)
		for _, req := range reqArr {
			total++
			if req != nil {
				h := &types.HTTP{}
				setRequest(h, req)

				atomic.AddInt64(&d.numRecords, 1)
				err := d.aWriter.PutProto(h)
				if err != nil {
					errorMap.Inc(err.Error())
				}
			}
		}
	}

done:

	reqMutex.Unlock()
	resMutex.Unlock()

	printProgress(1, 1)
	fmt.Println("")

	rows := [][]string{}
	if !*nodefrag {
		rows = append(rows, []string{"IPdefrag", strconv.Itoa(reassemblyStats.ipdefrag)})
	}
	rows = append(rows, []string{"missed bytes", strconv.Itoa(reassemblyStats.missedBytes)})
	rows = append(rows, []string{"total packets", strconv.Itoa(reassemblyStats.pkt)})
	rows = append(rows, []string{"rejected FSM", strconv.Itoa(reassemblyStats.rejectFsm)})
	rows = append(rows, []string{"rejected Options", strconv.Itoa(reassemblyStats.rejectOpt)})
	rows = append(rows, []string{"reassembled bytes", strconv.Itoa(reassemblyStats.sz)})
	rows = append(rows, []string{"total TCP bytes", strconv.Itoa(reassemblyStats.totalsz)})
	rows = append(rows, []string{"conn rejected FSM", strconv.Itoa(reassemblyStats.rejectConnFsm)})
	rows = append(rows, []string{"reassembled chunks", strconv.Itoa(reassemblyStats.reassembled)})
	rows = append(rows, []string{"out-of-order packets", strconv.Itoa(reassemblyStats.outOfOrderPackets)})
	rows = append(rows, []string{"out-of-order bytes", strconv.Itoa(reassemblyStats.outOfOrderBytes)})
	rows = append(rows, []string{"biggest-chunk packets", strconv.Itoa(reassemblyStats.biggestChunkPackets)})
	rows = append(rows, []string{"biggest-chunk bytes", strconv.Itoa(reassemblyStats.biggestChunkBytes)})
	rows = append(rows, []string{"overlap packets", strconv.Itoa(reassemblyStats.overlapPackets)})
	rows = append(rows, []string{"overlap bytes", strconv.Itoa(reassemblyStats.overlapBytes)})

	fmt.Println("TCP stats:")
	tui.Table(os.Stdout, []string{"Description", "Value"}, rows)

	if numErrors != 0 {
		rows = [][]string{}
		fmt.Printf("\nErrors: %d\n", numErrors)
		for e := range errorsMap {
			rows = append(rows, []string{e, strconv.FormatUint(uint64(errorsMap[e]), 10)})
		}
		tui.Table(os.Stdout, []string{"Error", "Count"}, rows)
	}

	fmt.Println("\nflushed", total, "http events.", "requests", requests, "responses", responses)

	return nil
})

// set HTTP request on types.HTTP
func setRequest(h *types.HTTP, req *http.Request) {
	h.Timestamp = req.Header.Get("netcap-ts")
	h.Proto = req.Proto
	h.Method = req.Method
	h.Host = req.Host
	h.UserAgent = strings.Replace(req.UserAgent(), ",", "(comma)", -1)
	h.Referer = strings.Replace(req.Referer(), ",", "(comma)", -1)
	h.ReqContentLength = int32(req.ContentLength)
	h.URL = strings.Replace(req.URL.String(), ",", "(comma)", -1)
	h.SrcIP = req.Header.Get("netcap-clientip")
	h.DstIP = req.Header.Get("netcap-serverip")
}
