package transform

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/dreadl0ck/netcap/maltego"
	"github.com/dreadl0ck/netcap/types"
	"github.com/dreadl0ck/netcap/utils"
)

func toEmails() {
	maltego.MailTransform(
		nil,
		func(lt maltego.LocalTransform, trx *maltego.Transform, m *types.Mail, min, max uint64, path string, ipaddr string) {

			var buf bytes.Buffer
			err := xml.EscapeText(&buf, []byte(m.Subject+"\n"+m.From))
			if err != nil {
				fmt.Println(err)
			}

			ent := trx.AddEntityWithPath("netcap.Email", buf.String(), path)

			var attachments string
			for _, p := range m.Body {
				cType := p.Header["Content-Type"]
				if cType != "" && strings.Contains(p.Header["Content-Disposition"], "attachment") {
					attachments += "<br>Attachment Content Type: " + cType + "<br>"
					attachments += "Filename: " + p.Filename + "<br><br>"
					if p.Content != "" && p.Content != "\n" {
						attachments += p.Content + "<br>"
					}
				}
			}

			var body string
			for _, p := range m.Body {
				cType := p.Header["Content-Type"]
				if strings.Contains(cType, "text/plain") || cType == "" {
					body += p.Content + "\n"
				}
			}

			// escape XML
			buf.Reset()
			err = xml.EscapeText(&buf, []byte(body))
			if err != nil {
				fmt.Println(err)
			}

			di := "<h3>EMail: " + m.Subject + "</h3><p>Timestamp First: " + utils.UnixTimeToUTC(m.Timestamp) + "</p><p>From: " + m.From + "</p><p>To: " + m.To + "</p><p>Text: " + buf.String() + "</p><p>Additional parts: " + attachments + "</p>"
			ent.AddDisplayInformation(di, "Netcap Info")

		},
	)
}