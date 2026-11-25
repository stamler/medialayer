package media

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// streamInfo builds a STREAMINFO block body.
func streamInfo(rate, channels, bits int, samples int64) []byte {
	b := make([]byte, 34)
	v := uint64(rate)<<44 | uint64(channels-1)<<41 | uint64(bits-1)<<36 | uint64(samples)
	binary.BigEndian.PutUint64(b[10:], v)
	return b
}

func vorbisComment(comments ...string) []byte {
	var b bytes.Buffer
	le := func(n int) { binary.Write(&b, binary.LittleEndian, uint32(n)) }
	le(6)
	b.WriteString("vendor")
	le(len(comments))
	for _, c := range comments {
		le(len(c))
		b.WriteString(c)
	}
	return b.Bytes()
}

func flacPictureBlock(typ int, mime string, w, h int, data []byte) []byte {
	var b bytes.Buffer
	be := func(n int) { binary.Write(&b, binary.BigEndian, uint32(n)) }
	be(typ)
	be(len(mime))
	b.WriteString(mime)
	be(5)
	b.WriteString("cover")
	be(w)
	be(h)
	be(24)
	be(0)
	be(len(data))
	b.Write(data)
	return b.Bytes()
}

func TestParseFLAC(t *testing.T) {
	art := randomBytes(5000, 7)
	file, audioStart := makeFLAC(t, 0, []flacBlock{
		{0, streamInfo(44100, 2, 16, 44100*185)},
		{4, vorbisComment("title=Song", "ARTIST=One", "ARTIST=Two", "junk")},
		{6, flacPictureBlock(3, "image/jpeg", 600, 600, art)},
		{1, make([]byte, 100)},
	}, randomBytes(1000, 1))

	info, err := ParseFLAC(file[:audioStart])
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 44100 || info.Channels != 2 || info.BitDepth != 16 || info.DurationMS != 185000 {
		t.Errorf("stream info = %d Hz, %d ch, %d bit, %d ms", info.SampleRate, info.Channels, info.BitDepth, info.DurationMS)
	}
	wantTags := []Tag{{"TITLE", "Song"}, {"ARTIST", "One"}, {"ARTIST", "Two"}}
	if !reflect.DeepEqual(info.Tags, wantTags) {
		t.Errorf("tags = %v, want %v", info.Tags, wantTags)
	}
	if len(info.Pictures) != 1 {
		t.Fatalf("got %d pictures, want 1", len(info.Pictures))
	}
	p := info.Pictures[0]
	if p.Type != 3 || p.MIME != "image/jpeg" || p.Width != 600 || !bytes.Equal(p.Data, art) {
		t.Errorf("picture = type %d %s %dx%d, %d bytes", p.Type, p.MIME, p.Width, p.Height, len(p.Data))
	}
}

func TestParseFLACTruncatedKeepsWholeBlocks(t *testing.T) {
	file, _ := makeFLAC(t, 3000, []flacBlock{
		{0, streamInfo(96000, 2, 24, 96000*10)},
		{4, vorbisComment("ALBUM=A")},
		{6, flacPictureBlock(3, "image/png", 1, 1, randomBytes(50_000, 3))},
	}, nil)
	info, err := ParseFLAC(file[:20_000]) // cuts into the picture
	if err != nil {
		t.Fatal(err)
	}
	if info.BitDepth != 24 || len(info.Tags) != 1 || len(info.Pictures) != 0 {
		t.Errorf("got %d bit, tags %v, %d pictures", info.BitDepth, info.Tags, len(info.Pictures))
	}
}

// id3Frame builds a v2.3 (plain size) or v2.4 (syncsafe size) frame.
func id3Frame(version int, id string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	n := len(data)
	if version == 4 {
		b.Write([]byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)})
	} else {
		binary.Write(&b, binary.BigEndian, uint32(n))
	}
	b.Write([]byte{0, 0})
	b.Write(data)
	return b.Bytes()
}

