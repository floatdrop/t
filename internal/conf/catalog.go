package conf

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/floatdrop/moq-go/pkg/moqt/msf"

	"tlmst/internal/bridge"
)

// catalogNicknameKey is the catalog-root extra field carrying the
// publisher's display name. MSF has no nickname concept, and §5.1 lets
// producers add their own root fields, so the participant's name rides
// there rather than being forced into a track label.
const catalogNicknameKey = "tlmstNickname"

// audioInitRef is the initDataList ID linking the audio track to its
// codec configuration payload (§5.1.7).
const audioInitRef = "audio-config"

// buildCatalog renders the local participant's MSF catalog. video and
// audio may each be nil, which is how a participant that publishes only
// one of the two (or neither, before its encoders have started) is
// described.
func buildCatalog(nickname string, video, audio *bridge.TrackConfig) (msf.Catalog, error) {
	live := true
	// Both media tracks belong to one render group so a player knows
	// they are meant to be presented together (§5.2.10).
	renderGroup := 1

	var tracks []msf.Track
	var initData []msf.InitData

	if video != nil {
		tracks = append(tracks, msf.Track{
			Name:        VideoTrack,
			Packaging:   msf.PackagingLOC,
			IsLive:      &live,
			Role:        msf.RoleVideo,
			Codec:       video.Codec,
			Width:       video.Width,
			Height:      video.Height,
			Framerate:   video.Framerate,
			Bitrate:     video.Bitrate,
			Timescale:   timescaleMicros,
			RenderGroup: &renderGroup,
		})
	}
	if audio != nil {
		track := msf.Track{
			Name:          AudioTrack,
			Packaging:     msf.PackagingLOC,
			IsLive:        &live,
			Role:          msf.RoleAudio,
			Codec:         audio.Codec,
			Samplerate:    audio.SampleRate,
			ChannelConfig: strconv.FormatUint(uint64(audio.Channels), 10),
			Bitrate:       audio.Bitrate,
			Timescale:     timescaleMicros,
			RenderGroup:   &renderGroup,
		}
		// Opus needs its 19-byte OpusHead to configure a decoder. §5.1.7
		// carries exactly that, as a base64 inline payload the track
		// references by ID.
		if audio.Description != "" {
			track.InitRef = audioInitRef
			initData = append(initData, msf.InitData{
				ID:   audioInitRef,
				Type: msf.InitDataTypeInline,
				Data: audio.Description,
			})
		}
		tracks = append(tracks, track)
	}

	cat := msf.BeginBroadcast(tracks, time.Time{})
	cat.InitDataList = initData
	cat.Extras = map[string]any{catalogNicknameKey: nickname}

	if err := cat.Validate(); err != nil {
		return msf.Catalog{}, fmt.Errorf("conf: build catalog: %w", err)
	}
	return cat, nil
}

// encodeCatalog marshals a catalog to the bytes that go in a catalog
// track object.
func encodeCatalog(cat msf.Catalog) ([]byte, error) {
	b, err := json.Marshal(cat)
	if err != nil {
		return nil, fmt.Errorf("conf: marshal catalog: %w", err)
	}
	return b, nil
}

// parsedCatalog is what a subscriber extracts from a remote catalog: the
// publisher's name plus a decoder config per media track it declares.
type parsedCatalog struct {
	Nickname string
	Video    *bridge.TrackConfig
	Audio    *bridge.TrackConfig
	// Complete reports the §11.3 terminator catalog — the publisher has
	// ended the broadcast and every track is done.
	Complete bool
}

// parseCatalog decodes and validates a catalog object payload.
func parseCatalog(payload []byte) (parsedCatalog, error) {
	var cat msf.Catalog
	if err := json.Unmarshal(payload, &cat); err != nil {
		return parsedCatalog{}, fmt.Errorf("conf: parse catalog: %w", err)
	}
	if err := cat.Validate(); err != nil {
		return parsedCatalog{}, fmt.Errorf("conf: invalid catalog: %w", err)
	}

	out := parsedCatalog{Complete: cat.IsComplete}
	if name, ok := cat.Extras[catalogNicknameKey].(string); ok {
		out.Nickname = name
	}

	// initDataList entries are referenced by ID from the tracks below.
	inits := make(map[string]string, len(cat.InitDataList))
	for _, d := range cat.InitDataList {
		if d.Type == msf.InitDataTypeInline {
			inits[d.ID] = d.Data
		}
	}

	for _, tr := range cat.Tracks {
		if tr.Packaging != msf.PackagingLOC {
			continue
		}
		switch tr.Role {
		case msf.RoleVideo:
			out.Video = &bridge.TrackConfig{
				Kind:      "video",
				Codec:     tr.Codec,
				Width:     tr.Width,
				Height:    tr.Height,
				Framerate: tr.Framerate,
				Bitrate:   tr.Bitrate,
			}
		case msf.RoleAudio:
			channels, err := strconv.ParseUint(tr.ChannelConfig, 10, 32)
			if err != nil {
				// channelConfig is free-form in the draft; a value we
				// can't read means mono is the safest assumption.
				channels = 1
			}
			out.Audio = &bridge.TrackConfig{
				Kind:        "audio",
				Codec:       tr.Codec,
				SampleRate:  tr.Samplerate,
				Channels:    uint32(channels),
				Bitrate:     tr.Bitrate,
				Description: inits[tr.InitRef],
			}
		}
	}
	return out, nil
}

// decodeDescription turns a base64 codec description into raw bytes for
// the LOC property that carries it on the wire.
func decodeDescription(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("conf: decode codec description: %w", err)
	}
	return raw, nil
}
