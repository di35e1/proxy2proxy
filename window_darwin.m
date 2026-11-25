#import <Cocoa/Cocoa.h>
#include <stdlib.h>

extern void goInterfaceSelected(const char* bsdName);
extern void goProxySelected(int index);

static NSWindow *mainWindow = nil;
static NSTextField *infoLabel = nil;
static NSTextField *localStatusLabel = nil;
static NSTextField *remoteStatusLabel = nil;
static NSPopUpButton *ifacePopup = nil;
static NSPopUpButton *proxyPopup = nil;
static NSMutableArray<NSString *> *ifaceDevices = nil;
static NSImage *baseAppIcon = nil;
static NSInteger lastDockStatus = -1;
static BOOL exceptionHandlerInstalled = NO;

void showAlertDialog(const char* title, const char* message, int critical);

static void uncaughtExceptionHandler(NSException *exception) {
    NSString *reason = exception.reason ?: @"Неизвестное исключение Cocoa";
    NSArray<NSString *> *stack = [exception callStackSymbols] ?: @[];
    NSUInteger count = MIN((NSUInteger)6, stack.count);
    NSString *topStack = [[stack subarrayWithRange:NSMakeRange(0, count)]
                          componentsJoinedByString:@"\n"];
    NSString *fullText = [NSString stringWithFormat:@"%@\n\n%@", reason, topStack];
    showAlertDialog("Ошибка macOS (NSException)", [fullText UTF8String], 1);
}

static void ensureExceptionHandler(void) {
    if (!exceptionHandlerInstalled) {
        NSSetUncaughtExceptionHandler(&uncaughtExceptionHandler);
        exceptionHandlerInstalled = YES;
    }
}

static NSImage *loadBaseIcon(void) {
    if (baseAppIcon != nil) {
        return baseAppIcon;
    }
    NSString *bundleIconPath = [[NSBundle mainBundle] pathForResource:@"AppIcon" ofType:@"icns"];
    if (bundleIconPath != nil) {
        NSImage *img = [[NSImage alloc] initWithContentsOfFile:bundleIconPath];
        if (img != nil) {
            baseAppIcon = img;
            return baseAppIcon;
        }
    }
    if (@available(macOS 11.0, *)) {
        NSImage *globeIcon = [NSImage imageWithSystemSymbolName:@"globe"
                                      accessibilityDescription:@"Proxy"];
        if (globeIcon != nil) {
            baseAppIcon = globeIcon;
            return baseAppIcon;
        }
    }
    baseAppIcon = [NSImage imageNamed:NSImageNameNetwork];
    return baseAppIcon;
}

