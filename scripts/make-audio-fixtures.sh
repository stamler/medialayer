#!/bin/sh
# Makes the lossless test files in internal/media/testdata with ffmpeg,
# and audio-hashes.txt, each file's expected audio hash (see pcmHash in
# internal/media/flacpcm.go) worked out from ffmpeg's own decoding.
set -eu
out=$(cd "$(dirname "$0")/../internal/media/testdata" && pwd)
cd "$out"
ff() { ffmpeg -hide_banner -loglevel error -y "$@"; }
src() { # seconds rate channels
	ff -f lavfi -i "sine=frequency=440:duration=$1:sample_rate=$2" -f lavfi -i "anoisesrc=d=$1:a=0.3:r=$2:seed=7" \
		-filter_complex "[0][1]amix=inputs=2,aformat=channel_layouts=$3" -f wav -c:a pcm_s32le src.wav
}
src 0.3 44100 stereo; ff -i src.wav -c:a flac -compression_level 8 -sample_fmt s16 flac16.flac
ff -i src.wav -c:a flac -compression_level 0 -sample_fmt s16 flac16-fixed.flac
src 0.2 96000 stereo; ff -i src.wav -c:a flac -sample_fmt s32 -bits_per_raw_sample 24 flac24.flac
src 0.2 48000 stereo; ff -i src.wav -c:a flac -sample_fmt s32 -strict experimental flac32.flac
src 0.2 22050 mono; ff -i src.wav -c:a flac -sample_fmt s16 flac16-mono.flac
src 0.1 48000 5.1; ff -i src.wav -c:a flac -sample_fmt s16 flac16-51.flac
ff -i src.wav -c:a alac -sample_fmt s16p alac16-51.m4a
# ALAC of the same audio as flac16.flac, tagged and with a cover.
src 0.3 44100 stereo
ff -f lavfi -i "color=c=red:s=32x32:d=1" -frames:v 1 cover.png
ff -i src.wav -c:a alac -sample_fmt s16p alac16.m4a
ff -i src.wav -i cover.png -map 0 -map 1 -c:a alac -sample_fmt s16p -c:v copy -disposition:v attached_pic \
	-metadata title="A Title" -metadata artist="An Artist" -metadata album="An Album" -metadata track=3/12 \
	-metadata disc=1/2 -metadata genre=Rock -metadata date=2024 -metadata album_artist="Various" alac16-tagged.m4a
ff -i src.wav -c:a alac -sample_fmt s16p -metadata title="A Title" -metadata artist="An Artist" \
	-metadata album="An Album" -metadata track=3/12 -metadata date=2024 alac16.caf
ff -i src.wav -c:a aac -b:a 96k aac.m4a
ff -i src.wav -i cover.png -map 0 -map 1 -c:a aac -b:a 96k -c:v copy -disposition:v attached_pic \
	-metadata title="A Title" -metadata artist="An Artist" aac-tagged.m4a
# FLAC in Ogg, tagged, of the same audio as flac16.flac.
ff -i src.wav -c:a flac -sample_fmt s16 -metadata title="A Title" -metadata artist="An Artist" -fflags +bitexact flac16.oga
# Remuxes, which must hash the same: AAC with its bit rates rewritten,
# ALAC the way QuickTime stores it.
ff -i aac.m4a -c copy -movflags faststart aac-remux.m4a
ff -i alac16.m4a -c copy -f mov alac16-qt.m4a
# A video and Opus in Ogg, which are not tracks.
ff -i src.wav -c:a libopus -b:a 32k -fflags +bitexact opus.ogg
ff -f lavfi -i "color=c=blue:s=16x16:d=0.3" -i src.wav -c:v mjpeg -c:a aac -b:a 32k -shortest video.mp4
src 0.2 96000 stereo; ff -i src.wav -c:a alac -sample_fmt s32p alac24.m4a
src 0.1 48000 5.1; ff -i src.wav -c:a alac -sample_fmt s32p alac24-51.caf
ff -i src.wav -c:a flac -sample_fmt s32 -bits_per_raw_sample 24 -fflags +bitexact flac24-51.ogg
src 0.2 48000 mono; ff -i src.wav -c:a alac -sample_fmt s16p alac16-mono.mp4
# Stretches of silence, which ALAC codes as runs of zeros.
ff -f lavfi -i "aevalsrc=if(lt(mod(t\\,0.1)\\,0.05)\\,0.2*sin(2*PI*440*t)\\,0)|0:s=44100:d=0.4" -c:a alac -sample_fmt s16p alac16-quiet.m4a
ff -f lavfi -i "aevalsrc=if(lt(mod(t\\,0.1)\\,0.05)\\,0.2*sin(2*PI*440*t)\\,0)|0:s=44100:d=0.4" -c:a flac -sample_fmt s16 flac16-quiet.flac
# Cut from a longer stream without renumbering its frames, and two FLAC
# streams multiplexed in one Ogg file.
# ffmpeg keeps the long stream's STREAMINFO, so the cut's sample count is
# set, and its MD5 cleared, as a tool that renumbered nothing would.
ff -ss 0.2 -i flac16-quiet.flac -c copy flac16-cut.flac
ff -ss 0.2 -i flac16-quiet.flac -c copy -fflags +bitexact flac16-cut.oga
n=$(ffmpeg -v error -i flac16-cut.flac -f s16le - | wc -c); n=$((n / 4))
python3 - "$n" <<'PY'
import struct, sys
n = int(sys.argv[1])
def patch(si):  # a STREAMINFO body
	x = struct.unpack('>Q', si[10:18])[0] & ~((1 << 36) - 1) | n
	si[10:18] = struct.pack('>Q', x)
	si[18:34] = bytes(16)
