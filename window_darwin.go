package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

void showAlertDialog(const char* title, const char* message, int critical);
void setDockStatus(int state);
void initWindow(const char* title, const char* infoText);
void addInterfaceOption(const char* label, const char* bsdName, int selected);
void addProxyOption(const char* label, int selected);
void setWindowInfo(const char* infoText);
void updateLocalStatus(const char* localText);
void updateStatus(const char* localText, const char* remoteText);
void runEventLoop(void);
*/
import "C"
import "unsafe"

const (
	dockStatusNone  = 0
	dockStatusGreen = 1
	dockStatusRed   = 2
)

//export goInterfaceSelected
func goInterfaceSelected(bsdName *C.char) {
	dev := C.GoString(bsdName)
	go switchInterface(dev)
}

//export goProxySelected
func goProxySelected(index C.int) {
	go switchProxy(int(index))
}

func showAlert(title, message string, critical bool) {
	cTitle := C.CString(title)
	cMsg := C.CString(message)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cMsg))
	crit := C.int(0)
	if critical {
		crit = C.int(1)
	}
	C.showAlertDialog(cTitle, cMsg, crit)
}

func setDockBadgeStatus(state int) {
	C.setDockStatus(C.int(state))
}

func setupWindow(title, infoText string) {
	cTitle := C.CString(title)
	cInfo := C.CString(infoText)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cInfo))
	C.initWindow(cTitle, cInfo)
}

func addWindowInterfaceOption(label, bsdName string, selected bool) {
	cLabel := C.CString(label)
	cDev := C.CString(bsdName)
	defer C.free(unsafe.Pointer(cLabel))
	defer C.free(unsafe.Pointer(cDev))
	sel := C.int(0)
	if selected {
		sel = C.int(1)
	}
	C.addInterfaceOption(cLabel, cDev, sel)
}

func addWindowProxyOption(label string, selected bool) {
	cLabel := C.CString(label)
	defer C.free(unsafe.Pointer(cLabel))
	sel := C.int(0)
	if selected {
		sel = C.int(1)
	}
	C.addProxyOption(cLabel, sel)
}

func setWindowInfo(infoText string) {
	cInfo := C.CString(infoText)
	defer C.free(unsafe.Pointer(cInfo))
	C.setWindowInfo(cInfo)
}

func setLocalWindowStatus(localText string) {
	cLocal := C.CString(localText)
	defer C.free(unsafe.Pointer(cLocal))
	C.updateLocalStatus(cLocal)
}

func setWindowStatus(localText, remoteText string) {
	cLocal := C.CString(localText)
	cRemote := C.CString(remoteText)
	defer C.free(unsafe.Pointer(cLocal))
	defer C.free(unsafe.Pointer(cRemote))
	C.updateStatus(cLocal, cRemote)
}

func runWindow() {
	windowRunning.Store(true)
	C.runEventLoop()
}
