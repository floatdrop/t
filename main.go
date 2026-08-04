// Command tlmst is a teleconference client that carries every
// participant's camera and microphone over Media over QUIC.
//
// The process is split in two halves. The WebView owns the media: it holds
// the camera and microphone, runs the WebCodecs encoders and decoders, and
// paints the video grid. This Go half owns the MOQ session: it dials the
// relay, publishes the local participant's tracks, discovers everyone else
// in the room, and subscribes to them. The two halves talk over a loopback
// WebSocket (see internal/bridge).
package main

import (
	"context"
	"embed"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/lmittmann/tint"
	"github.com/wailsapp/wails/v3/pkg/application"

	"tlmst/internal/app"
	"tlmst/internal/bridge"
	"tlmst/internal/telemetry"
)

//go:embed all:frontend/dist
var assets embed.FS

// bridgeEndpointPath is where the frontend reads the WebSocket URL and
// token. Serving it from the asset handler means the WebView needs no
// injected globals and no generated bindings to find the backend.
const bridgeEndpointPath = "/__bridge"

func main() {
	// Launch flags prefill (and optionally submit) the welcome form. They
	// exist so a call can be started from the command line — useful for
	// testing two instances against a relay, and for handling a room link.
	relayFlag := flag.String("relay", "", "relay address to prefill on the welcome screen")
	roomFlag := flag.String("room", "", "room to prefill on the welcome screen")
	nickFlag := flag.String("nickname", "", "nickname to prefill on the welcome screen")
	autoJoin := flag.Bool("join", false, "join immediately, without waiting for a click")
	debugOpen := flag.Bool("debug", false, "open the debug drawer at start")
	debugTab := flag.String("debug-tab", "", "debug tab to open: transport, tracks, or logs")
	flag.Parse()

	// Everything the backend logs goes two places: the terminal, for
	// development, and a ring buffer the debug panel streams from. The
	// sink owns the level so the panel can raise it at runtime.
	sink := telemetry.NewLogSink(slog.LevelInfo)
	logger := slog.New(telemetry.MultiHandler{
		tint.NewTextHandler(os.Stderr, &tint.Options{
			Level:      slog.LevelDebug,
			TimeFormat: time.TimeOnly,
		}),
		sink,
	})
	// moq-go logs through slog's default logger, so this is what routes
	// the transport's own diagnostics into the debug panel.
	slog.SetDefault(logger)

	backend := app.New(logger, sink)
	server, err := bridge.NewServer(logger, backend)
	if err != nil {
		log.Fatal(err)
	}
	backend.SetServer(server)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		if err := server.Serve(ctx); err != nil {
			logger.Error("bridge server stopped", "err", err)
		}
	}()
	endpoint := server.Endpoint()
	logger.Info("bridge listening", "url", endpoint.URL)

	// grantWebViewMediaCapture must run before any window exists: it
	// patches the WebView delegate class that the first window
	// instantiates. Without it macOS silently denies getUserMedia.
	grantWebViewMediaCapture(logger)

	wailsApp := application.New(application.Options{
		Name:        "tlmst",
		Description: "Teleconferencing over Media over QUIC",
		Assets: application.AssetOptions{
			Handler: assetHandler(endpoint),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "tlmst",
		Width:            1280,
		Height:           860,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: application.NewRGB(11, 13, 18),
		URL:              startURL(*relayFlag, *roomFlag, *nickFlag, *autoJoin, *debugOpen, *debugTab),
		// The window renders only this app's own bundled frontend, so a
		// capture request can only have come from us — there is no
		// third-party page whose request would need scrutiny. Granting
		// up front is what Linux (WebKitGTK, no prompt mechanism) and
		// Windows (WebView2) act on. macOS ignores this map entirely, which
		// is why grantWebViewMediaCapture above exists; the OS still
		// enforces its own TCC prompt there either way.
		Permissions: map[application.PermissionType]application.Permission{
			application.PermissionCamera:     application.PermissionAllow,
			application.PermissionMicrophone: application.PermissionAllow,
		},
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 42,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
	})

	err = wailsApp.Run()

	backend.Shutdown()
	stop()
	_ = server.Close()

	if err != nil {
		log.Fatal(err)
	}
}

// startURL turns the launch flags into the query string the welcome screen
// reads. Empty flags yield a bare "/", which is the normal interactive case.
func startURL(relay, room, nickname string, join, debug bool, debugTab string) string {
	q := url.Values{}
	if relay != "" {
		q.Set("relay", relay)
	}
	if room != "" {
		q.Set("room", room)
	}
	if nickname != "" {
		q.Set("nickname", nickname)
	}
	if join {
		q.Set("join", "1")
	}
	if debug {
		q.Set("debug", "1")
	}
	if debugTab != "" {
		q.Set("debugTab", debugTab)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// assetHandler serves the embedded frontend, plus the one extra route the
// frontend needs to reach the bridge.
func assetHandler(endpoint bridge.Endpoint) http.Handler {
	files := application.AssetFileServerFS(assets)
	body := endpoint.JSON()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == bridgeEndpointPath {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
			return
		}
		files.ServeHTTP(w, r)
	})
}