b = bytearray(open('flac16-cut.flac', 'rb').read())
patch(memoryview(b)[8:42])
open('flac16-cut.flac', 'wb').write(b)
b = bytearray(open('flac16-cut.oga', 'rb').read())
body = 27 + b[26]  # the first page holds just the mapping header
end = body + sum(b[27:body])
patch(memoryview(b)[body + 17:body + 51])
b[22:26] = bytes(4)
crc = 0
for c in b[:end]:
	crc ^= c << 24
	for _ in range(8):
		crc = (crc << 1 ^ 0x04c11db7 if crc & 0x80000000 else crc << 1) & 0xffffffff
b[22:26] = struct.pack('<I', crc)
open('flac16-cut.oga', 'wb').write(b)
PY
ff -i flac16.flac -i flac16-mono.flac -map 0 -map 1 -c copy -f ogg -fflags +bitexact flac16-mux.oga
# Left at the 32-bit maximum, right at the minimum: ffmpeg wraps the side
# channel at 32 bits, which RFC 9639 doesn't allow, so it has no hash.
python3 -c "import struct,sys; n=2400; sys.stdout.buffer.write(b'RIFF'+struct.pack('<I',36+8*n)+b'WAVEfmt '+
	struct.pack('<IHHIIHH',16,1,2,48000,384000,8,32)+b'data'+struct.pack('<I',8*n)+b''.join(struct.pack('<ii',*((2**31-1,-2**31) if i%7<3 else (-2**31,2**31-1))) for i in range(n)))" > src.wav
ff -i src.wav -c:a flac -strict experimental -ch_mode left_side -lpc_type none flac32-wrapped-side.flac
rm -f src.wav cover.png
: > audio-hashes.txt
for f in *.flac *.m4a *.mp4 *.caf *.oga *.ogg; do
	[ -e "$f" ] || continue
	codec=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name -of csv=p=0 "$f")
	[ "$codec" = flac ] || [ "$codec" = alac ] || continue
	case $f in *-wrapped-side.* | *-mux.*) continue;; esac
	bits=$(ffprobe -v error -select_streams a:0 -show_entries stream=bits_per_raw_sample -of csv=p=0 "$f")
	rate=$(ffprobe -v error -select_streams a:0 -show_entries stream=sample_rate -of csv=p=0 "$f")
	chans=$(ffprobe -v error -select_streams a:0 -show_entries stream=channels -of csv=p=0 "$f")
	# Only whole-byte depths: ffmpeg shifts others up to fill a byte.
	case $bits in 8) fmt=s8;; 16) fmt=s16le;; 24) fmt=s24le;; 32) fmt=s32le;; *) echo "$f: $bits bits" >&2; exit 1;; esac
	sum=$( (printf 'pcm\0'; python3 -c "import struct,sys; sys.stdout.buffer.write(struct.pack('<IBB',$rate,$chans,$bits))";
		ffmpeg -v error -i "$f" -map 0:a -f "$fmt" -) | sha256sum | cut -d' ' -f1)
	echo "$f $sum" >> audio-hashes.txt
done
