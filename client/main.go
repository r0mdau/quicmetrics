package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/r0mdau/quicmetrics/internal/testdata"
	"github.com/scaleway/scaleway-sdk-go/logger"
)

const addr = "https://localhost:6121"

var messages = []string{
	"users.online:1|c|#country:china,city:beijing",
	"users.online:2|c|#country:usa,city:losangeles",
}

func main() {

	pool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatal(err)
	}
	testdata.AddRootCA(pool)

	var qconf quic.Config

	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: true,
		},
		QuicConfig: &qconf,
	}
	defer roundTripper.Close()
	hclient := &http.Client{
		Transport: roundTripper,
	}

	var wg sync.WaitGroup
	wg.Add(len(messages))
	for _, message := range messages {
		go func(message string) {
			myReader := strings.NewReader(message)
			rsp, err := hclient.Post(fmt.Sprintf("%s/metrics", addr), "text", myReader)
			if err != nil {
				log.Fatal(err)
			}

			body := &bytes.Buffer{}
			_, err = io.Copy(body, rsp.Body)
			if err != nil {
				log.Fatal(err)
			}

			logger.Infof("Response Body:")
			logger.Infof("%s", body.Bytes())

			wg.Done()
		}(message)
	}
	wg.Wait()
}
