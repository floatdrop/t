//go:build darwin && !ios

package main

/*
#cgo LDFLAGS: -framework WebKit -framework Cocoa

// Returns 1 when the delegate method was installed, 0 when it was already
// present or the class could not be found.
int tlmstGrantMediaCapture(void);
*/
import "C"

import "log/slog"

// grantWebViewMediaCapture makes getUserMedia work inside the macOS
// WebView.
//
// WKWebView asks its WKUIDelegate for permission before it will hand a
// page a camera or microphone, and denies capture outright when the
// delegate does not implement the request method. Wails v3.0.0-beta.3
// implements that method only on the Linux (WebKitGTK) backend — its macOS
// delegate is silent — so without this every getUserMedia call in the app
// fails with NotAllowedError before macOS is ever consulted.
//
// The fix installs the missing method on Wails' delegate class at runtime
// (see mediapermission_darwin.m). It grants unconditionally, which is the
// right policy here because the WebView renders only this app's own bundled
// frontend: there is no third-party page whose request could need scrutiny,
// and macOS still enforces its own TCC prompt on first use.
//
// Two things must be in place for that prompt to appear at all, both in
// build/darwin/Info.plist: NSCameraUsageDescription and
// NSMicrophoneUsageDescription. macOS terminates a process that touches
// either device without them, so the app has to run from its .app bundle
// (`wails3 task run`), not as a bare binary.
func grantWebViewMediaCapture(log *slog.Logger) {
	if C.tlmstGrantMediaCapture() == 1 {
		log.Debug("installed WebView media-capture permission handler")
		return
	}
	// Either Wails started implementing this itself — in which case this
	// shim can be deleted — or the class name changed and capture will
	// fail. Worth a warning either way.
	log.Warn("could not install WebView media-capture permission handler; " +
		"camera and microphone access may be denied")
}
