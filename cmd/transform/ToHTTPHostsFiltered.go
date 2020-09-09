package transform

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"log"
	"os"

	"github.com/dreadl0ck/netcap/maltego"
	"github.com/dreadl0ck/netcap/resolvers"
	"github.com/dreadl0ck/netcap/types"
)

func toHTTPHostsFiltered() {

	resolverLog := zap.New(zapcore.NewNopCore())
	defer func() {
		err := resolverLog.Sync()
		if err != nil {
			log.Println(err)
		}
	}()

	resolvers.SetLogger(resolverLog)

	stdOut := os.Stdout
	os.Stdout = os.Stderr
	resolvers.InitDNSWhitelist()
	os.Stdout = stdOut

	maltego.HTTPTransform(
		nil,
		func(lt maltego.LocalTransform, trx *maltego.Transform, http *types.HTTP, min, max uint64, path string, ipaddr string) {
			if http.SrcIP != ipaddr {
				return
			}
			if http.Host != "" {
				if !resolvers.IsWhitelistedDomain(http.Host) {
					ent := trx.AddEntityWithPath("netcap.Website", http.Host, path)
					ent.AddProperty("ipaddr", "IPAddress", "strict", ipaddr)

				}
			}
		},
		false,
	)
}
