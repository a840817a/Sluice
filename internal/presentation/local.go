package presentation

import (
	"fmt"
	"io/fs"
	"sort"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
)

// LocalFile is one relative export file. Local returns dependencies first and
// the entry manifest last, so callers never publish an entry before its children.
type LocalFile struct {
	Name string
	Data []byte
	Mode fs.FileMode
}

// Local renders a complete filesystem presentation of a VOD: snap is a
// snapshot of ch.Index, the VOD index, which no longer changes. HLS playlists
// read each track's Timeline from ch.Index. It preserves holes, unlike served
// live playlists. It does no writes or network I/O; encrypted HLS requires
// keys already acquired by the VOD checkpoint.
func (g *Generator) Local(ch Channel, snap index.Snapshot, dir string, keys *hlskey.Cache) ([]LocalFile, error) {
	if !ch.IsHLSSource {
		data, err := manifest.GenerateLocalMPD(ch.ParsedMPD, snap, dir)
		if err != nil {
			return nil, err
		}
		return []LocalFile{{Name: manifest.LocalManifestName, Data: data, Mode: 0o644}}, nil
	}
	published := map[string]bool{}
	snap.ForEachRep(func(ref index.RepRef, segs []index.SegmentState) {
		if len(segs) > 0 {
			published[ref.RepID] = true
		}
	})
	// HLS ingest gives every track a unique RepID. Sort independently of discovery
	// order so numbered file names stay stable for a fixed set of exported tracks.
	var ids []string
	for id := range published {
		if ch.Tracks.KnownRep(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("local presentation: no published tracks")
	}
	names := map[string]string{}
	for i, id := range ids {
		names[id] = hlsout.LocalMediaName(i)
	}
	opts := hlsout.MasterOptions{IndependentSegments: ch.Tracks.IndependentSegments}
	audioGroups, subtitleGroups := map[string]bool{}, map[string]bool{}
	for _, r := range ch.Tracks.Renditions {
		if names[r.RepID] != "" {
			opts.Renditions = append(opts.Renditions, r)
			audioGroups[r.GroupID] = true
		}
	}
	for _, s := range ch.Tracks.Subtitles {
		if names[s.RepID] != "" {
			opts.Subtitles = append(opts.Subtitles, s)
			subtitleGroups[s.GroupID] = true
		}
	}
	var variants []hlsdesc.Variant
	for _, v := range ch.Tracks.Variants {
		if names[v.RepID] == "" {
			continue
		}
		if !audioGroups[v.AudioGroup] {
			v.AudioGroup = ""
		}
		if !subtitleGroups[v.SubtitleGroup] {
			v.SubtitleGroup = ""
		}
		v.Bandwidth = max(v.Bandwidth, 1)
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return nil, fmt.Errorf("local presentation: no published variants")
	}
	var files []LocalFile
	keyNames := map[string]string{}
	for _, id := range ids {
		var parts []index.TimelinePeriod
		for _, p := range ch.Index.Timeline(id, ch.PeriodDocOrder()) {
			if len(p.Segments) == 0 {
				continue
			}
			parts = append(parts, p)
			for _, s := range p.Segments {
				if s.KeyURI == "" || keyNames[s.KeyURI] != "" {
					continue
				}
				data, err := keys.Cached(s.KeyURI)
				if err != nil {
					return nil, err
				}
				name := "local_key_" + hlskey.KeyID(s.KeyURI) + ".key"
				keyNames[s.KeyURI] = name
				files = append(files, LocalFile{Name: name, Data: data, Mode: 0o600})
			}
		}
		data, err := hlsout.LocalMedia(parts, dir, keyNames)
		if err != nil {
			return nil, err
		}
		files = append(files, LocalFile{Name: names[id], Data: data, Mode: 0o644})
	}
	files = append(files, LocalFile{Name: hlsout.LocalMasterName, Data: hlsout.LocalMasterTracks(variants, opts, names), Mode: 0o644})
	return files, nil
}
