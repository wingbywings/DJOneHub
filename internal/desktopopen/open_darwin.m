//go:build darwin && cgo
#import <AppKit/AppKit.h>
int dj_web_open(const char *value) {
    @autoreleasepool {
        NSURL *url = [NSURL URLWithString:[NSString stringWithUTF8String:value]];
        return url && [[NSWorkspace sharedWorkspace] openURL:url] ? 1 : 0;
    }
}
