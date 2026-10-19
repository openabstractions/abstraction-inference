package modelpack

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// borrowBase takes a loan on a proven base section and maps it read-only
// for this process: the loan holds the section until done is called, so
// the base cannot be freed while a fill decodes against it. The bytes are
// the section's, proved against its digest when it was filled.
func borrowBase(s *modelfiles.Section) (ProvenBase, func(), error) {
	loan, err := s.Lend()
	if err != nil {
		return ProvenBase{}, nil, fmt.Errorf("modelpack: the base section lends nothing: %w", err)
	}
	addr, err := windows.MapViewOfFile(windows.Handle(loan.Handle()), windows.FILE_MAP_READ, 0, 0, uintptr(s.Length()))
	if err != nil {
		loan.End()
		return ProvenBase{}, nil, fmt.Errorf("modelpack: mapping the base section: %w", err)
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	view := unsafe.Slice((*byte)(p), s.Length())
	done := func() {
		//unchecked: unmapping this process's own read-only view; the loan's end follows either way
		windows.UnmapViewOfFile(addr)
		loan.End()
	}
	return ProvenBase{digest: s.Digest(), b: view}, done, nil
}
