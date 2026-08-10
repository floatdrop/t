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
	"os/user"
	"strings"
	"time"

	"github.com/lmittmann/tint"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"tlmst/internal/app"
	"tlmst/internal/bridge"
	"tlmst/internal/conf"
	"tlmst/internal/telemetry"
	"tlmst/internal/version"
)

//go:embed all:frontend/dist
var assets embed.FS

// buildConfig is the file the packaging reads to stamp the bundle and name the
// release archives. Embedded so the running binary can report the same version
// it was packaged as, without a second copy of the number to keep in step —
// see internal/version.
//
//go:embed build/config.yml
var buildConfig []byte

// bridgeEndpointPath is where the frontend reads the WebSocket URL and
// token. Serving it from the asset handler means the WebView needs no
// injected globals and no generated bindings to find the backend.
const bridgeEndpointPath = "/__bridge"

// inviteScheme is the URL scheme registered in build/darwin/Info.plist, which
// makes t://<relay>/<room> links clickable.
//
// One letter, matching the binary. A scheme this short is a real collision risk
// with anything else that claims it — LaunchServices hands a scheme to whichever
// bundle registered it most recently — so an invite opening the wrong
// application is the failure to expect, not a malformed link.
const inviteScheme = "t"

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

	appVersion := version.Parse(buildConfig)
	logger.Info("starting tlmst", "version", appVersion)

	backend := app.New(logger, sink, appVersion)
	server, err := bridge.NewServer(logger, backend)
	if err != nil {
		log.Fatal(err)
	}
	backend.SetServer(server)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		if err := server.Serve(ctx); err != nil && ctx.Err() == nil {
			// Fatal, not merely logged. The listener is the only way the
			// WebView reaches the backend: without it nothing can join, no
			// call can be made, and every future reconnect attempt finds a
			// closed port — while the window stays open looking perfectly
			// usable. An app that cannot do the one thing it is for should
			// stop rather than pretend.
			logger.Error("bridge server stopped; the app cannot serve its own frontend", "err", err)
			log.Fatal(err)
		}
	}()
	endpoint := server.Endpoint()
	// The frontend reads this before anything else, which makes it the
	// earliest place the welcome screen can learn what build it is part of.
	endpoint.Version = appVersion
	endpoint.OS = conf.PlatformName()
	logger.Info("bridge listening", "url", endpoint.URL)

	// grantWebViewMediaCapture must run before any window exists: it
	// patches the WebView delegate class that the first window
	// instantiates. Without it macOS silently denies getUserMedia.
	grantWebViewMediaCapture(logger)

	wailsApp := application.New(application.Options{
		Name:        "t",
		Description: "Teleconferencing over Media over QUIC",
		Assets: application.AssetOptions{
			Handler: assetHandler(endpoint),
		},
		// Leave the room before the process goes, not after.
		//
		// Closing the window terminates the app (see the Mac option below), and
		// the Shutdown after wailsApp.Run() below never gets to run when it
		// does — measured: the peer kept showing a tile for a departed
		// participant for a full ten seconds, which is exactly the QUIC idle
		// timeout. Nothing had closed the session, so the relay had no way to
		// know anyone had gone until it gave up waiting, and only then told
		// the room.
		//
		// This hook runs synchronously on the main thread while the app is
		// still alive, so the session closes properly and the relay reports
		// the departure at once. Shutdown is idempotent, so the call after
		// Run() returning stays where it is for the paths that do reach it.
		OnShutdown: backend.Shutdown,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// Opening a link is the OS's job, not the WebView's: navigating the
	// WebView to a release page would replace the app with a web page and
	// leave no way back to the call.
	backend.SetOpenURL(wailsApp.Browser.OpenURL)

	// Asked once, in the background, and never waited on — the answer is a
	// button that may or may not appear on the welcome screen.
	go backend.CheckForUpdate(ctx)

	// Invite links. Wails already installs the macOS Apple Event handler for
	// custom URL schemes and republishes it as this event, so registering
	// "t" in Info.plist (CFBundleURLTypes) is all it takes to make
	// t://<relay>/<room> clickable. Fires both when a link launches
	// the app and when one arrives while it is already running.
	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl,
		func(event *application.ApplicationEvent) {
			relay, room, ok := parseInviteURL(event.Context().URL())
			if !ok {
				logger.Warn("ignoring unusable invite link", "url", event.Context().URL())
				return
			}
			backend.OpenInvite(relay, room)
		})

	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "t",
		Width:  1280,
		Height: 860,
		// No minimum. The grid is built to cope with being small — it drops to
		// one column, and a tile narrow enough stops being worth the full
		// picture and takes the publisher's smaller encoding instead. A floor
		// of 900 meant that second behaviour could not be reached at all
		// except by filling the room, which is not how anyone would meet it.
		BackgroundColour: application.NewRGB(11, 13, 18),
		URL: startURL(launch{
			relay:    *relayFlag,
			room:     *roomFlag,
			nickname: *nickFlag,
			user:     systemUserName(),
			join:     *autoJoin,
			debug:    *debugOpen,
			debugTab: *debugTab,
		}),
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
			// Without this the Fullscreen API is simply refused: WKWebView
			// defaults elementFullscreenEnabled to false, and Wails only sets
			// it when asked, so requestFullscreen on a tile rejects and the
			// control silently does nothing but expand within the window.
			WebviewPreferences: application.MacWebviewPreferences{
				FullscreenEnabled: application.Enabled,
			},
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

// launch is what the welcome screen is told at startup: the flags that
// prefill its form, plus the system's own idea of who is using it.
//
// A struct rather than a parameter list because two of these are adjacent
// strings that mean different things — transposing nickname and user would
// compile and quietly change behaviour.
type launch struct {
	relay    string
	room     string
	nickname string
	// user is the OS account name, which the nickname field falls back to when
	// nothing else has named the user. Kept separate from nickname so that an
	// explicit -nickname, and a nickname saved from a previous run, both still
	// take precedence over it.
	user     string
	join     bool
	debug    bool
	debugTab string
}

// startURL turns the launch values into the query string the welcome screen
// reads. Empty values are omitted, so a bare interactive start with no system
// account name yields "/".
func startURL(l launch) string {
	q := url.Values{}
	if l.relay != "" {
		q.Set("relay", l.relay)
	}
	if l.room != "" {
		q.Set("room", l.room)
	}
	if l.nickname != "" {
		q.Set("nickname", l.nickname)
	}
	if l.user != "" {
		q.Set("user", l.user)
	}
	if l.join {
		q.Set("join", "1")
	}
	if l.debug {
		q.Set("debug", "1")
	}
	if l.debugTab != "" {
		q.Set("debugTab", l.debugTab)
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// systemUserName is the OS account name, for the welcome screen to offer as a
// nickname. Empty when the account cannot be determined, which is not worth
// failing over: the frontend falls back to a generated name.
func systemUserName() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	name := u.Username
	// Windows reports DOMAIN\user, and the domain is not part of anyone's name.
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

// parseInviteURL pulls the relay and room out of an invite link —
// tlmst://<relay>/<room>, where the relay is the authority and the room the
// path. Must stay in step with buildInviteLink in frontend/src/lib/invite.ts.
//
// A `relay` query parameter overrides the authority. It is only present when
// the relay is more than a bare host:port (a moqt:// or https:// URL, possibly
// with a path), which an authority cannot express on its own.
//
// Both halves are required: half an invite cannot be joined.
func parseInviteURL(raw string) (relay, room string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != inviteScheme {
		return "", "", false
	}
	relay = strings.TrimSpace(u.Query().Get("relay"))
	if relay == "" {
		relay = u.Host
	}
	// url.Parse has already percent-decoded Path.
	room = strings.TrimSpace(strings.Trim(u.Path, "/"))
	if relay == "" || room == "" {
		return "", "", false
	}
	return relay, room, true
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
