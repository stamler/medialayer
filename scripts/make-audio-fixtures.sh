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
src 0.2 96000 stereo; ff -i src.wav -c:a alac -sample_fmt s32p alac24.m4a
src 0.1 48000 5.1; ff -i src.wav -c:a alac -sample_fmt s32p alac24-51.caf
src 0.2 48000 mono; ff -i src.wav -c:a alac -sample_fmt s16p alac16-mono.mp4
# Stretches of silence, which ALAC codes as runs of zeros.
ff -f lavfi -i "aevalsrc=if(lt(mod(t\\,0.1)\\,0.05)\\,0.2*sin(2*PI*440*t)\\,0)|0:s=44100:d=0.4" -c:a alac -sample_fmt s16p alac16-quiet.m4a
ff -f lavfi -i "aevalsrc=if(lt(mod(t\\,0.1)\\,0.05)\\,0.2*sin(2*PI*440*t)\\,0)|0:s=44100:d=0.4" -c:a flac -sample_fmt s16 flac16-quiet.flac
rm -f src.wav cover.png
: > audio-hashes.txt
for f in *.flac *.m4a *.mp4 *.caf *.oga; do
	[ -e "$f" ] || continue
	codec=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name -of csv=p=0 "$f")
	[ "$codec" = flac ] || [ "$codec" = alac ] || continue
	bits=$(ffprobe -v error -select_streams a:0 -show_entries stream=bits_per_raw_sample -of csv=p=0 "$f")
	rate=$(ffprobe -v error -select_streams a:0 -show_entries stream=sample_rate -of csv=p=0 "$f")
	chans=$(ffprobe -v error -select_streams a:0 -show_entries stream=channels -of csv=p=0 "$f")
	case $bits in 8) fmt=s8;; 16) fmt=s16le;; 24) fmt=s24le;; *) fmt=s32le;; esac
	sum=$( (printf 'pcm\0'; python3 -c "import struct,sys; sys.stdout.buffer.write(struct.pack('<IBB',$rate,$chans,$bits))";
		ffmpeg -v error -i "$f" -map 0:a -f "$fmt" -) | sha256sum | cut -d' ' -f1)
	echo "$f $sum" >> audio-hashes.txt
done
