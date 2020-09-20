package transform

import (
	"fmt"
	"github.com/dustin/go-humanize"
	"html"
	"strconv"

	"github.com/dreadl0ck/netcap/maltego"
	"github.com/dreadl0ck/netcap/types"
	"github.com/dreadl0ck/netcap/utils"
)

func toServices() {
	var typ string
	maltego.ServiceTransform(
		nil,
		func(lt maltego.LocalTransform, trx *maltego.Transform, service *types.Service, min, max uint64, path string, mac string, ipaddr string) {
			if typ == "" {
				typ = lt.Values["properties.servicetype"]
				if typ == "" {
					die("properties.servicetype not set", fmt.Sprint(lt.Values))
				}
			}

			if service.Name == typ {
				val := service.IP + ":" + strconv.Itoa(int(service.Port))
				if len(service.Vendor) > 0 {
					val += "\n" + service.Vendor
				}
				if len(service.Product) > 0 {
					val += "\n" + service.Product
				}
				//if len(service.Name) > 0 {
				//	val += "\n" + service.Name
				//}
				if len(service.Hostname) > 0 {
					val += "\n" + service.Hostname
				}

				ent := trx.AddEntityWithPath("netcap.Service", val, path)
				ent.AddProperty("timestamp", "Timestamp", maltego.Strict, utils.UnixTimeToUTC(service.Timestamp))
				ent.AddProperty("product", "Product", maltego.Strict, service.Product)
				ent.AddProperty("version", "Version", maltego.Strict, service.Version)
				ent.AddProperty("protocol", "Protocol", maltego.Strict, service.Protocol)
				ent.AddProperty("ip", "IP", maltego.Strict, service.IP)
				ent.AddProperty("port", "Port", maltego.Strict, strconv.Itoa(int(service.Port)))
				ent.AddProperty("hostname", "Hostname", maltego.Strict, service.Hostname)
				ent.AddProperty("bytesclient", "BytesClient", maltego.Strict, strconv.Itoa(int(service.BytesClient)))
				ent.AddProperty("bytesserver", "BytesServer", maltego.Strict, strconv.Itoa(int(service.BytesServer)))
				ent.AddProperty("vendor", "Vendor", maltego.Strict, service.Vendor)
				ent.AddProperty("name", "Name", maltego.Strict, service.Name)

				ent.SetLinkLabel(humanize.Bytes(uint64(service.BytesServer)) + " server\n" + humanize.Bytes(uint64(service.BytesClient)) + " client")
				// TODO: set thickness
				//ent.SetLinkThickness(maltego.GetThickness(uint64(service.BytesServer), min, max))

				if len(service.Banner) > 0 {
					ent.AddDisplayInformation("<pre style='color: dodgerblue;'>"+maltego.EscapeText(html.EscapeString(service.Banner))+"</pre>", "Transferred Data")
				}
			}
		},
		false,
	)
}