static NSImage *renderDockIcon(NSImage *base, int state) {
    NSSize canvasSize = NSMakeSize(512, 512);
    NSImage *result = [[NSImage alloc] initWithSize:canvasSize];
    [result lockFocus];

    // Стандартные пропорции macOS Big Sur+ Dock icon: отступ ~50px (из 512) и радиус ~22.5%
    CGFloat inset = 50.0;
    NSRect iconRect = NSMakeRect(inset, inset,
                                 canvasSize.width - inset * 2.0,
                                 canvasSize.height - inset * 2.0);
    CGFloat cornerRadius = iconRect.size.width * 0.225;
    NSBezierPath *squirclePath = [NSBezierPath bezierPathWithRoundedRect:iconRect
                                                                xRadius:cornerRadius
                                                                yRadius:cornerRadius];

    // Мягкая тень под иконкой, как у системных приложений в Dock
    [NSGraphicsContext saveGraphicsState];
    NSShadow *shadow = [[NSShadow alloc] init];
    [shadow setShadowOffset:NSMakeSize(0, -6.0)];
    [shadow setShadowBlurRadius:14.0];
    [shadow setShadowColor:[NSColor colorWithWhite:0.0 alpha:0.35]];
    [shadow set];
    [[NSColor blackColor] setFill];
    [squirclePath fill];
    [NSGraphicsContext restoreGraphicsState];

    // AppIcon.icns уже содержит squircle-геометрию с прозрачными углами,
    // поэтому базовую иконку рисуем на весь холст. Повторная отрисовка в
    // iconRect уменьшила бы её второй раз; clip остаётся как страховка для
    // fallback-иконок без прозрачной маски.
    [NSGraphicsContext saveGraphicsState];
    [squirclePath addClip];
    [base drawInRect:NSMakeRect(0, 0, canvasSize.width, canvasSize.height)
            fromRect:NSZeroRect
           operation:NSCompositingOperationSourceOver
            fraction:1.0];
    [NSGraphicsContext restoreGraphicsState];

    // Кружок статуса в правом верхнем углу иконки
    if (state == 1 || state == 2) {
        CGFloat dotDiameter = 116.0;
        CGFloat dotX = NSMaxX(iconRect) - dotDiameter + 14.0;
        CGFloat dotY = NSMaxY(iconRect) - dotDiameter + 14.0;
        NSRect dotRect = NSMakeRect(dotX, dotY, dotDiameter, dotDiameter);

        [NSGraphicsContext saveGraphicsState];
        NSShadow *badgeShadow = [[NSShadow alloc] init];
        [badgeShadow setShadowOffset:NSMakeSize(0, -3.0)];
        [badgeShadow setShadowBlurRadius:8.0];
        [badgeShadow setShadowColor:[NSColor colorWithWhite:0.0 alpha:0.30]];
        [badgeShadow set];

        CGFloat ringPad = 10.0;
        NSRect ringRect = NSInsetRect(dotRect, -ringPad, -ringPad);
        [[NSColor colorWithWhite:1.0 alpha:0.96] setFill];
        [[NSBezierPath bezierPathWithOvalInRect:ringRect] fill];
        [NSGraphicsContext restoreGraphicsState];

        NSColor *dotColor = (state == 1)
            ? [NSColor colorWithSRGBRed:0.16 green:0.78 blue:0.35 alpha:1.0]
            : [NSColor colorWithSRGBRed:0.92 green:0.22 blue:0.20 alpha:1.0];
        [dotColor setFill];
        [[NSBezierPath bezierPathWithOvalInRect:dotRect] fill];
    }

    [result unlockFocus];
    return result;
}

// state: 0 = без индикатора, 1 = зелёный (работает), 2 = красный (нет связи)
void setDockStatus(int state) {
    dispatch_async(dispatch_get_main_queue(), ^{
        if (lastDockStatus == state) {
            return;
        }
        lastDockStatus = state;

        NSImage *base = loadBaseIcon();
        if (base == nil) {
            return;
        }

        NSImage *rendered = renderDockIcon(base, state);
        if (rendered != nil) {
            [NSApp setApplicationIconImage:rendered];
        }
    });
}

@interface AppDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate>
@end

@implementation AppDelegate
- (void)interfaceChanged:(id)sender {
    NSInteger idx = [ifacePopup indexOfSelectedItem];
    if (idx >= 0 && (NSUInteger)idx < [ifaceDevices count]) {
        NSString *dev = [ifaceDevices objectAtIndex:(NSUInteger)idx];
        goInterfaceSelected([dev UTF8String]);
    }
}

- (void)proxyChanged:(id)sender {
    NSInteger idx = [proxyPopup indexOfSelectedItem];
    if (idx >= 0) {
        goProxySelected((int)idx);
    }
}

- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)flag {
    if (mainWindow != nil) {
        if ([mainWindow isMiniaturized]) {
            [mainWindow deminiaturize:nil];
        }
        [mainWindow makeKeyAndOrderFront:nil];
        [NSApp activateIgnoringOtherApps:YES];
    }
    return YES;
}

- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
    return YES;
}

- (void)windowWillClose:(NSNotification *)notification {
    [NSApp terminate:nil];
}
@end

static AppDelegate *appDelegate = nil;

