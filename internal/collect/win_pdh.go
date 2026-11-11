//go:build windows && (amd64 || arm64)

package collect

// Minimal PDH (pdh.dll) binding: one real-time query per winPdhQuery, counters added by
// their English path, values formatted as doubles. Every call goes through
// syscall.SyscallN with the uintptr(unsafe.Pointer(...)) conversion inline in the call, and
// all output buffers that outlive a call are heap memory owned by the query or collector.

import (
	"errors"
	"fmt"
	"math"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winModPdh = windows.NewLazySystemDLL("pdh.dll")

	winProcPdhOpenQueryW                = winModPdh.NewProc("PdhOpenQueryW")
	winProcPdhAddEnglishCounterW        = winModPdh.NewProc("PdhAddEnglishCounterW")
	winProcPdhRemoveCounter             = winModPdh.NewProc("PdhRemoveCounter")
	winProcPdhCollectQueryData          = winModPdh.NewProc("PdhCollectQueryData")
	winProcPdhGetFormattedCounterValue  = winModPdh.NewProc("PdhGetFormattedCounterValue")
	winProcPdhGetFormattedCounterArrayW = winModPdh.NewProc("PdhGetFormattedCounterArrayW")
	winProcPdhCloseQuery                = winModPdh.NewProc("PdhCloseQuery")
)

const winPdhFormat = 0x00000200 | 0x00008000 // PDH_FMT_DOUBLE | PDH_FMT_NOCAP100

// winPdhValue is PDH_FMT_COUNTERVALUE read through the doubleValue member of its union.
type winPdhValue struct {
	CStatus uint32
	_       uint32
	Value   float64
}

// winPdhItem is PDH_FMT_COUNTERVALUE_ITEM_W. Name is the address of a NUL-terminated
// UTF-16 string inside the same buffer; it is kept as a plain address (never dereferenced
// as a Go pointer) and resolved with winPdhName.
type winPdhItem struct {
	Name     uintptr
	FmtValue winPdhValue
}

// winPdhError is a PDH_STATUS or PDH_CSTATUS code.
type winPdhError uint32

func (e winPdhError) Error() string {
	if e != 0 && e < 0x10000 { // some PDH calls return plain Win32 errors
		return syscall.Errno(e).Error()
	}
	return winPdhStatus(uint32(e))
}

// winPdhNotReadyErr reports whether err only means "no valid value yet".
func winPdhNotReadyErr(err error) bool {
	var e winPdhError
	return errors.As(err, &e) && winPdhNotReady(uint32(e))
}

// winPdhQuery is one real-time PDH query.
type winPdhQuery struct {
	h   uintptr
	val winPdhValue // PdhGetFormattedCounterValue output, heap memory
}

func winPdhLoad() error {
	for _, p := range []*windows.LazyProc{winProcPdhOpenQueryW, winProcPdhAddEnglishCounterW,
		winProcPdhRemoveCounter, winProcPdhCollectQueryData, winProcPdhGetFormattedCounterValue,
		winProcPdhGetFormattedCounterArrayW, winProcPdhCloseQuery} {
		if err := p.Find(); err != nil {
			return err
		}
	}
	return nil
}

// winPdhOpen opens a real-time query (PdhOpenQueryW with a NULL data source).
func winPdhOpen() (*winPdhQuery, error) {
	if err := winPdhLoad(); err != nil {
		return nil, fmt.Errorf("pdh.dll: %w", err)
	}
	q := &winPdhQuery{}
	r, _, _ := syscall.SyscallN(winProcPdhOpenQueryW.Addr(), 0, 0, uintptr(unsafe.Pointer(&q.h)))
	if uint32(r) != 0 {
		return nil, fmt.Errorf("PdhOpenQueryW: %w", winPdhError(uint32(r)))
	}
	if q.h == 0 {
		return nil, errors.New("PdhOpenQueryW returned a null query")
	}
	return q, nil
}

// add adds a counter by its English path (PdhAddEnglishCounterW), so paths work on every
// display language. A wildcard instance "(*)" follows instances as they come and go.
func (q *winPdhQuery) add(path string) (uintptr, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var h uintptr
	r, _, _ := syscall.SyscallN(winProcPdhAddEnglishCounterW.Addr(), q.h, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&h)))
	if uint32(r) != 0 {
		return 0, winPdhError(uint32(r))
	}
	if h == 0 {
		return 0, errors.New("PdhAddEnglishCounterW returned a null counter")
	}
	return h, nil
}

