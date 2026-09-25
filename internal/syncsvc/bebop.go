package syncsvc

import (
	"encoding/binary"
	"fmt"
)

// Minimal Bebop wire codec for services/sync-service/bebop/schema.bop.
//
// Union frame layout (from bebop-schema/src/generated.rs):
//
//	u32 LE  fieldBytes   (= total - LEN_SIZE(4) - discriminant(1))
//	u8      discriminant
//	...     fields in declaration order
//
// Scalar encodings: byte[] = u32 len + bytes; byte[][] = u32 count +
// len-prefixed elements; string = u32 len + utf8; uint64 = 8 LE bytes.

const bebopLenSize = 4

// Decoded-length bounds: a peer controls the u32 length/count prefixes, so
// every allocation is gated before it happens (the ws read cap is separate
// and much smaller; these bound *decoded* sizes for robustness if the codec
// is ever used on larger buffers).
const (
	// maxBebopByteLen caps a single byte/string field (64 MiB).
	maxBebopByteLen = 64 << 20
	// maxBebopElems caps a byte[][] element count (10M entries).
	maxBebopElems = 10 << 20
)

type bebopReader struct {
	buf []byte
	i   int
}

func (r *bebopReader) u8() (uint8, bool) {
	if r.i+1 > len(r.buf) {
		return 0, false
	}
	v := r.buf[r.i]
	r.i++
	return v, true
}

func (r *bebopReader) u32() (uint32, bool) {
	if r.i+4 > len(r.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.buf[r.i:])
	r.i += 4
	return v, true
}

func (r *bebopReader) u64() (uint64, bool) {
	if r.i+8 > len(r.buf) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(r.buf[r.i:])
	r.i += 8
	return v, true
}

func (r *bebopReader) bytes() ([]byte, bool) {
	n, ok := r.u32()
	if !ok || n > maxBebopByteLen || int(n) > len(r.buf)-r.i {
		return nil, false
	}
	v := r.buf[r.i : r.i+int(n)]
	r.i += int(n)
	return v, true
}

func (r *bebopReader) str() (string, bool) {
	b, ok := r.bytes()
	return string(b), ok
}

func (r *bebopReader) bytesList() ([][]byte, bool) {
	n, ok := r.u32()
	// Bound the element count before allocating the backing array — a
	// malicious u32 count would otherwise allocate ~16 bytes per element
	// regardless of how much data actually follows.
	if !ok || n > maxBebopElems {
		return nil, false
	}
	out := make([][]byte, 0, min(int(n), len(r.buf)/bebopLenSize+1))
	for j := uint32(0); j < n; j++ {
		b, ok := r.bytes()
		if !ok {
			return nil, false
		}
		out = append(out, b)
	}
	return out, true
}

// --- FromPeer (inbound) -----------------------------------------------------

// peerMsg is the decoded FromPeer union.
type peerMsg struct {
	kind      uint8
	updates   [][]byte // kind 1
	id        string   // kind 1
	awareness []byte   // kind 2
	vv        []byte   // kind 3
	peerID    uint64   // kind 5
}

const (
	peerUpdate          = 1
	peerAwareness       = 2
	peerRequestSince    = 3
	peerRequestSnapshot = 4
	peerRegisterID      = 5
)

// decodeFromPeer parses one ws binary frame into a peerMsg.
func decodeFromPeer(frame []byte) (peerMsg, error) {
	var m peerMsg
	if len(frame) < bebopLenSize+1 {
		return m, fmt.Errorf("frame too short")
	}
	fieldLen := binary.LittleEndian.Uint32(frame[:4])
	if int(fieldLen)+bebopLenSize+1 > len(frame) {
		return m, fmt.Errorf("declared length %d exceeds frame %d", fieldLen, len(frame))
	}
	m.kind = frame[4]
	r := &bebopReader{buf: frame[5 : 5+fieldLen]}
	switch m.kind {
	case peerUpdate:
		var ok bool
		if m.updates, ok = r.bytesList(); !ok {
			return m, fmt.Errorf("PeerUpdate.updates")
		}
		if m.id, ok = r.str(); !ok {
			return m, fmt.Errorf("PeerUpdate.id")
		}
	case peerAwareness:
		var ok bool
		if m.awareness, ok = r.bytes(); !ok {
			return m, fmt.Errorf("PeerAwareness.awareness")
		}
	case peerRequestSince:
		var ok bool
		if m.vv, ok = r.bytes(); !ok {
			return m, fmt.Errorf("PeerRequestSince.vv")
		}
	case peerRequestSnapshot:
	case peerRegisterID:
		var ok bool
		if m.peerID, ok = r.u64(); !ok {
			return m, fmt.Errorf("PeerRegisterId.peerid")
		}
	default:
		return m, fmt.Errorf("unknown FromPeer discriminant %d", m.kind)
	}
	return m, nil
}

// --- FromRemote (outbound) ---------------------------------------------------

const (
	remoteInitialSync = 1
	remoteUpdate      = 2
	remoteAwareness   = 3
	remoteSnapshot    = 4
	remoteUpdateAck   = 5
	remoteUpdateSince = 6
)

type bebopWriter struct{ body []byte }

func (w *bebopWriter) putBytes(b []byte) {
	w.body = binary.LittleEndian.AppendUint32(w.body, uint32(len(b)))
	w.body = append(w.body, b...)
}
func (w *bebopWriter) putStr(s string) { w.putBytes([]byte(s)) }

// frame wraps discr + body into a full union frame.
func (w *bebopWriter) frame(discr uint8) []byte {
	out := make([]byte, 0, len(w.body)+5)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(w.body)))
	out = append(out, discr)
	return append(out, w.body...)
}

func encInitialSync(snapshot, awareness []byte) []byte {
	w := &bebopWriter{}
	w.putBytes(snapshot)
	w.putBytes(awareness)
	return w.frame(remoteInitialSync)
}

func encRemoteUpdate(update []byte) []byte {
	w := &bebopWriter{}
	w.putBytes(update)
	return w.frame(remoteUpdate)
}

func encRemoteAwareness(awareness []byte) []byte {
	w := &bebopWriter{}
	w.putBytes(awareness)
	return w.frame(remoteAwareness)
}

func encRemoteSnapshot(snapshot []byte) []byte {
	w := &bebopWriter{}
	w.putBytes(snapshot)
	return w.frame(remoteSnapshot)
}

func encRemoteUpdateAck(id string) []byte {
	w := &bebopWriter{}
	w.putStr(id)
	return w.frame(remoteUpdateAck)
}

func encRemoteUpdateSince(update, vv []byte) []byte {
	w := &bebopWriter{}
	w.putBytes(update)
	w.putBytes(vv)
	return w.frame(remoteUpdateSince)
}

// decodeInitializeFromSnapshotRequest decodes InitializeFromSnapshotRequest
// {snapshot byte[]} — a plain record: u32 len + u32 len + bytes.
func decodeInitializeFromSnapshotRequest(body []byte) ([]byte, error) {
	r := &bebopReader{buf: body}
	if _, ok := r.u32(); !ok { // record len
		return nil, fmt.Errorf("missing record length")
	}
	snap, ok := r.bytes()
	if !ok {
		return nil, fmt.Errorf("missing snapshot field")
	}
	return snap, nil
}
