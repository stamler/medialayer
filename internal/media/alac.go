package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// An ALAC (Apple Lossless) decoder, after Apple's open-source reference
// decoder and ffmpeg's. One packet (an MP4 or CAF sample) decodes to a
// frame of every channel.

// ALACConfig is an ALAC stream's magic cookie (ALACSpecificConfig).
type ALACConfig struct {
	FrameLength   uint32
	BitDepth      uint8
	HistoryMult   uint8 // pb
	InitHistory   uint8 // mb
	RiceLimit     uint8 // kb
	Channels      uint8
	MaxRun        uint16
	MaxFrameBytes uint32
	AvgBitRate    uint32
	SampleRate    uint32
}

// parseALACConfig reads the 24-byte cookie, which may come wrapped in
// "frma" and "alac" atoms, as CAF files and some MP4 files keep it.
func parseALACConfig(b []byte) (ALACConfig, error) {
	for len(b) >= 12 && (string(b[4:8]) == "frma" || string(b[4:8]) == "alac") {
		n := int(binary.BigEndian.Uint32(b))
		switch {
		case string(b[4:8]) == "frma" && n >= 12 && n <= len(b):
			b = b[n:]
		case string(b[4:8]) == "alac" && len(b) >= 36:
			b = b[12:] // atom header and version/flags
		default:
			return ALACConfig{}, errors.New("malformed ALAC magic cookie")
		}
	}
	if len(b) < 24 {
		return ALACConfig{}, fmt.Errorf("ALAC magic cookie of %d bytes", len(b))
	}
	c := ALACConfig{
		FrameLength: binary.BigEndian.Uint32(b), BitDepth: b[5],
		HistoryMult: b[6], InitHistory: b[7], RiceLimit: b[8], Channels: b[9],
		MaxRun: binary.BigEndian.Uint16(b[10:]), MaxFrameBytes: binary.BigEndian.Uint32(b[12:]),
		AvgBitRate: binary.BigEndian.Uint32(b[16:]), SampleRate: binary.BigEndian.Uint32(b[20:]),
	}
	switch {
	case c.FrameLength == 0 || c.FrameLength > 1<<16:
		return c, fmt.Errorf("ALAC frame length %d", c.FrameLength)
	case c.BitDepth < 4 || c.BitDepth > 32:
		return c, fmt.Errorf("ALAC bit depth %d", c.BitDepth)
	case c.Channels == 0 || c.Channels > 8:
		return c, fmt.Errorf("ALAC with %d channels", c.Channels)
	case c.SampleRate == 0:
		return c, errors.New("ALAC with no sample rate")
	}
	return c, nil
}

// alacOrder maps the order ALAC stores channels in to WAVE order: entry
// [n-1][i] is where the i-th stored channel of an n-channel stream goes.
var alacOrder = [8][]int{
	{0},
	{0, 1},
	{2, 0, 1},
	{2, 0, 1, 3},
	{2, 0, 1, 3, 4},
	{2, 0, 1, 4, 5, 3},
	{2, 0, 1, 4, 5, 6, 3},
	{2, 6, 7, 0, 1, 4, 5, 3},
}

// ALACDecoder decodes ALAC packets.
type ALACDecoder struct {
	Config ALACConfig
	// Samples holds the last packet's decoded samples, by channel in WAVE
	// order; it is reused by the next packet.
	Samples [][]int64
	work    [2][]int32 // prediction errors, then samples, of one element
	extra   [2][]int32
}

// NewALACDecoder makes a decoder for the stream the cookie describes.
func NewALACDecoder(c ALACConfig) *ALACDecoder {
	d := &ALACDecoder{Config: c, Samples: make([][]int64, c.Channels)}
	n := int(c.FrameLength)
	for ch := range d.Samples {
		d.Samples[ch] = make([]int64, n)
	}
	for i := range 2 {
		d.work[i] = make([]int32, n)
		d.extra[i] = make([]int32, n)
	}
	return d
}

// alacReader reads ALAC's bit fields from one packet.
type alacReader struct {
	b   []byte
	pos uint // in bits
}

func (r *alacReader) left() int { return len(r.b)*8 - int(r.pos) }

// bits reads k bits, k at most 32.
func (r *alacReader) bits(k uint) (uint32, error) {
	if k == 0 {
		return 0, nil
	}
	if r.left() < int(k) {
		return 0, errCorrupt
	}
	var v uint64
	i := r.pos / 8
	for j := range uint(5) {
		v <<= 8
		if int(i+j) < len(r.b) {
			v |= uint64(r.b[i+j])
		}
	}
	v = v << (24 + r.pos%8) >> (64 - k)
	r.pos += k
	return uint32(v), nil
}

