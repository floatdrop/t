//go:build !darwin || ios

package main

import "log/slog"

// grantWebViewMediaCapture does nothing outside macOS.
//
// Wails honours WebviewWindowOptions.Permissions on Linux (WebKitGTK) and
// Windows (WebView2), so the camera and microphone grants set in main are all
// those platforms need. Only the macOS backend ignores that map, which is why
// the darwin build has a real implementation — see mediapermission_darwin.go.
func grantWebViewMediaCapture(log *slog.Logger) {
	log.Debug("WebView media-capture permission is handled by the window's Permissions option")
}
