package media

import "errors"

// Info is what MediaLayer indexes about one audio file.
type Info struct {
	Format     string // "flac", "mp3", "m4a", "caf" or "oga"
	DurationMS int64  // 0 if unknown
	SampleRate int    // 0 if unknown
	BitDepth   int    // 0 if unknown or not meaningful (lossy formats)
	Channels   int    // 0 if unknown
	Tags       []Tag
	Pictures   []Picture
}

// ErrUnsupported is wrapped by the errors for a file in a container
// MediaLayer reads (MP4, CAF, Ogg) whose audio is in a codec it doesn't,
// or that holds no audio at all: not a track, rather than a damaged one.
var ErrUnsupported = errors.New("no audio in a codec MediaLayer reads")

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
