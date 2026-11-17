// The wire formats git smart HTTP speaks: pkt-line framing and packfiles.
// The reader accepts version 2 and 3 packs, full objects and both delta
// kinds, including thin packs whose bases live in the store; the writer
// emits delta-free version 2 packs, which every git client accepts and
// which are cheap to build and cache. No clock, no randomness, no I/O.

package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// PktLineMax is the largest pkt-line frame, length header included.
const PktLineMax = 65520

// PktLineMaxPayload is the largest pkt-line payload.
const PktLineMaxPayload = PktLineMax - 4

// PacketKind distinguishes data packets from the three control packets.
type PacketKind uint8

const (
	PktData PacketKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

// Packet is one decoded pkt-line. Payload is set only for PktData and
// aliases the decoded buffer.
type Packet struct {
	Kind    PacketKind
	Payload []byte
}

// AppendPktLine appends one data packet: a 4-byte lowercase hex length
// (payload + 4) then the payload.
func AppendPktLine(dst, payload []byte) ([]byte, error) {
	if len(payload) > PktLineMaxPayload {
		return dst, fmt.Errorf("pkt-line payload is %d bytes, over the %d-byte maximum", len(payload), PktLineMaxPayload)
	}
	dst = append(dst, pktLenHex(len(payload)+4)...)
	return append(dst, payload...), nil
}

func pktLenHex(n int) string {
	const digits = "0123456789abcdef"
	var b [4]byte
	for i := 3; i >= 0; i-- {
		b[i] = digits[n&0xf]
		n >>= 4
	}
	return string(b[:])
}

// AppendPktFlush appends a flush packet, AppendPktDelim a delim packet.
func AppendPktFlush(dst []byte) []byte { return append(dst, "0000"...) }
func AppendPktDelim(dst []byte) []byte { return append(dst, "0001"...) }

// EncodePktLine encodes one data packet.
func EncodePktLine(payload []byte) ([]byte, error) {
	return AppendPktLine(nil, payload)
}

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// DecodePktLines decodes a complete buffer into packets. Decoding stops at
// the first four bytes that are not a pkt-line length and returns the rest
// untouched, so a packfile following a pkt-line stream survives; strict
// refuses any trailing bytes instead. On error the partial packets are
// discarded by the caller.
func DecodePktLines(data []byte, strict bool) (packets []Packet, rest []byte, err error) {
	offset := 0
	for offset < len(data) {
		if len(data)-offset < 4 {
			if strict {
				return nil, nil, fmt.Errorf("truncated pkt-line: %d trailing byte(s)", len(data)-offset)
			}
			break
		}
		header := data[offset : offset+4]
		if !isHex4(header) {
			break
		}
		length, _ := hex.DecodeString(string(header))
		n := int(length[0])<<8 | int(length[1])
		switch n {
		case 0:
			packets = append(packets, Packet{Kind: PktFlush})
			offset += 4
			continue
		case 1:
			packets = append(packets, Packet{Kind: PktDelim})
			offset += 4
			continue
		case 2:
			packets = append(packets, Packet{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		if n == 3 {
			return nil, nil, errors.New("invalid pkt-line length 0003")
		}
		if n > PktLineMax {
			return nil, nil, fmt.Errorf("pkt-line length %d exceeds %d", n, PktLineMax)
		}
		if offset+n > len(data) {
			if strict {
				return nil, nil, fmt.Errorf("truncated pkt-line: %d announced, %d available", n, len(data)-offset)
			}
			break
		}
		packets = append(packets, Packet{Kind: PktData, Payload: data[offset+4 : offset+n]})
		offset += n
	}
	return packets, data[offset:], nil
}

// PacketsToLines returns the decoded text of every data packet, control
// packets dropped.
func PacketsToLines(packets []Packet) []string {
	var lines []string
	for _, p := range packets {
		if p.Kind == PktData {
			lines = append(lines, SanitizeUTF8(string(p.Payload)))
		}
	}
	return lines
}

// ---------------------------------------------------------------------------
// Packfile read
// ---------------------------------------------------------------------------

var typeCode = map[ObjType]byte{
	ObjCommit: 1,
	ObjTree:   2,
	ObjBlob:   3,
	ObjTag:    4,
}

var codeType = map[byte]ObjType{
	1: ObjCommit,
	2: ObjTree,
	3: ObjBlob,
	4: ObjTag,
}

// readVarintSize reads a pack entry's size header: the type in bits 4-6 of
// the first byte, the size in its low nibble plus 7-bit continuation
// groups. It returns the size, the offset past the header, and the first
// byte.
func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return size, offset, first, nil
}

// readOfsDeltaBase reads an OFS_DELTA's negative base offset: git's
// variable-length encoding where each continuation adds one before shifting.
func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[offset]
		offset++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, offset, nil
}

// inflateAt inflates one zlib stream starting at offset within buf[:limit]
// and returns its bytes plus the offset just past the stream. A buffered
// reader accounts for the inflater's readahead: the bytes the stream
// consumed are the total minus what the underlying reader still holds
// minus what sits unread in the buffer.
func inflateAt(buf []byte, offset, limit int) (data []byte, next int, err error) {
	src := bytes.NewReader(buf[offset:limit])
	br := bufio.NewReader(src)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate", offset)
	}
	data, err = io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate", offset)
	}
	consumed := (limit - offset) - src.Len() - br.Buffered()
	return data, offset + consumed, nil
}

