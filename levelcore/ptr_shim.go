package levelcore

import "unsafe"

func unsafePointer(p *tileChunk) unsafe.Pointer { return unsafe.Pointer(p) }
