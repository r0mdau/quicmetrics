package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	_ "net/http/pprof"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/narqo/go-dogstatsd-parser"
	"github.com/r0mdau/quicmetrics/internal/testdata"
)

const addr = "0.0.0.0:6121"

type QuicMetrics map[string]*dogstatsd.Metric

var qm = QuicMetrics{}

func setupHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			log.Fatal(err)
		}
		str := string(raw)

		m, err := dogstatsd.Parse(str)
		if err != nil {
			log.Fatal(err)
		}

		mKey := uniqueMetricKey(m.Name, str)
		if _, ok := qm[mKey]; !ok {
			qm[mKey] = m
		} else {
			if m.Type == dogstatsd.Counter {
				qm[mKey].Value = int64(qm[mKey].Value.(int64) + m.Value.(int64))
			}
		}
		outputQuicMetrics(mKey, qm)
		io.WriteString(w, "OK")
	})

	mux.HandleFunc("/demo/echo", func(w http.ResponseWriter, r *http.Request) {
		received, err := io.ReadAll(r.Body)
		fmt.Println("received", string(received))
		if err != nil {
			fmt.Printf("error reading body while handling /echo: %s\n", err.Error())
		}
		io.WriteString(w, "Hello world !")
	})

	return mux
}

func main() {
	handler := setupHandler()
	quicConf := &quic.Config{}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		var err error
		server := http3.Server{
			Handler:    handler,
			Addr:       addr,
			QuicConfig: quicConf,
		}
		err = server.ListenAndServeTLS(testdata.GetCertificatePaths())
		if err != nil {
			fmt.Println(err)
		}
		wg.Done()
	}()
	wg.Wait()
}

func uniqueMetricKey(name, raw string) string {
	splitTags := strings.SplitN(raw, "#", 2)
	return fmt.Sprintf("%s%s", name, splitTags[1])
}

func outputQuicMetrics(mKey string, qm QuicMetrics) {
	m := qm[mKey]
	fmt.Println(mKey, m.Value)
	for k, v := range m.Tags {
		fmt.Println(k + " - " + v)
	}
}