static void setupMainMenu(NSString *appTitle) {
    NSMenu *menubar = [[NSMenu alloc] init];

    NSMenuItem *appMenuItem = [[NSMenuItem alloc] init];
    [menubar addItem:appMenuItem];
    NSMenu *appMenu = [[NSMenu alloc] init];
    NSString *quitTitle = [@"Quit " stringByAppendingString:appTitle];
    [appMenu addItemWithTitle:quitTitle action:@selector(terminate:) keyEquivalent:@"q"];
    [appMenuItem setSubmenu:appMenu];

    NSMenuItem *windowMenuItem = [[NSMenuItem alloc] init];
    [menubar addItem:windowMenuItem];
    NSMenu *windowMenu = [[NSMenu alloc] initWithTitle:@"Window"];
    [windowMenu addItemWithTitle:@"Minimize" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
    [windowMenuItem setSubmenu:windowMenu];

    [NSApp setMainMenu:menubar];
}

void showAlertDialog(const char* title, const char* message, int critical) {
    ensureExceptionHandler();
    NSString *nsTitle = (title != NULL) ? [NSString stringWithUTF8String:title] : @"Ошибка";
    NSString *nsMsg = (message != NULL) ? [NSString stringWithUTF8String:message] : @"";

    void (^runAlert)(void) = ^{
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
        [NSApp activateIgnoringOtherApps:YES];

        NSAlert *alert = [[NSAlert alloc] init];
        [alert setMessageText:nsTitle];
        [alert setInformativeText:nsMsg];
        [alert setAlertStyle:critical ? NSAlertStyleCritical : NSAlertStyleWarning];
        [alert addButtonWithTitle:@"OK"];
        [alert runModal];
    };

    if ([NSThread isMainThread]) {
        runAlert();
    } else {
        dispatch_sync(dispatch_get_main_queue(), runAlert);
    }
}

void initWindow(const char* title, const char* infoText) {
    ensureExceptionHandler();
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];

    NSString *nsTitle = [NSString stringWithUTF8String:title];
    setupMainMenu(nsTitle);

    appDelegate = [[AppDelegate alloc] init];
    [NSApp setDelegate:appDelegate];

    ifaceDevices = [[NSMutableArray alloc] init];

    NSRect frame = NSMakeRect(0, 0, 540, 218);
    NSUInteger style = NSWindowStyleMaskTitled |
                       NSWindowStyleMaskClosable |
                       NSWindowStyleMaskMiniaturizable;

    mainWindow = [[NSWindow alloc] initWithContentRect:frame
                                             styleMask:style
                                               backing:NSBackingStoreBuffered
                                                 defer:NO];
    [mainWindow setTitle:nsTitle];
    [mainWindow setDelegate:appDelegate];
    [mainWindow center];

    NSImage *base = loadBaseIcon();
    if (base != nil) {
        [NSApp setApplicationIconImage:renderDockIcon(base, 0)];
    }

    NSView *content = [mainWindow contentView];

    ifacePopup = [[NSPopUpButton alloc] initWithFrame:NSMakeRect(20, 167, 232, 26) pullsDown:NO];
    [ifacePopup setTarget:appDelegate];
    [ifacePopup setAction:@selector(interfaceChanged:)];
    [content addSubview:ifacePopup];

    NSTextField *arrowLabel = [[NSTextField alloc] initWithFrame:NSMakeRect(254, 169, 32, 22)];
    [arrowLabel setStringValue:@"→"];
    [arrowLabel setAlignment:NSTextAlignmentCenter];
    [arrowLabel setBezeled:NO];
    [arrowLabel setDrawsBackground:NO];
    [arrowLabel setEditable:NO];
    [arrowLabel setSelectable:NO];
    [arrowLabel setFont:[NSFont systemFontOfSize:14]];
    if (@available(macOS 10.14, *)) {
        [arrowLabel setTextColor:[NSColor secondaryLabelColor]];
    } else {
        [arrowLabel setTextColor:[NSColor grayColor]];
    }
    [content addSubview:arrowLabel];

    proxyPopup = [[NSPopUpButton alloc] initWithFrame:NSMakeRect(288, 167, 232, 26) pullsDown:NO];
    [proxyPopup setTarget:appDelegate];
    [proxyPopup setAction:@selector(proxyChanged:)];
    [content addSubview:proxyPopup];

    localStatusLabel = [[NSTextField alloc] initWithFrame:NSMakeRect(20, 120, 500, 24)];
    [localStatusLabel setStringValue:@"⏳ Локальный SOCKS5: запуск... | Сессий: 0"];
    [localStatusLabel setBezeled:NO];
    [localStatusLabel setDrawsBackground:NO];
    [localStatusLabel setEditable:NO];
    [localStatusLabel setSelectable:NO];
    [localStatusLabel setFont:[NSFont systemFontOfSize:13]];
    [content addSubview:localStatusLabel];

    remoteStatusLabel = [[NSTextField alloc] initWithFrame:NSMakeRect(20, 90, 500, 24)];
    [remoteStatusLabel setStringValue:@"⏳ Удалённый прокси: ожидание..."];
    [remoteStatusLabel setBezeled:NO];
    [remoteStatusLabel setDrawsBackground:NO];
    [remoteStatusLabel setEditable:NO];
    [remoteStatusLabel setSelectable:NO];
    [remoteStatusLabel setFont:[NSFont systemFontOfSize:13]];
    [content addSubview:remoteStatusLabel];

    NSBox *separator = [[NSBox alloc] initWithFrame:NSMakeRect(20, 72, 500, 1)];
    [separator setBoxType:NSBoxSeparator];
    [content addSubview:separator];

    infoLabel = [[NSTextField alloc] initWithFrame:NSMakeRect(20, 14, 500, 46)];
    [infoLabel setStringValue:[NSString stringWithUTF8String:infoText]];
    [infoLabel setBezeled:NO];
    [infoLabel setDrawsBackground:NO];
    [infoLabel setEditable:NO];
    [infoLabel setSelectable:NO];
    [infoLabel setLineBreakMode:NSLineBreakByWordWrapping];
    [infoLabel setFont:[NSFont systemFontOfSize:11]];
    if (@available(macOS 10.14, *)) {
        [infoLabel setTextColor:[NSColor secondaryLabelColor]];
    } else {
        [infoLabel setTextColor:[NSColor grayColor]];
    }
    [content addSubview:infoLabel];

    [mainWindow makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

void addInterfaceOption(const char* label, const char* bsdName, int selected) {
    NSString *nsLabel = [NSString stringWithUTF8String:label];
    NSString *nsDev = [NSString stringWithUTF8String:bsdName];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (ifacePopup != nil && ifaceDevices != nil) {
            [ifacePopup addItemWithTitle:nsLabel];
            [ifaceDevices addObject:nsDev];
            if (selected) {
                [ifacePopup selectItemAtIndex:(NSInteger)([ifaceDevices count] - 1)];
            }
        }
    });
}

