package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A FLAC decoder (RFC 9639), so that every bit depth from 4 to 32 can be
// hashed: samples are kept as int64, which also holds the 33-bit side
// channel of 32-bit stereo. It decodes native FLAC streams and the frames
// of FLAC in Ogg alike.

// StreamInfo is a FLAC stream's STREAMINFO block.
type StreamInfo struct {
	MinBlock, MaxBlock uint16
	SampleRate         uint32
	Channels           uint8
	BitsPerSample      uint8
	TotalSamples       uint64 // 0 if unknown
	MD5                [16]byte
}

// decodeStreamInfo reads a STREAMINFO block's 34-byte body.
func decodeStreamInfo(b []byte) (StreamInfo, error) {
	var si StreamInfo
	if len(b) < 34 {
		return si, fmt.Errorf("STREAMINFO of %d bytes: %w", len(b), ErrNotFLAC)
	}
	si.MinBlock = binary.BigEndian.Uint16(b[0:])
	si.MaxBlock = binary.BigEndian.Uint16(b[2:])
	x := binary.BigEndian.Uint64(b[10:])
	si.SampleRate = uint32(x >> 44)
	si.Channels = uint8(x>>41&7) + 1
	si.BitsPerSample = uint8(x>>36&31) + 1
	si.TotalSamples = x & (1<<36 - 1)
	copy(si.MD5[:], b[18:34])
	if si.SampleRate == 0 || si.BitsPerSample < 4 {
		return si, fmt.Errorf("STREAMINFO says %d Hz, %d bits: %w", si.SampleRate, si.BitsPerSample, ErrNotFLAC)
	}
	return si, nil
}

// FLACDecoder decodes a FLAC stream's frames one by one.
type FLACDecoder struct {
	Info StreamInfo
	br   *bitReader
	// Samples holds the last frame's decoded samples, by channel; it is
	// reused by the next frame.
	Samples [][]int64
	resid   []int64
}

// NewFLACDecoder reads a native FLAC stream's start: any ID3v2 tags, the
// "fLaC" marker and the metadata blocks. Frames follow with Next.
func NewFLACDecoder(r io.ByteReader) (*FLACDecoder, error) {
	br := newBitReader(r)
	var marker [4]byte
	for {
		for i := range marker {
			v, err := br.bits(8)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrNotFLAC, err)
			}
			marker[i] = byte(v)
		}
		if !bytes.Equal(marker[:3], []byte("ID3")) {
			break
		}
		// An ID3v2 tag: 2 more header bytes, a syncsafe size, the body.
		var h [6]byte
		for i := range h {
			v, err := br.bits(8)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrNotFLAC, err)
			}
			h[i] = byte(v)
		}
		n := int64(syncsafe(h[2:6]))
		if h[1]&0x10 != 0 {
			n += 10
		}
		for ; n > 0; n-- {
			if _, err := br.bits(8); err != nil {
				return nil, fmt.Errorf("ID3v2 tag: %w", ErrNotFLAC)
			}
		}
	}
	if !bytes.Equal(marker[:], []byte("fLaC")) {
		return nil, ErrNotFLAC
	}
	d := &FLACDecoder{br: br}
	seen := false
	for range maxBlocks {
		h, err := br.bits(32)
		if err != nil {
			return nil, fmt.Errorf("FLAC metadata: %w: %v", ErrNotFLAC, err)
		}
		last, typ, n := h>>31 != 0, h>>24&0x7f, int(h&0xffffff)
		if typ == 127 {
			return nil, fmt.Errorf("invalid FLAC metadata block type: %w", ErrNotFLAC)
		}
		if typ == 0 && !seen {
			body := make([]byte, n)
			for i := range body {
				v, err := br.bits(8)
				if err != nil {
					return nil, fmt.Errorf("STREAMINFO: %w: %v", ErrNotFLAC, err)
				}
				body[i] = byte(v)
			}
			if d.Info, err = decodeStreamInfo(body); err != nil {
				return nil, err
			}
			seen = true
		} else {
			for ; n > 0; n-- {
				if _, err := br.bits(8); err != nil {
					return nil, fmt.Errorf("FLAC metadata: %w: %v", ErrNotFLAC, err)
				}
			}
		}
		if last {
			if !seen {
				return nil, fmt.Errorf("no STREAMINFO block: %w", ErrNotFLAC)
			}
			return d, nil
		}
	}
	return nil, fmt.Errorf("more than %d FLAC metadata blocks: %w", maxBlocks, ErrNotFLAC)
}

// newFLACFrameDecoder decodes frames from r for a stream described by
// info, as FLAC in Ogg carries them.
func newFLACFrameDecoder(info StreamInfo, r io.ByteReader) *FLACDecoder {
	return &FLACDecoder{Info: info, br: newBitReader(r)}
}

