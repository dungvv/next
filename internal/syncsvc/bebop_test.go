package syncsvc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func frame(discr uint8, fields []byte) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(fields)))
	out = append(out, discr)
	return append(out, fields...)
}

func u32b(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func u64b(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

func TestDecodePeerUpdate(t *testing.T) {
	// PeerUpdate{updates:[[1,2,3],[4]], id:"abc"}
	fields := u32b(2) // 2 updates
	fields = append(fields, u32b(3)...)
	fields = append(fields, 1, 2, 3)
	fields = append(fields, u32b(1)...)
	fields = append(fields, 4)
	fields = append(fields, u32b(3)...)
	fields = append(fields, 'a', 'b', 'c')

	m, err := decodeFromPeer(frame(peerUpdate, fields))
	if err != nil {
		t.Fatal(err)
	}
	if m.kind != peerUpdate || m.id != "abc" || len(m.updates) != 2 {
		t.Fatalf("bad decode: %+v", m)
	}
	if !bytes.Equal(m.updates[0], []byte{1, 2, 3}) || !bytes.Equal(m.updates[1], []byte{4}) {
		t.Fatalf("bad updates: %v", m.updates)
	}
}

func TestDecodeOthers(t *testing.T) {
	m, err := decodeFromPeer(frame(peerAwareness, append(u32b(2), 9, 9)))
	if err != nil || m.kind != peerAwareness || !bytes.Equal(m.awareness, []byte{9, 9}) {
		t.Fatalf("awareness: %v %+v", err, m)
	}
	m, err = decodeFromPeer(frame(peerRequestSince, append(u32b(2), 7, 8)))
	if err != nil || m.kind != peerRequestSince || !bytes.Equal(m.vv, []byte{7, 8}) {
		t.Fatalf("requestSince: %v %+v", err, m)
	}
	m, err = decodeFromPeer(frame(peerRequestSnapshot, nil))
	if err != nil || m.kind != peerRequestSnapshot {
		t.Fatalf("requestSnapshot: %v %+v", err, m)
	}
	m, err = decodeFromPeer(frame(peerRegisterID, u64b(42)))
	if err != nil || m.kind != peerRegisterID || m.peerID != 42 {
		t.Fatalf("registerId: %v %+v", err, m)
	}
}

func TestDecodeBad(t *testing.T) {
	if _, err := decodeFromPeer([]byte{1, 2}); err == nil {
		t.Fatal("short frame accepted")
	}
	if _, err := decodeFromPeer(frame(99, nil)); err == nil {
		t.Fatal("unknown discriminant accepted")
	}
	// truncated field
	if _, err := decodeFromPeer(frame(peerAwareness, u32b(10))); err == nil {
		t.Fatal("truncated bytes accepted")
	}
}

func TestEncodeRemoteRoundTrip(t *testing.T) {
	// Each encoder must produce len-prefixed frame with right discriminant.
	check := func(name string, got []byte, discr uint8, fields []byte) {
		t.Helper()
		if len(got) < 5 || binary.LittleEndian.Uint32(got[:4]) != uint32(len(fields)) ||
			got[4] != discr || !bytes.Equal(got[5:], fields) {
			t.Fatalf("%s: bad frame %x", name, got)
		}
	}
	check("InitialSync", encInitialSync([]byte{1}, []byte{2}), remoteInitialSync,
		append(append(u32b(1), 1), append(u32b(1), 2)...))
	check("Update", encRemoteUpdate([]byte{5}), remoteUpdate, append(u32b(1), 5))
	check("Ack", encRemoteUpdateAck("hi"), remoteUpdateAck, append(u32b(2), 'h', 'i'))
	check("UpdateSince", encRemoteUpdateSince([]byte{1}, []byte{2}), remoteUpdateSince,
		append(append(u32b(1), 1), append(u32b(1), 2)...))
}

func TestDecodeInitRequest(t *testing.T) {
	// InitializeFromSnapshotRequest{snapshot:[9,9]}: u32 reclen + u32 len + bytes
	body := append(u32b(6), u32b(2)...)
	body = append(body, 9, 9)
	snap, err := decodeInitializeFromSnapshotRequest(body)
	if err != nil || !bytes.Equal(snap, []byte{9, 9}) {
		t.Fatalf("init req: %v %x", err, snap)
	}
}

// Attacker-controlled u32 length/count prefixes must be rejected before any
// allocation sized by them happens.
func TestDecodeLengthBounds(t *testing.T) {
	// byte[] field claiming > maxBebopByteLen bytes
	if _, err := decodeFromPeer(frame(peerAwareness, u32b(maxBebopByteLen+1))); err == nil {
		t.Fatal("oversized awareness field accepted")
	}
	// byte[][] claiming a huge element count (no elements follow)
	if _, err := decodeFromPeer(frame(peerUpdate, u32b(maxBebopElems+1))); err == nil {
		t.Fatal("oversized updates count accepted")
	}
	// count within limit but elements missing — must fail, not allocate 10M
	if _, err := decodeFromPeer(frame(peerUpdate, u32b(1<<20))); err == nil {
		t.Fatal("truncated updates accepted")
	}
	// string id claiming > maxBebopByteLen
	fields := append(u32b(0), u32b(maxBebopByteLen+1)...)
	if _, err := decodeFromPeer(frame(peerUpdate, fields)); err == nil {
		t.Fatal("oversized id string accepted")
	}
	// byte[] lying about its length vs. what's in the frame
	if _, err := decodeFromPeer(frame(peerAwareness, append(u32b(100), 1, 2))); err == nil {
		t.Fatal("overrun byte field accepted")
	}
	// record-level: init request with oversized snapshot len
	if _, err := decodeInitializeFromSnapshotRequest(append(u32b(8), u32b(maxBebopByteLen+1)...)); err == nil {
		t.Fatal("oversized init snapshot accepted")
	}
}