func id3Tag(version int, frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	body = append(body, make([]byte, 64)...) // padding
	n := len(body)
	hdr := []byte{'I', 'D', '3', byte(version), 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(hdr, body...)
}

func utf16LE(s string) []byte {
	b := []byte{0xff, 0xfe}
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// mpegFrames is MPEG-1 Layer III, 128 kbps, 44.1 kHz, joint stereo. With
// xingFrames > 0 the first frame carries a Xing header with that count.
func mpegFrames(xingFrames int, n int) []byte {
	const frameLen = 417 // 144 * 128000 / 44100
	hdr := []byte{0xff, 0xfb, 0x90, 0x40}
	var b bytes.Buffer
	for i := range n {
		f := make([]byte, frameLen)
		copy(f, hdr)
		if i == 0 && xingFrames > 0 {
			copy(f[4+32:], "Xing")
			binary.BigEndian.PutUint32(f[4+32+4:], 1)
			binary.BigEndian.PutUint32(f[4+32+8:], uint32(xingFrames))
		}
		b.Write(f)
	}
	return b.Bytes()
}

func fetchAll(file []byte) FetchFunc {
	return func(offset, length int64) ([]byte, error) {
		end := min(offset+length, int64(len(file)))
		return file[offset:end], nil
	}
}

func TestParseMP3v23(t *testing.T) {
	art := randomBytes(3000, 5)
	apic := append([]byte{0}, "image/jpeg\x00"...)
	apic = append(apic, 3)
	apic = append(apic, "desc\x00"...)
	apic = append(apic, art...)
	tag := id3Tag(3,
		id3Frame(3, "TIT2", append([]byte{1}, utf16LE("Café")...)),
		id3Frame(3, "TPE1", append([]byte{0}, "Artist"...)),
		id3Frame(3, "TRCK", append([]byte{0}, "3/12"...)),
		id3Frame(3, "TCON", append([]byte{0}, "(17)"...)),
		id3Frame(3, "APIC", apic),
	)
	// 1000 frames at 1152 samples, 44.1 kHz: 26122 ms.
	file := append(tag, mpegFrames(1000, 20)...)
	info, err := ParseMP3(fetchAll(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	wantTags := []Tag{{"TITLE", "Café"}, {"ARTIST", "Artist"}, {"TRACKNUMBER", "3"}, {"TRACKTOTAL", "12"}, {"GENRE", "Rock"}}
	if !reflect.DeepEqual(info.Tags, wantTags) {
		t.Errorf("tags = %v, want %v", info.Tags, wantTags)
	}
	if len(info.Pictures) != 1 || !bytes.Equal(info.Pictures[0].Data, art) || info.Pictures[0].Type != 3 {
		t.Errorf("pictures = %+v", info.Pictures)
	}
	if info.SampleRate != 44100 || info.Channels != 2 || info.DurationMS != 26122 {
		t.Errorf("got %d Hz, %d ch, %d ms", info.SampleRate, info.Channels, info.DurationMS)
	}
}

func TestParseMP3v24MultiValueAndCBR(t *testing.T) {
	tag := id3Tag(4,
		id3Frame(4, "TPE1", append([]byte{3}, "One\x00Two"...)),
		id3Frame(4, "TXXX", append([]byte{3}, "ALBUMARTIST\x00Various"...)),
	)
	audio := mpegFrames(0, 100)
	file := append(tag, audio...)
	info, err := ParseMP3(fetchAll(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	wantTags := []Tag{{"ARTIST", "One"}, {"ARTIST", "Two"}, {"ALBUMARTIST", "Various"}}
	if !reflect.DeepEqual(info.Tags, wantTags) {
		t.Errorf("tags = %v, want %v", info.Tags, wantTags)
	}
	// CBR: bytes * 8 / 128 kbps.
	if want := int64(len(audio)) * 8 / 128; info.DurationMS != want {
		t.Errorf("duration = %d ms, want %d", info.DurationMS, want)
	}
}

func TestParseMP3RejectsOtherFiles(t *testing.T) {
	file := []byte("this is not audio, just some text that goes on for a while")
	if _, err := ParseMP3(fetchAll(file), int64(len(file))); err == nil {
		t.Fatal("no error for a text file")
	}
}