// ErrFLACFrame wraps errors in a frame (as opposed to the end of the
// stream, io.EOF).
var ErrFLACFrame = errors.New("bad FLAC frame")

// Next decodes the next frame into d.Samples and returns its sample
// count per channel, or io.EOF at a clean end of the stream.
func (d *FLACDecoder) Next() (int, error) {
	br := d.br
	br.crc, br.crc8, br.crc16 = true, 0, 0
	start := br.taken
	sync, err := br.bits(15)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) && br.taken == start {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("%w: %v", ErrFLACFrame, err)
	}
	if sync != 0x7ffc { // 14-bit sync code and a reserved 0
		return 0, fmt.Errorf("%w: no frame sync code", ErrFLACFrame)
	}
	n, err := d.frame()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrFLACFrame, err)
	}
	return n, nil
}

func (d *FLACDecoder) frame() (int, error) {
	br, info := d.br, d.Info
	h, err := br.bits(17) // blocking strategy, block size, rate, channels, sample size, reserved
	if err != nil {
		return 0, err
	}
	bsCode, rateCode, chCode, sizeCode := h>>12&15, h>>8&15, h>>4&15, h>>1&7
	if h&1 != 0 || sizeCode == 3 || rateCode == 15 || chCode > 10 || bsCode == 0 {
		return 0, errCorrupt
	}
	// The frame or sample number, UTF-8 coded; only its length matters.
	first, err := br.bits(8)
	if err != nil {
		return 0, err
	}
	extra := 0
	for m := uint64(0x80); first&m != 0 && extra < 7; m >>= 1 {
		extra++
	}
	if extra == 1 || extra == 7 && first != 0xfe {
		return 0, errCorrupt
	}
	for i := 1; i < extra; i++ {
		if c, err := br.bits(8); err != nil {
			return 0, err
		} else if c>>6 != 2 {
			return 0, errCorrupt
		}
	}
	var blockSize int
	switch {
	case bsCode == 1:
		blockSize = 192
	case bsCode <= 5:
		blockSize = 576 << (bsCode - 2)
	case bsCode == 6:
		v, err := br.bits(8)
		if err != nil {
			return 0, err
		}
		blockSize = int(v) + 1
	case bsCode == 7:
		v, err := br.bits(16)
		if err != nil {
			return 0, err
		}
		blockSize = int(v) + 1
	default:
		blockSize = 256 << (bsCode - 8)
	}
	rate := info.SampleRate
	switch rateCode {
	case 0:
	case 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11:
		rate = [...]uint32{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}[rateCode]
	case 12:
		v, err := br.bits(8)
		if err != nil {
			return 0, err
		}
		rate = uint32(v) * 1000
	default:
		v, err := br.bits(16)
		if err != nil {
			return 0, err
		}
		rate = uint32(v)
		if rateCode == 14 {
			rate *= 10
		}
	}
	bps := info.BitsPerSample
	if sizeCode != 0 {
		bps = [...]uint8{0, 8, 12, 0, 16, 20, 24, 32}[sizeCode]
	}
	channels := int(chCode) + 1
	if chCode >= 8 {
		channels = 2
	}
	headerCRC := br.crc8
	if c, err := br.bits(8); err != nil {
		return 0, err
	} else if byte(c) != headerCRC {
		return 0, errors.New("frame header CRC-8 mismatch")
	}
	if rate != info.SampleRate || bps != info.BitsPerSample || channels != int(info.Channels) {
		return 0, fmt.Errorf("frame of %d channels, %d bits at %d Hz in a stream of %d, %d at %d: %w",
			channels, bps, rate, info.Channels, info.BitsPerSample, info.SampleRate, ErrAudioMismatch)
	}
	if blockSize > 65536 {
		return 0, errCorrupt
	}

	for len(d.Samples) < channels {
		d.Samples = append(d.Samples, nil)
	}
	d.Samples = d.Samples[:channels]
	for ch := range channels {
		sb := uint(bps)
		if (chCode == 8 || chCode == 10) && ch == 1 || chCode == 9 && ch == 0 {
			sb++ // the side channel
		}
		if cap(d.Samples[ch]) < blockSize {
			d.Samples[ch] = make([]int64, blockSize)
		}
		d.Samples[ch] = d.Samples[ch][:blockSize]
		if err := d.subframe(d.Samples[ch], sb); err != nil {
			return 0, err
		}
	}
	switch chCode {
	case 8: // left, side
		l, s := d.Samples[0], d.Samples[1]
		for i := range l {
			s[i] = l[i] - s[i]
		}
	case 9: // side, right
		s, r := d.Samples[0], d.Samples[1]
		for i := range s {
			s[i] += r[i]
		}
	case 10: // mid, side
		m, s := d.Samples[0], d.Samples[1]
		for i := range m {
			mid := m[i]<<1 | s[i]&1
			m[i], s[i] = (mid+s[i])>>1, (mid-s[i])>>1
		}
	}
	br.align()
	frameCRC := br.crc16
	br.crc = false
	if c, err := br.bits(16); err != nil {
		return 0, err
	} else if uint16(c) != frameCRC {
		return 0, errors.New("frame CRC-16 mismatch")
	}
	return blockSize, nil
}