// ApplyDelta applies a git delta (the copy/insert instruction stream) to a
// base object's bytes.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	// A corrupt delta must fail, never attempt a gigantic allocation: no
	// legitimate object in a trial approaches this bound.
	if targetSize < 0 || targetSize > 1<<30 {
		return nil, fmt.Errorf("delta target size %d is out of range", targetSize)
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if offset+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// PackReadResult is a decoded packfile: its version, declared count, the
// resolved objects keyed by id, whether the trailer checksum matched, and
// the pack's byte length.
type PackReadResult struct {
	Version       int
	DeclaredCount int
	Objects       map[string]*GitObject
	ChecksumOK    bool
	ByteLength    int
}

// rawEntry is one pack entry before delta resolution.
type rawEntry struct {
	offset       int
	typeCode     byte
	size         int
	data         []byte
	baseOffset   int
	baseOID      string
	resolvedType ObjType
	resolved     []byte
	oid          string
}

// ReadPack reads a packfile (version 2 or 3) and resolves every object,
// including OFS_DELTA and REF_DELTA. resolveBase supplies a base object a
// thin pack leaves out. Every object's SHA-1 is recomputed; a mismatch is
// an error, so corruption is never stored as a successful push.
func ReadPack(pack []byte, resolveBase ObjectLookup) (*PackReadResult, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(binary.BigEndian.Uint32(pack[4:8]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	sum := sha1.Sum(pack[:trailerStart])
	checksumOK := trailer == hex.EncodeToString(sum[:])

	var raw []*rawEntry
	byOffset := map[int]*rawEntry{}
	offset := 12
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		code := (first >> 4) & 0x07
		entry := &rawEntry{offset: entryOffset, typeCode: code, size: size}
		if code == 6 {
			baseOff, nextOff, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nextOff
			if baseOff <= 0 || baseOff > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, baseOff)
			}
			entry.baseOffset = entryOffset - baseOff
		} else if code == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[code]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, code)
		}
		inflated, nextOff, err := inflateAt(pack, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := map[string]*GitObject{}
	resolving := map[int]bool{}
	resolvedByOID := map[string]*rawEntry{}

	var resolve func(entry *rawEntry) (ObjType, []byte, string, error)
	resolve = func(entry *rawEntry) (ObjType, []byte, string, error) {
		if entry.resolved != nil && entry.oid != "" {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ ObjType
		var data []byte
		var err error
		switch entry.typeCode {
		case 6:
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				err = fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
				break
			}
			var baseType ObjType
			var baseData []byte
			baseType, baseData, _, err = resolve(baseEntry)
			if err == nil {
				typ = baseType
				data, err = ApplyDelta(baseData, entry.data)
			}
		case 7:
			wanted := entry.baseOID
			if baseEntry := resolvedByOID[wanted]; baseEntry != nil {
				var baseType ObjType
				var baseData []byte
				baseType, baseData, _, err = resolve(baseEntry)
				if err == nil {
					typ = baseType
					data, err = ApplyDelta(baseData, entry.data)
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					err = fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", wanted)
				} else {
					typ = external.Type
					data, err = ApplyDelta(external.Data, entry.data)
				}
			}
		default:
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		if err != nil {
			delete(resolving, entry.offset)
			return "", nil, "", err
		}
		oid := HashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}

	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, exists := objects[oid]; !exists {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return &PackReadResult{
		Version:       version,
		DeclaredCount: declaredCount,
		Objects:       objects,
		ChecksumOK:    checksumOK,
		ByteLength:    len(pack),
	}, nil
}

// ---------------------------------------------------------------------------
// Packfile write
// ---------------------------------------------------------------------------

// writePackObjectHeader encodes one object's pack header: the type/size
// varint git's pack format uses.
func writePackObjectHeader(typ ObjType, size int) []byte {
	code := typeCode[typ]
	var bytes []byte
	remaining := size
	first := byte(code<<4) | byte(remaining&0x0f)
	remaining /= 16
	for remaining > 0 {
		bytes = append(bytes, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	bytes = append(bytes, first)
	return bytes
}

// DeflateLevel selects the zlib compression level pack building uses. The
// default matches the reference entry's behavior class; the benchmark pass
// may raise it to BestSpeed when the numbers justify it.
var DeflateLevel = zlib.DefaultCompression

// WritePack builds a packfile from plain objects (no deltas). The order is
// the caller's; git accepts any order. The trailer is SHA-1 over everything
// before it. Every entry is deflated first so the whole pack fits a single
// buffer.
func WritePack(objects []*GitObject) []byte {
	type entry struct {
		header   []byte
		deflated []byte
	}
	entries := make([]entry, 0, len(objects))
	total := 12 + 20
	for _, object := range objects {
		header := writePackObjectHeader(object.Type, len(object.Data))
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, DeflateLevel)
		w.Write(object.Data)
		w.Close()
		deflated := buf.Bytes()
		entries = append(entries, entry{header: header, deflated: deflated})
		total += len(header) + len(deflated)
	}
	out := make([]byte, total)
	copy(out, "PACK")
	binary.BigEndian.PutUint32(out[4:8], 2)
	binary.BigEndian.PutUint32(out[8:12], uint32(len(entries)))
	at := 12
	for _, e := range entries {
		at += copy(out[at:], e.header)
		at += copy(out[at:], e.deflated)
	}
	sum := sha1.Sum(out[:at])
	copy(out[at:], sum[:])
	return out
}
