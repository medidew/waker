package wol

import (
	"bytes"
	"testing"
)

func TestPacket(t *testing.T) {
	pkt, err := Packet("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("Packet: %v", err)
	}
	if len(pkt) != PacketLen {
		t.Fatalf("len = %d, want %d", len(pkt), PacketLen)
	}
	for i := 0; i < 64; i++ {
		if pkt[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xff", i, pkt[i])
		}
	}
	wantMAC := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(pkt[64+i*6:70+i*6], wantMAC) {
			t.Fatalf("mac copy %d mismatch: %x", i, pkt[64+i*6:])
		}
	}
}

func TestPacketInvalidMAC(t *testing.T) {
	for _, bad := range []string{"", "aa:bb", "zz:bb:cc:dd:ee:ff"} {
		if _, err := Packet(bad); err == nil {
			t.Errorf("Packet(%q): expected error, got nil", bad)
		}
	}
}