// subframe decodes one channel of a frame into out, at bps bits.
func (d *FLACDecoder) subframe(out []int64, bps uint) error {
	br := d.br
	h, err := br.bits(8)
	if err != nil {
		return err
	}
	if h&0x80 != 0 {
		return errCorrupt
	}
	typ := h >> 1 & 63
	wasted := uint(0)
	if h&1 != 0 {
		k, err := br.unary(uint64(bps))
		if err != nil {
			return err
		}
		wasted = uint(k) + 1
		if wasted >= bps {
			return errCorrupt
		}
		bps -= wasted
	}
	switch {
	case typ == 0:
		v, err := br.signed(bps)
		if err != nil {
			return err
		}
		for i := range out {
			out[i] = v
		}
	case typ == 1:
		for i := range out {
			if out[i], err = br.signed(bps); err != nil {
				return err
			}
		}
	case typ >= 8 && typ <= 12:
		order := int(typ - 8)
		if err := d.warmup(out, order, bps); err != nil {
			return err
		}
		if err := d.residual(out, order); err != nil {
			return err
		}
		fixedRestore(out, order)
	case typ >= 32:
		order := int(typ - 31)
		if err := d.warmup(out, order, bps); err != nil {
			return err
		}
		p, err := br.bits(4)
		if err != nil {
			return err
		}
		if p == 15 {
			return errCorrupt
		}
		prec := uint(p) + 1
		shift, err := br.signed(5)
		if err != nil {
			return err
		}
		if shift < 0 {
			return errCorrupt
		}
		var coeffs [32]int64
		for i := range order {
			if coeffs[i], err = br.signed(prec); err != nil {
				return err
			}
		}
		if err := d.residual(out, order); err != nil {
			return err
		}
		for i := order; i < len(out); i++ {
			var sum int64
			for j := range order {
				sum += coeffs[j] * out[i-1-j]
			}
			out[i] += sum >> uint(shift)
		}
	default:
		return errCorrupt
	}
	if wasted > 0 {
		for i := range out {
			out[i] <<= wasted
		}
	}
	return nil
}

func (d *FLACDecoder) warmup(out []int64, order int, bps uint) error {
	if order > len(out) {
		return errCorrupt
	}
	for i := range order {
		v, err := d.br.signed(bps)
		if err != nil {
			return err
		}
		out[i] = v
	}
	return nil
}

// residual reads the Rice-coded residual into out[order:].
func (d *FLACDecoder) residual(out []int64, order int) error {
	br := d.br
	method, err := br.bits(2)
	if err != nil {
		return err
	}
	if method > 1 {
		return errCorrupt
	}
	paramBits, escape := uint(4), uint64(15)
	if method == 1 {
		paramBits, escape = 5, 31
	}
	po, err := br.bits(4)
	if err != nil {
		return err
	}
	parts := 1 << po
	if len(out)%parts != 0 || len(out)/parts < order {
		return errCorrupt
	}
	per := len(out) / parts
	i := order
	for p := range parts {
		end := (p + 1) * per
		k, err := br.bits(paramBits)
		if err != nil {
			return err
		}
		if k == escape {
			n, err := br.bits(5)
			if err != nil {
				return err
			}
			for ; i < end; i++ {
				if out[i], err = br.signed(uint(n)); err != nil {
					return err
				}
			}
			continue
		}
		for ; i < end; i++ {
			q, err := br.unary(1 << 32)
			if err != nil {
				return err
			}
			lo, err := br.bits(uint(k))
			if err != nil {
				return err
			}
			v := q<<k | lo
			out[i] = int64(v>>1) ^ -int64(v&1)
		}
	}
	return nil
}

// fixedRestore undoes a fixed predictor of this order.
func fixedRestore(s []int64, order int) {
	switch order {
	case 1:
		for i := 1; i < len(s); i++ {
			s[i] += s[i-1]
		}
	case 2:
		for i := 2; i < len(s); i++ {
			s[i] += 2*s[i-1] - s[i-2]
		}
	case 3:
		for i := 3; i < len(s); i++ {
			s[i] += 3*s[i-1] - 3*s[i-2] + s[i-3]
		}
	case 4:
		for i := 4; i < len(s); i++ {
			s[i] += 4*s[i-1] - 6*s[i-2] + 4*s[i-3] - s[i-4]
		}
	}
}
