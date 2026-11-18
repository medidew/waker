// wolcheck listens on a UDP port and validates incoming Wake-on-LAN magic
// packets against the exact byte layout wol.Packet produces. livetest.sh uses
// it on loopback to prove a real send/receive round trip; it also works as a
// manual listener on a LAN while debugging a real wake:
//
//	wolcheck -mac AA:BB:CC:DD:EE:FF -listen 0.0.0.0:7
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"waker/internal/wol"
)

func main() {
	var (
		mac     = flag.String("mac", "", "MAC address the magic packet must be for (required)")
		want    = flag.Int("want", 1, "how many valid packets to wait for")
		timeout = flag.Duration("timeout", 15*time.Second, "give up after this long")
		listen  = flag.String("listen", "127.0.0.1:0", "UDP address to bind (port 0 picks a free one)")
	)
	flag.Parse()

	if *mac == "" {
		log.Fatal("wolcheck: -mac is required")
	}
	expected, err := wol.Packet(*mac)
	if err != nil {
		log.Fatalf("wolcheck: %v", err)
	}

	addr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		log.Fatalf("wolcheck: bad -listen: %v", err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Fatalf("wolcheck: %v", err)
	}
	defer conn.Close()

	// First line is parsed by livetest.sh (sed, anchored), so keep it bare.
	fmt.Printf("listening on %s\n", conn.LocalAddr())
	fmt.Printf("want %d valid magic packet(s) for %s\n", *want, *mac)
	conn.SetDeadline(time.Now().Add(*timeout))

	buf := make([]byte, 1<<16)
	valid, seen := 0, 0
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break // deadline: report below
			}
			log.Printf("wolcheck: read: %v", err)
			continue
		}
		seen++
		if n == len(expected) && bytes.Equal(buf[:n], expected) {
			valid++
			fmt.Printf("packet %d: ok (%d bytes)\n", seen, n)
			if valid >= *want {
				fmt.Printf("ok: received %d of %d valid magic packet(s) for %s\n", valid, *want, *mac)
				os.Exit(0)
			}
			continue
		}
		first := n
		if first > 16 {
			first = 16
		}
		fmt.Printf("packet %d: invalid (%d bytes, first %d bytes: % x)\n", seen, n, first, buf[:first])
	}
	fmt.Printf("FAIL: received %d of %d valid magic packet(s) for %s\n", valid, *want, *mac)
	os.Exit(1)
}