void addProxyOption(const char* label, int selected) {
    NSString *nsLabel = [NSString stringWithUTF8String:label];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (proxyPopup != nil) {
            [proxyPopup addItemWithTitle:nsLabel];
            if (selected) {
                NSInteger lastIdx = [proxyPopup numberOfItems] - 1;
                if (lastIdx >= 0) {
                    [proxyPopup selectItemAtIndex:lastIdx];
                }
            }
        }
    });
}

void setWindowInfo(const char* infoText) {
    NSString *nsInfo = [NSString stringWithUTF8String:infoText];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (infoLabel != nil) {
            [infoLabel setStringValue:nsInfo];
        }
    });
}

void updateLocalStatus(const char* localText) {
    NSString *nsLocal = [NSString stringWithUTF8String:localText];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (localStatusLabel != nil) {
            [localStatusLabel setStringValue:nsLocal];
        }
    });
}

void updateStatus(const char* localText, const char* remoteText) {
    NSString *nsLocal = [NSString stringWithUTF8String:localText];
    NSString *nsRemote = [NSString stringWithUTF8String:remoteText];
    dispatch_async(dispatch_get_main_queue(), ^{
        if (localStatusLabel != nil) {
            [localStatusLabel setStringValue:nsLocal];
        }
        if (remoteStatusLabel != nil) {
            [remoteStatusLabel setStringValue:nsRemote];
        }
    });
}

void runEventLoop(void) {
    [NSApp run];
}