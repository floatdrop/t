//go:build darwin && !ios

// Runtime patch for the WKUIDelegate media-capture callback Wails v3
// omits on macOS. See mediapermission_darwin.go for why this exists.

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/runtime.h>

// WKPermissionDecision, from <WebKit/WKUIDelegate.h>:
//   0 Prompt, 1 Grant, 2 Deny.
static const NSInteger kPermissionDecisionGrant = 1;

// grantCapture is installed as
//   -webView:requestMediaCapturePermissionForOrigin:initiatedByFrame:type:decisionHandler:
// The signature must match that selector exactly; `type` is a
// WKMediaCaptureType (camera, microphone, or both).
static void grantCapture(id self, SEL _cmd, WKWebView *webView,
                         WKSecurityOrigin *origin, WKFrameInfo *frame,
                         NSInteger type, void (^decisionHandler)(NSInteger)) {
    decisionHandler(kPermissionDecisionGrant);
}

int tGrantMediaCapture(void) {
    // Wails names its WKUIDelegate class WebviewWindowDelegate. It is
    // compiled into this binary, so the runtime already knows it by the
    // time main runs.
    Class cls = objc_getClass("WebviewWindowDelegate");
    if (cls == NULL) {
        return 0;
    }
    SEL sel = sel_registerName("webView:requestMediaCapturePermissionForOrigin:"
                               "initiatedByFrame:type:decisionHandler:");
    if (class_respondsToSelector(cls, sel)) {
        // Wails implements it now — leave its version alone.
        return 0;
    }
    // Type encoding: void return; self, _cmd; three object arguments;
    // NSInteger (q); block (@?).
    return class_addMethod(cls, sel, (IMP)grantCapture, "v@:@@@q@?") ? 1 : 0;
}
