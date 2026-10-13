package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiAudioArgumentsAndCompatibility(t *testing.T) {
	video := MediaStream{Index: 0}
	audio := &MediaStream{Index: 2, Type: "audio", Codec: "aac", Profile: "LC", Channels: 2, BitRate: "192000", Tags: map[string]string{"language": "eng", "title": "Original"}}
	ru := MediaStream{Index: 3, Type: "audio", Codec: "ac3", Channels: 6, Tags: map[string]string{"language": "rus", "title": "Russian dub"}}
	args := strings.Join(encodeArgs("source", "output", video, audio, true, ru), " ")
	for _, want := range []string{"-map 0:2 -map 0:3", "-c:a copy", "-c:a:1 aac", "-b:a:1 192k", "-ac:a:1 2", "-metadata:s:a:0 language=eng", "-metadata:s:a:1 language=rus", "-metadata:s:a:1 title=Russian dub", "-disposition:a:0 default", "-disposition:a:1 0"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %s in %s", want, args)
		}
	}
	probe := MediaProbe{Streams: []MediaStream{video, *audio, ru}}
	if allAudioCompatible(probe) {
		t.Fatal("ignored alternate incompatible audio")
	}
	extra := additionalAudio(probe, audio)
	if len(extra) != 1 || extra[0].Index != 3 {
		t.Fatal(extra)
	}
}
func TestRealFFmpegPreservesAudioLanguages(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	out := filepath.Join(dir, "out.mp4")
	args := []string{"-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=size=64x64:rate=24:duration=1", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-f", "lavfi", "-i", "sine=frequency=880:duration=1", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "libx264", "-c:a", "ac3", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=rus", "-metadata:s:a:1", "title=Russian dub", source}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, data)
	}
	ctx := context.Background()
	probe, err := probeMedia(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	video, audio, err := selectedStreams(probe)
	if err != nil {
		t.Fatal(err)
	}
	args = encodeArgs(source, out, video, audio, true, additionalAudio(probe, audio)...)
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("encode: %v %s", err, data)
	}
	output, err := probeMedia(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	languages := []string{}
	for _, s := range output.Streams {
		if s.Type == "audio" {
			if !compatibleAudio(&s) {
				t.Fatalf("incompatible track: %+v", s)
			}
			languages = append(languages, s.Tags["language"])
		}
	}
	if strings.Join(languages, "/") != "eng/rus" {
		t.Fatal(languages)
	}
	duration, err := probeDuration(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOutput(ctx, out, duration); err != nil {
		t.Fatal(err)
	}
}
