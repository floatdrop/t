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

// catalogVersionKey is the catalog-root extra field carrying the build the
// publisher is running, on the same §5.1 producer-extension footing as the
// nickname above.
//
// In the catalog rather than anywhere else because that is the one thing every
// participant already reads about every other participant, and because it costs
// nothing when absent: a peer on an older build simply omits the field, and the
// roster shows no version for them rather than a wrong one.
const catalogVersionKey = "tlmstVersion"

// catalogTemporalLayersKey is the per-track extra carrying how many temporal
// layers a video track is published on, as a §5.6.6 producer field.
//
// MSF describes SVC as one track per layer, with temporalId and depends saying
// how they relate. This publishes the layers as subgroups of a single track
// instead — a subgroup being the smallest thing §5.1.3 lets a subscriber
// decline and §8 lets a publisher mark sheddable — so neither field says what a
// subscriber here needs to know, and inventing a reading for temporalId would
// be a lie to anyone else parsing the catalog. A producer extra says the one
// true thing plainly and is ignored by everyone it does not concern.
const catalogTemporalLayersKey = "tlmstTemporalLayers"

// audioInitRef is the initDataList ID linking the audio track to its
// codec configuration payload (§5.1.7).
const audioInitRef = "audio-config"

// buildCatalog renders the local participant's MSF catalog. video and
// audio may each be nil, which is how a participant that publishes only
// one of the two (or neither, before its encoders have started) is
// described.
func buildCatalog(nickname, version string, video, videoLow, audio *bridge.TrackConfig) (msf.Catalog, error) {
	live := true
	// Both media tracks belong to one render group so a player knows
	// they are meant to be presented together (§5.2.10).
	renderGroup := 1

	var tracks []msf.Track
	var initData []msf.InitData

	// Both video encodings are declared the same way and differ only in name
	// and size: a subscriber picks whichever fits the tile it will draw.
	for _, layer := range []struct {
		name string
		cfg  *bridge.TrackConfig
	}{{VideoTrack, video}, {VideoLowTrack, videoLow}} {
		if layer.cfg == nil {
			continue
		}
		v := layer.cfg
		track := msf.Track{
			Name:        layer.name,
			Packaging:   msf.PackagingLOC,
			IsLive:      &live,
			Role:        msf.RoleVideo,
			Codec:       v.Codec,
			Width:       v.Width,
			Height:      v.Height,
			Framerate:   v.Framerate,
			Bitrate:     v.Bitrate,
			Timescale:   timescaleMicros,
			RenderGroup: &renderGroup,
		}
		// Declared only when there is more than one, so a flat encoding says
		// nothing rather than claiming a layering it does not have.
		if v.TemporalLayers > 1 {
			track.Extras = map[string]any{
				catalogTemporalLayersKey: int(v.TemporalLayers),
			}
		}
		tracks = append(tracks, track)
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
	// Omitted rather than sent empty when unknown, so that a reader cannot
	// tell an unversioned build from one claiming to be nothing.
	if version != "" {
		cat.Extras[catalogVersionKey] = version
	}

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
	// Version is the build the publisher is running, or empty from a peer
	// old enough not to say.
	Version string
	Video   *bridge.TrackConfig
	// VideoLow is the smaller encoding, when the publisher offers one. Both
	// carry MSF's video role, so they are told apart by track name.
	VideoLow *bridge.TrackConfig
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
	if v, ok := cat.Extras[catalogVersionKey].(string); ok {
		out.Version = v
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
			// Kind stays "video" for both: the layer is a fact about which
			// track was subscribed, not about the pictures inside it, and the
			// frontend decodes either identically.
			cfg := &bridge.TrackConfig{
				Kind:      "video",
				Codec:     tr.Codec,
				Width:     tr.Width,
				Height:    tr.Height,
				Framerate: tr.Framerate,
				Bitrate:   tr.Bitrate,
			}
			// JSON numbers decode as float64 through the extras map, and a
			// publisher old enough not to say leaves it absent — which reads as
			// zero and means the single subgroup every track used to have.
			if n, ok := tr.Extras[catalogTemporalLayersKey].(float64); ok &&
				n > 1 && n <= float64(bridge.MaxTemporalLayer+1) {
				cfg.TemporalLayers = uint8(n)
			}
			// A publisher too old to simulcast names its only video track
			// "video"; anything else with the video role that is not the low
			// layer is ignored rather than guessed at.
			switch tr.Name {
			case VideoTrack:
				out.Video = cfg
			case VideoLowTrack:
				out.VideoLow = cfg
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
