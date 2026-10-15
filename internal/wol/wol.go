// Package wol builds and sends Wake-on-LAN magic packets.
package wol

import (
	"fmt"
	"net"
	"syscall"
)

// PacketLen is the length of a WoL magic packet in bytes: 64×0xFF + MAC×16.
// (The minimal spec form is 6×0xFF + MAC×16 = 102; the longer prefix is what
// most tools send and is accepted by all NICs.)
const PacketLen = 160

// ParseMAC validates and normalizes a MAC address string.
func ParseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC address %q: %w", s, err)
	}
	return mac, nil
}

// Packet builds the 102-byte magic packet for the given MAC address.
func Packet(mac string) ([]byte, error) {
	m, err := ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	pkt := make([]byte, PacketLen)
	for i := 0; i < 64; i++ {
		pkt[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(pkt[64+i*6:], m)
	}
	return pkt, nil
}

// Send transmits the magic packet for mac to addr (e.g. "192.168.1.255:9").
// SO_BROADCAST is enabled on the socket so broadcast destinations work.
func Send(mac, addr string) error {
	pkt, err := Packet(mac)
	if err != nil {
		return err
	}
	conn, err := net.Dial("udp4", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	uconn := conn.(*net.UDPConn)
	rawConn, err := uconn.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscall conn: %w", err)
	}
	var soErr error
	rawConn.Control(func(fd uintptr) {
		soErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
	if soErr != nil {
		return fmt.Errorf("enable broadcast: %w", soErr)
	}
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("send to %s: %w", addr, err)
	}
	return nil
}
