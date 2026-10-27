package main

// On macOS, video sinks need a Cocoa (NSApplication) event loop running on the
// process's main thread, otherwise no window is ever shown. gst_macos_main
// starts that loop on the main thread and runs our code on a secondary thread.

/*
#cgo pkg-config: gstreamer-1.0
#include <gst/gst.h>

extern int goMacosMain(int argc, char **argv, void *userData);
*/
import "C"

import (
	"runtime"
	"unsafe"
)

func init() {
	// Keep main.main on the process's main thread so Cocoa can use it.
	runtime.LockOSThread()
}

var macosMainFn func()

//export goMacosMain
func goMacosMain(argc C.int, argv **C.char, userData unsafe.Pointer) C.int {
	macosMainFn()
	return 0
}

func runMain(fn func()) {
	macosMainFn = fn
	C.gst_macos_main(C.GstMainFunc(C.goMacosMain), 0, nil, nil)
}