// remove drops a counter that turned out to be useless, so collections stop paying for it.
func (q *winPdhQuery) remove(counter uintptr) {
	if counter != 0 {
		syscall.SyscallN(winProcPdhRemoveCounter.Addr(), counter)
	}
}

// collect samples every counter of the query (PdhCollectQueryData).
func (q *winPdhQuery) collect() error {
	r, _, _ := syscall.SyscallN(winProcPdhCollectQueryData.Addr(), q.h)
	if uint32(r) != 0 {
		return winPdhError(uint32(r))
	}
	return nil
}

// value formats a single-instance counter from the last collection. Rate counters have
// no valid value until the query was collected twice.
func (q *winPdhQuery) value(counter uintptr) (float64, error) {
	q.val = winPdhValue{}
	r, _, _ := syscall.SyscallN(winProcPdhGetFormattedCounterValue.Addr(), counter, winPdhFormat, 0, uintptr(unsafe.Pointer(&q.val)))
	if uint32(r) != 0 {
		return 0, winPdhError(uint32(r))
	}
	if s := q.val.CStatus; s != 0 && s != 1 { // PDH_CSTATUS_VALID_DATA, PDH_CSTATUS_NEW_DATA
		return 0, winPdhError(s)
	}
	if v := q.val.Value; !math.IsNaN(v) && !math.IsInf(v, 0) {
		return v, nil
	}
	return 0, errors.New("PDH returned a non-finite value")
}

// array formats every instance of a wildcard counter (PdhGetFormattedCounterArrayW) into
// *buf, which the caller keeps between calls and which grows when PDH answers
// PDH_MORE_DATA. The items and their names live in *buf until the next call with it.
func (q *winPdhQuery) array(counter uintptr, buf *[]byte) ([]winPdhItem, error) {
	if len(*buf) < 4096 {
		*buf = make([]byte, 4096)
	}
	for attempt := 0; attempt < 4; attempt++ {
		b := *buf
		size, count := uint32(len(b)), uint32(0)
		r, _, _ := syscall.SyscallN(winProcPdhGetFormattedCounterArrayW.Addr(), counter, winPdhFormat,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&b[0])))
		switch uint32(r) {
		case 0:
			if uint64(count)*uint64(unsafe.Sizeof(winPdhItem{})) > uint64(len(b)) {
				return nil, fmt.Errorf("PDH reported %d items in a %d-byte buffer", count, len(b))
			}
			if count == 0 {
				return nil, nil
			}
			return unsafe.Slice((*winPdhItem)(unsafe.Pointer(&b[0])), count), nil
		case winPdhMoreData:
			// The size returned for a too-small non-zero buffer is not reliable (pdh.h
			// docs), so ask properly: size 0 and no buffer.
			need, n := uint32(0), uint32(0)
			r2, _, _ := syscall.SyscallN(winProcPdhGetFormattedCounterArrayW.Addr(), counter, winPdhFormat,
				uintptr(unsafe.Pointer(&need)), uintptr(unsafe.Pointer(&n)), 0)
			if uint32(r2) != winPdhMoreData && uint32(r2) != 0 {
				return nil, winPdhError(uint32(r2))
			}
			grow := uint64(need) + uint64(need)/4 // headroom: instances come and go every second
			if grow < 2*uint64(len(b)) {
				grow = 2 * uint64(len(b))
			}
			if grow > 64<<20 {
				return nil, fmt.Errorf("PDH wants a %d-byte buffer", need)
			}
			*buf = make([]byte, grow)
		default:
			return nil, winPdhError(uint32(r))
		}
	}
	return nil, errors.New("PDH buffer size kept changing")
}

// winPdhName returns an item's instance name as a view into buf (no copy), or nil when
// PDH's address is not inside buf or the string is not terminated there.
func winPdhName(buf []byte, it *winPdhItem) []uint16 {
	if len(buf) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	if it.Name < base || it.Name-base >= uintptr(len(buf)) || (it.Name-base)%2 != 0 {
		return nil
	}
	off := int(it.Name - base)
	s := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), (len(buf)-off)/2)
	for i, c := range s {
		if c == 0 {
			return s[:i]
		}
	}
	return nil
}

// close closes the query and every counter in it.
func (q *winPdhQuery) close() {
	if q.h != 0 {
		syscall.SyscallN(winProcPdhCloseQuery.Addr(), q.h)
		q.h = 0
	}
}
