package conf

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
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

// catalogOSKey is the catalog-root extra naming the operating system the
// publisher is running on, as a §5.6.6 producer field beside the version.
//
// It rides with the version for the same reason and at the same cost: it is
// something every participant already reads about every other participant, and
// a peer that omits it simply shows nothing rather than showing a guess. Which
// platform someone is on is the other half of "is it just me?" — WebKit,
// WebView2 and WebKitGTK are three different engines wearing one API, and a
// fault that only appears on one of them is otherwise invisible from the room.
const catalogOSKey = "tlmstOS"

// PlatformName is the operating system this build runs on, in the spelling a
// person would use. Taken from the build rather than asked of the WebView: the
// Go side knows for certain, where navigator's answer is a string the browser
// chooses and has been steadily degrading for privacy.
func PlatformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

// audioInitRef is the initDataList ID linking the audio track to its
// codec configuration payload (§5.1.7).
const audioInitRef = "audio-config"

// buildCatalog renders the local participant's MSF catalog. video and
// audio may each be nil, which is how a participant that publishes only
// one of the two (or neither, before its encoders have started) is
// described.
func buildCatalog(nickname, version string, video, audio *bridge.TrackConfig) (msf.Catalog, error) {
	live := true
	// Both media tracks belong to one render group so a player knows
	// they are meant to be presented together (§5.2.10).
	renderGroup := 1

	var tracks []msf.Track
	var initData []msf.InitData

	if video != nil {
		v := video
		track := msf.Track{
			Name:        VideoTrack,
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
	cat.Extras[catalogOSKey] = PlatformName()

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
	// OS is the operating system the publisher is on, empty from a peer old
	// enough not to say.
	OS    string
	Video *bridge.TrackConfig
	Audio *bridge.TrackConfig
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
	if v, ok := cat.Extras[catalogOSKey].(string); ok {
		out.OS = v
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
			// Only the track named "video"; anything else with the video role is
			// ignored rather than guessed at.
			if tr.Name == VideoTrack {
				out.Video = cfg
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