func (r *alacReader) peek(k uint) uint32 {
	pos := r.pos
	v, err := r.bits(min(k, uint(max(r.left(), 0))))
	r.pos = pos
	if err != nil {
		return 0
	}
	return v << (k - min(k, uint(max(r.left(), 0))))
}

func (r *alacReader) signed(k uint) (int32, error) {
	v, err := r.bits(k)
	if k == 0 || k == 32 {
		return int32(v), err
	}
	return int32(v<<(32-k)) >> (32 - k), err
}

// Decode decodes one packet into d.Samples and returns its sample count
// per channel.
func (d *ALACDecoder) Decode(packet []byte) (int, error) {
	r := &alacReader{b: packet}
	c := d.Config
	ch, n := 0, 0
	for r.left() >= 3 {
		tag, _ := r.bits(3)
		if tag == 7 { // END
			break
		}
		if tag != 0 && tag != 1 && tag != 3 { // SCE, CPE, LFE
			return 0, fmt.Errorf("ALAC element %d: %w", tag, errCorrupt)
		}
		channels := 1
		if tag == 1 {
			channels = 2
		}
		if ch+channels > int(c.Channels) {
			return 0, fmt.Errorf("ALAC packet with too many channels: %w", errCorrupt)
		}
		got, err := d.element(r, channels, n)
		if err != nil {
			return 0, err
		}
		n = got
		order := alacOrder[c.Channels-1]
		for i := range channels {
			out := order[ch+i]
			if channels == 2 && i == 1 {
				out = order[ch] + 1
			}
			if out >= int(c.Channels) {
				return 0, errCorrupt
			}
			dst := d.Samples[out][:n]
			for j, v := range d.work[i][:n] {
				dst[j] = int64(v)
			}
		}
		ch += channels
	}
	if ch != int(c.Channels) || n == 0 {
		return 0, fmt.Errorf("ALAC packet with %d of %d channels: %w", ch, c.Channels, errCorrupt)
	}
	return n, nil
}

// element decodes one single or channel-pair element into d.work. want is
// the sample count earlier elements of the packet had, or 0.
func (d *ALACDecoder) element(r *alacReader, channels, want int) (int, error) {
	c := d.Config
	if _, err := r.bits(16); err != nil { // element instance tag, unused header
		return 0, err
	}
	hdr, err := r.bits(4)
	if err != nil {
		return 0, err
	}
	hasSize, extraBits, compressed := hdr>>3 != 0, uint(hdr>>1&3)*8, hdr&1 == 0
	bps := int(c.BitDepth) - int(extraBits) + channels - 1
	if bps > 32 || bps < 1 {
		return 0, errCorrupt
	}
	n := int(c.FrameLength)
	if hasSize {
		v, err := r.bits(32)
		if err != nil {
			return 0, err
		}
		n = int(v)
	}
	if n == 0 || n > int(c.FrameLength) || want != 0 && n != want {
		return 0, fmt.Errorf("ALAC element of %d samples: %w", n, errCorrupt)
	}
	shift, weight := uint32(0), int32(0)
	if compressed {
		if c.RiceLimit == 0 {
			return 0, errCorrupt
		}
		s, _ := r.bits(8)
		w, err := r.bits(8)
		if err != nil {
			return 0, err
		}
		shift, weight = s, int32(w)
		if channels == 2 && weight != 0 && shift > 31 {
			return 0, errCorrupt
		}
		var mode, quant, mult, order [2]uint32
		var coefs [2][32]int16 // 16 bits, as the reference decoders keep them
		for ch := range channels {
			h, err := r.bits(16)
			if err != nil {
				return 0, err
			}
			mode[ch], quant[ch], mult[ch], order[ch] = h>>12, h>>8&15, h>>5&7, h&31
			if quant[ch] == 0 || order[ch] >= c.FrameLength || mode[ch] != 0 && mode[ch] != 15 {
				return 0, errCorrupt
			}
			for i := int(order[ch]) - 1; i >= 0; i-- {
				v, err := r.signed(16)
				if err != nil {
					return 0, err
				}
				coefs[ch][i] = int16(v)
			}
		}
		if extraBits > 0 {
			for i := range n {
				for ch := range channels {
					v, err := r.bits(extraBits)
					if err != nil {
						return 0, err
					}
					d.extra[ch][i] = int32(v)
				}
			}
		}
		for ch := range channels {
			buf := d.work[ch][:n]
			if err := d.rice(r, buf, uint(bps), uint32(mult[ch])*uint32(c.HistoryMult)/4); err != nil {
				return 0, err
			}
			if mode[ch] == 15 {
				alacPredict(buf, buf, uint(bps), nil, 31, 0)
			}
			alacPredict(buf, buf, uint(bps), coefs[ch][:order[ch]], int(order[ch]), uint(quant[ch]))
		}
	} else {
		for i := range n {
			for ch := range channels {
				v, err := r.signed(uint(c.BitDepth))
				if err != nil {
					return 0, err
				}
				d.work[ch][i] = v
			}
		}
		extraBits = 0
	}
	if channels == 2 && weight != 0 {
		a, b := d.work[0][:n], d.work[1][:n]
		for i := range n {
			x, y := a[i], b[i]
			x -= int32(uint32(y)*uint32(weight)) >> shift
			y += x
			a[i], b[i] = y, x
		}
	}
	if extraBits > 0 {
		for ch := range channels {
			for i := range n {
				d.work[ch][i] = d.work[ch][i]<<extraBits | d.extra[ch][i]
			}
		}
	}
	return n, nil
}

