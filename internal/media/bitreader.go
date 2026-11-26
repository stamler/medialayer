package media

import (
	"errors"
	"io"
	"math/bits"
)

// bitReader reads big-endian bit fields, as FLAC and ALAC store them. It
// takes bytes from its source only as they are needed, so the bytes it
// has taken are exactly those of the fields read so far: what FLAC's
// checksums cover.
type bitReader struct {
	r     io.ByteReader
	cache uint64 // the next n bits, at the top
	n     uint
	crc8  byte   // FLAC's CRC-8 of the bytes taken, when crc is set
	crc16 uint16 // and its CRC-16
	crc   bool
	taken int64 // bytes taken from r
}

func newBitReader(r io.ByteReader) *bitReader { return &bitReader{r: r} }

// fetch takes one more byte from the source.
func (b *bitReader) fetch() error {
	c, err := b.r.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	b.cache |= uint64(c) << (56 - b.n)
	b.n += 8
	b.taken++
	if b.crc {
		b.crc8 = crc8Table[b.crc8^c]
		b.crc16 = b.crc16<<8 ^ crc16Table[byte(b.crc16>>8)^c]
	}
	return nil
}

// bits reads an unsigned field of k bits, k at most 57.
func (b *bitReader) bits(k uint) (uint64, error) {
	if k == 0 {
		return 0, nil
	}
	for b.n < k {
		if err := b.fetch(); err != nil {
			return 0, err
		}
	}
	v := b.cache >> (64 - k)
	b.cache <<= k
	b.n -= k
	return v, nil
}

// bits64 reads an unsigned field of up to 64 bits.
func (b *bitReader) bits64(k uint) (uint64, error) {
	if k <= 32 {
		return b.bits(k)
	}
	hi, err := b.bits(k - 32)
	if err != nil {
		return 0, err
	}
	lo, err := b.bits(32)
	return hi<<32 | lo, err
}

// signed reads a two's complement field of k bits, k at most 64.
func (b *bitReader) signed(k uint) (int64, error) {
	if k == 0 {
		return 0, nil
	}
	v, err := b.bits64(k)
	if err != nil {
		return 0, err
	}
	return int64(v<<(64-k)) >> (64 - k), nil
}

// unary counts zero bits up to the next one bit, which it reads too.
// It gives up past max zeros, against damaged streams.
func (b *bitReader) unary(max uint64) (uint64, error) {
	var q uint64
	for {
		if b.n == 0 {
			if err := b.fetch(); err != nil {
				return 0, err
			}
		}
		if z := uint(bits.LeadingZeros64(b.cache)); z < b.n {
			b.cache <<= z + 1
			b.n -= z + 1
			return q + uint64(z), nil
		}
		q += uint64(b.n)
		b.cache, b.n = 0, 0
		if q > max {
			return 0, errCorrupt
		}
	}
}

// align drops the bits left in the current byte.
func (b *bitReader) align() {
	drop := b.n % 8
	b.cache <<= drop
	b.n -= drop
}

// aligned reports whether the next bit starts a byte.
func (b *bitReader) aligned() bool { return b.n%8 == 0 }

// errCorrupt is a stream that breaks its format's rules.
var errCorrupt = errors.New("corrupt audio stream")

var crc8Table, crc16Table = func() (t8 [256]byte, t16 [256]uint16) {
	for i := range 256 {
		c8 := byte(i)
		c16 := uint16(i) << 8
		for range 8 {
			if c8&0x80 != 0 {
				c8 = c8<<1 ^ 0x07
			} else {
				c8 <<= 1
			}
			if c16&0x8000 != 0 {
				c16 = c16<<1 ^ 0x8005
			} else {
				c16 <<= 1
			}
		}
		t8[i], t16[i] = c8, c16
	}
	return
}()
