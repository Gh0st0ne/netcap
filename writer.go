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

package netcap

import (
	"bufio"
	gzip "github.com/klauspost/pgzip"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/dreadl0ck/netcap/delimited"
	"github.com/dreadl0ck/netcap/io"
	"github.com/dreadl0ck/netcap/types"
	"github.com/golang/protobuf/proto"
)

/*
 *	Type Definition
 */

// Writer is a structure that supports writing audit records to disk
type Writer struct {

	// Name of the associated audit record type
	Name string

	// private fields
	file      *os.File
	bWriter   *bufio.Writer
	gWriter   *gzip.Writer
	dWriter   *delimited.Writer
	aWriter   *io.AtomicDelimitedWriter
	cWriter   *io.ChanWriter
	csvWriter *io.CSVWriter

	// configuration
	compress     bool
	buffer       bool
	csv          bool
	out          string
	IsChanWriter bool

	mu sync.Mutex
}

/*
 *	Constructor
 */

// NewWriter initializes and configures a new Writer
func NewWriter(name string, buffer, compress, csv bool, out string, writeChan bool, memBufferSize int) *Writer {

	w := &Writer{}
	w.Name = name
	w.compress = compress
	w.buffer = buffer
	w.csv = csv
	w.out = out
	w.IsChanWriter = writeChan

	if memBufferSize <= 0 {
		memBufferSize = DefaultBufferSize
	}

	if csv {

		// create file
		if compress {
			w.file = CreateFile(filepath.Join(out, w.Name), ".csv.gz")
		} else {
			w.file = CreateFile(filepath.Join(out, w.Name), ".csv")
		}

		if buffer {

			w.bWriter = bufio.NewWriterSize(w.file, memBufferSize)

			if compress {
				w.gWriter = gzip.NewWriter(w.bWriter)
				w.csvWriter = io.NewCSVWriter(w.gWriter)
			} else {
				w.csvWriter = io.NewCSVWriter(w.bWriter)
			}
		} else {
			if compress {
				w.gWriter = gzip.NewWriter(w.file)
				w.csvWriter = io.NewCSVWriter(w.gWriter)
			} else {
				w.csvWriter = io.NewCSVWriter(w.file)
			}
		}

		if w.gWriter != nil {
			// To get any performance gains, you should at least be compressing more than 1 megabyte of data at the time.
			// You should at least have a block size of 100k and at least a number of blocks that match the number of cores
			// your would like to utilize, but about twice the number of blocks would be the best.
			if err := w.gWriter.SetConcurrency(DefaultCompressionBlockSize, runtime.GOMAXPROCS(0)*2); err != nil {
				log.Fatal("failed to configure compression package: ", err)
			}
		}

		return w
	}

	if writeChan && buffer || writeChan && compress {
		panic("buffering or compression cannot be activated when running using writeChan")
	}

	// write into channel OR into file
	if writeChan {
		w.cWriter = io.NewChanWriter()
	} else {
		if compress {
			w.file = CreateFile(filepath.Join(out, w.Name), ".ncap.gz")
		} else {
			w.file = CreateFile(filepath.Join(out, w.Name), ".ncap")
		}
	}

	// buffer data?
	if buffer {

		w.bWriter = bufio.NewWriterSize(w.file, DefaultBufferSize)
		if compress {
			w.gWriter = gzip.NewWriter(w.bWriter)
			w.dWriter = delimited.NewWriter(w.gWriter)
		} else {
			w.dWriter = delimited.NewWriter(w.bWriter)
		}
	} else {
		if compress {
			w.gWriter = gzip.NewWriter(w.file)
			w.dWriter = delimited.NewWriter(w.gWriter)
		} else {
			if writeChan {
				// write into channel writer without compression
				w.dWriter = delimited.NewWriter(w.cWriter)
			} else {
				w.dWriter = delimited.NewWriter(w.file)
			}
		}
	}
	w.aWriter = io.NewAtomicDelimitedWriter(w.dWriter)

	if w.gWriter != nil {
		// To get any performance gains, you should at least be compressing more than 1 megabyte of data at the time.
		// You should at least have a block size of 100k and at least a number of blocks that match the number of cores
		// your would like to utilize, but about twice the number of blocks would be the best.
		if err := w.gWriter.SetConcurrency(DefaultCompressionBlockSize, runtime.GOMAXPROCS(0)*2); err != nil {
			log.Fatal("failed to configure compression package: ", err)
		}
	}

	return w
}

/*
 *	Protobuf
 */

// WriteProto writes a protobuf message
func (w *Writer) WriteProto(msg proto.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.aWriter.PutProto(msg)
}

/*
 *	CSV
 */

// WriteCSV writes a csv record
func (w *Writer) WriteCSV(msg proto.Message) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.csvWriter.WriteRecord(msg)
}

// WriteCSVHeader writes a CSV record
func (w *Writer) WriteCSVHeader(msg proto.Message) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.csvWriter.WriteHeader(msg)
}

/*
 *	Utils
 */

func (w *Writer) Write(msg proto.Message) error {
	if w.csv {
		// write as csv
		_, err := w.WriteCSV(msg)
		if err != nil {
			panic(err)
		}
	} else {
		// write protobuf
		err := w.WriteProto(msg)
		if err != nil {
			panic(err)
		}
	}
	return nil
}

func (w *Writer) WriteHeader(t types.Type, source string, version string, includesPayloads bool) error {
	if w.csv {
		// write as csv
		_, err := w.WriteCSVHeader(InitRecord(t))
		if err != nil {
			panic(err)
		}
	} else {
		// write protobuf
		err := w.WriteProto(NewHeader(t, source, version, includesPayloads))
		if err != nil {
			panic(err)
		}
	}
	return nil
}

type flushableWriter interface {
	Flush() error
}

func FlushWriters(writers ...flushableWriter) {
	for _, w := range writers {
		err := w.Flush()
		if err != nil {
			panic(err)
		}
	}
}

func CloseGzipWriters(writers ...*gzip.Writer) {
	for _, w := range writers {
		err := w.Flush()
		if err != nil {
			panic(err)
		}
		err = w.Close()
		if err != nil {
			panic(err)
		}
	}
}

func (w *Writer) Close() (name string, size int64) {
	if w.compress {
		w.mu.Lock()
		CloseGzipWriters(w.gWriter)
		w.mu.Unlock()
	}
	if w.buffer {
		FlushWriters(w.bWriter)
	}
	return CloseFile(w.out, w.file, w.Name)
}

// GetChan returns a channel for receiving bytes
func (w *Writer) GetChan() <-chan []byte {
	return w.cWriter.Chan()
}

func (w *Writer) IsCSV() bool {
	return w.csv
}