// rice reads n Rice-coded prediction errors with ALAC's adaptive
// parameter.
func (d *ALACDecoder) rice(r *alacReader, out []int32, bps uint, histMult uint32) error {
	c := d.Config
	history := uint32(c.InitHistory)
	signMod := uint32(0)
	limit := uint(c.RiceLimit)
	for i := 0; i < len(out); i++ {
		k := min(uint(log2(history>>9+3)), limit)
		x, err := alacScalar(r, k, bps)
		if err != nil {
			return err
		}
		x += signMod
		signMod = 0
		out[i] = int32(x>>1) ^ -int32(x&1)
		if x > 0xffff {
			history = 0xffff
		} else {
			history += x*histMult - (history*histMult)>>9
		}
		if history < 128 && i+1 < len(out) {
			k := min(uint(7-log2(history)+int((history+16)>>6)), limit)
			run, err := alacScalar(r, k, 16)
			if err != nil {
				return err
			}
			if run > 0 {
				if int(run) >= len(out)-i {
					return errCorrupt
				}
				clear(out[i+1 : i+1+int(run)])
				i += int(run)
			}
			if run <= 0xffff {
				signMod = 1
			}
			history = 0
		}
	}
	return nil
}

// log2 is the index of the highest set bit, 0 for 0.
func log2(v uint32) int {
	if v == 0 {
		return 0
	}
	return 31 - bits.LeadingZeros32(v)
}

// alacScalar reads one value: a unary prefix of up to 8 one bits, then
// k bits with ALAC's adjustment, or, after 9 one bits, bps raw bits.
func alacScalar(r *alacReader, k, bps uint) (uint32, error) {
	x := uint32(0)
	for x < 9 {
		b, err := r.bits(1)
		if err != nil {
			return 0, err
		}
		if b == 0 {
			break
		}
		x++
	}
	if x > 8 {
		return r.bits(bps)
	}
	if k != 1 {
		extra := r.peek(k)
		x = x<<k - x
		if extra > 1 {
			x += extra - 1
			if _, err := r.bits(k); err != nil {
				return 0, err
			}
		} else if _, err := r.bits(k - 1); err != nil {
			return 0, err
		}
	}
	return x, nil
}

// alacPredict undoes ALAC's adaptive predictor, in place (in and out
// may be the same).
func alacPredict(in, out []int32, bps uint, coefs []int16, order int, quant uint) {
	n := len(in)
	if n == 0 {
		return
	}
	ext := func(v uint32) int32 { return int32(v<<(32-bps)) >> (32 - bps) }
	if bps == 32 {
		ext = func(v uint32) int32 { return int32(v) }
	}
	out[0] = in[0]
	if n <= 1 {
		return
	}
	if order == 0 {
		copy(out[1:], in[1:])
		return
	}
	if order == 31 {
		for i := 1; i < n; i++ {
			out[i] = ext(uint32(out[i-1]) + uint32(in[i]))
		}
		return
	}
	i := 1
	for ; i <= order && i < n; i++ {
		out[i] = ext(uint32(out[i-1]) + uint32(in[i]))
	}
	for ; i < n; i++ {
		errVal := uint32(in[i])
		base := i - order - 1
		d := out[base]
		pred := out[base+1 : base+1+order]
		var val int32
		for j := range order {
			val += int32((uint32(pred[j]) - uint32(d)) * uint32(int32(coefs[j])))
		}
		val = int32((int64(val) + int64(1)<<(quant-1)) >> quant)
		val = int32(uint32(val) + uint32(d) + errVal)
		out[i] = ext(uint32(val))
		sign := signOf(int32(errVal))
		if sign != 0 {
			for j := 0; j < order && int32(errVal*uint32(sign)) > 0; j++ {
				v := d - pred[j]
				s := signOf(v) * sign
				coefs[j] -= int16(s)
				v *= s
				errVal -= uint32(v>>quant) * uint32(j+1)
			}
		}
	}
}

func signOf(v int32) int32 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}
