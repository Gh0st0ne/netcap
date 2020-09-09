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

package maltego

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gogo/protobuf/proto"

	"github.com/dreadl0ck/netcap/defaults"
	netio "github.com/dreadl0ck/netcap/io"
	"github.com/dreadl0ck/netcap/types"
)

// HTTPCountFunc is a function that counts something over multiple HTTP audit records.
//goland:noinspection GoUnnecessarilyExportedIdentifiers
type HTTPCountFunc = func(http *types.HTTP, min, max *uint64)

// HTTPTransformationFunc is a transformation over HTTP audit records.
//goland:noinspection GoUnnecessarilyExportedIdentifiers
type HTTPTransformationFunc = func(lt LocalTransform, trx *Transform, http *types.HTTP, min, max uint64, path string, ip string)

// HTTPTransform applies a maltego transformation over HTTP audit records.
func HTTPTransform(count HTTPCountFunc, transform HTTPTransformationFunc, continueTransform bool) {
	var (
		lt               = ParseLocalArguments(os.Args[1:])
		path             = lt.Values["path"]
		ipaddr           = lt.Values["ipaddr"]
		dir              = filepath.Dir(path)
		httpAuditRecords = filepath.Join(dir, "HTTP.ncap.gz")
		trx              = Transform{}
	)

	f, err := os.Open(httpAuditRecords)
	if err != nil {

		log.Println(err)
		httpAuditRecords = strings.TrimSuffix(httpAuditRecords, ".gz")

		f, err = os.Open(httpAuditRecords)
		if err != nil {

			trx.AddUIMessage("audit record file not found: "+err.Error(), UIMessageFatal)
			fmt.Println(trx.ReturnOutput())
			log.Println("input file must be an audit record file")

			return
		}
	}

	// check if its an audit record file
	if !strings.HasSuffix(f.Name(), defaults.FileExtensionCompressed) && !strings.HasSuffix(f.Name(), defaults.FileExtension) {
		die(errUnexpectedFileType, f.Name())
	}

	r, err := netio.Open(httpAuditRecords, defaults.BufferSize)
	if err != nil {
		panic(err)
	}

	// read netcap header
	header, errFileHeader := r.ReadHeader()
	if errFileHeader != nil {
		die("failed to read file header", errFileHeader.Error())
	}

	if header.Type != types.Type_NC_HTTP {
		die("file does not contain HTTP records", header.Type.String())
	}

	var (
		http = new(types.HTTP)
		pm   proto.Message
		ok   bool
	)

	pm = http

	if _, ok = pm.(types.AuditRecord); !ok {
		panic("type does not implement types.AuditRecord interface")
	}

	var (
		min uint64 = 10000000
		max uint64 = 0
	)

	if count != nil {
		for {
			err = r.Next(http)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			} else if err != nil {
				panic(err)
			}

			count(http, &min, &max)
		}

		err = r.Close()
		if err != nil {
			log.Println("failed to close audit record file: ", err)
		}
	}

	r, err = netio.Open(httpAuditRecords, defaults.BufferSize)
	if err != nil {
		panic(err)
	}

	// read netcap header - ignore err as it has been checked before
	_, _ = r.ReadHeader()

	for {
		err = r.Next(http)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		} else if err != nil {
			panic(err)
		}

		transform(lt, &trx, http, min, max, path, ipaddr)
	}

	err = r.Close()
	if err != nil {
		log.Println("failed to close audit record file: ", err)
	}

	if !continueTransform {
		trx.AddUIMessage("completed!", UIMessageInform)
		fmt.Println(trx.ReturnOutput())
	}
}
