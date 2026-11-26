package media

// Info is what MediaLayer indexes about one audio file.
type Info struct {
	Format     string // "flac", "mp3", "m4a" or "caf"
	DurationMS int64  // 0 if unknown
	SampleRate int    // 0 if unknown
	BitDepth   int    // 0 if unknown or not meaningful (lossy formats)
	Channels   int    // 0 if unknown
	Tags       []Tag
	Pictures   []Picture
}

// Tag is one tag value as the file holds it. Keys are upper-cased Vorbis
// comment names ("ARTIST", "TRACKNUMBER"); ID3 frames are mapped to them.
// A tag with several values appears once per value, in file order.
type Tag struct {
	Key   string
	Value string
}

// Picture is an embedded image.
type Picture struct {
	Type   int // FLAC/ID3 picture type: 3 = front cover, 4 = back cover ...
	MIME   string
	Width  int // 0 if the file does not say
	Height int
	Data   []byte
}
