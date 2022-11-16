package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"

	"github.com/lucas-clemente/quic-go"
)

const addr = "localhost:4242"

const message1 = "users.online:1|c|#country:china,city:beijing"
const message2 = "users.online:2|c|#country:usa,city:losangeles"

// We start a server echoing data on the first stream the client opens,
// then connect with a client, send the message, and wait for its receipt.
func main() {
	err := clientMain()
	if err != nil {
		panic(err)
	}
}

func clientMain() error {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-echo-example"},
	}
	conn, err := quic.DialAddr(addr, tlsConf, nil)
	if err != nil {
		return err
	}

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer stream.Close()

	err = sendStream(stream, message1)
	if err != nil {
		return err
	}
	err = sendStream(stream, message2)
	if err != nil {
		return err
	}

	return nil
}

func sendStream(stream quic.Stream, message string) error {
	fmt.Printf("Client: Sending '%s'\n", message)
	_, err := stream.Write([]byte(message))
	if err != nil {
		return err
	}

	buf := make([]byte, len(message))
	_, err = io.ReadFull(stream, buf)
	if err != nil {
		return err
	}
	fmt.Printf("Client: Got '%s'\n", buf)
	return nil
}
