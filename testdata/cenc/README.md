# CENC test fixtures

A 2 s, 160x90 H.264 + mono AAC clip, packaged as DASH (SegmentTemplate with a
SegmentTimeline, 0.5 s segments) three times: in the clear, encrypted with
`cenc`, and encrypted with `cbcs`. All three come from the same fragmented
file, so their segments hold the same samples at the same sizes and a
decrypted sample can be compared with the clear one byte for byte.

| Directory | Scheme | Video | Audio |
|---|---|---|---|
| `clear/` | none | | |
| `cenc/` | `cenc`, AES-CTR, 16-byte per-sample IV | subsamples (two slices on the IDR frames) | whole sample |
| `cbcs/` | `cbcs`, AES-CBC, 16-byte constant IV | pattern 1:9, subsamples | pattern 0:0 (every whole block), whole sample |

Video and audio use different keys; `keys.txt` lists them as `kid:key`. They
are test keys only. Each encrypted init carries a moov-level EME `pssh`
(system ID `1077efec-c0b2-4d02-ace3-3c1e52e2fb4b`). Bento4 writes
`value="cenc"` on the `mp4protection` ContentProtection for the cbcs MPD too.

There is no clear-lead variant: Bento4 1.6 has no option for one. Tests build
that case from the clear segments instead.

## Regenerating

ffmpeg 9.0.2, Bento4 1.6.0-641 (`mp4fragment`, `mp4encrypt` 1.7, `mp4dash`).
The output is not byte-stable across tool versions; the tests only depend on
the three variants matching each other.

```sh
ffmpeg -f lavfi -i testsrc=size=160x90:rate=24:duration=2 \
       -f lavfi -i sine=frequency=440:sample_rate=48000:duration=2 \
       -c:v libx264 -profile:v baseline -preset veryfast -b:v 40k \
       -g 12 -keyint_min 12 -sc_threshold 0 -x264-params slices=2 -pix_fmt yuv420p \
       -c:a aac -b:a 32k -ac 1 -shortest src.mp4
mp4fragment --fragment-duration 500 src.mp4 frag.mp4

VK=00112233445566778899aabbccddeeff VKID=10101010101010101010101010101010
AK=ffeeddccbbaa99887766554433221100 AKID=20202020202020202020202020202020
mp4encrypt --method MPEG-CENC --global-option mpeg-cenc.eme-pssh:true \
  --property 1:KID:$VKID --key 1:$VK:0a0b0c0d0e0f1011 \
  --property 2:KID:$AKID --key 2:$AK:1a1b1c1d1e1f2021 frag.mp4 cenc.mp4
mp4encrypt --method MPEG-CBCS --global-option mpeg-cenc.eme-pssh:true \
  --property 1:KID:$VKID --key 1:$VK:0102030405060708090a0b0c0d0e0f10 \
  --property 2:KID:$AKID --key 2:$AK:1112131415161718191a1b1c1d1e1f20 frag.mp4 cbcs.mp4

mp4dash --force -o clear --mpd-name=manifest.mpd --use-segment-timeline frag.mp4
mp4dash --force -o cenc --mpd-name=manifest.mpd --use-segment-timeline --clearkey cenc.mp4
mp4dash --force -o cbcs --mpd-name=manifest.mpd --use-segment-timeline --clearkey cbcs.mp4
```
