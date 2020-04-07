package tilemap

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	setFlags        = uintptr(0x40086602)
	getFlags        = uintptr(0x40086601)
	NO_COMP  uint32 = 0x00000400
	COMP     uint32 = 0x00000004
	NO_COW   uint32 = 0x00800000
)

var (
	sipkey1 uint64 = 0xDEADBEEFFEEDFEBE
	sipkey2 uint64 = 0x0123456789ABCDEF
)

type es struct{}

type controlFunc func(fd uintptr) error

func rawFdCall(fio *os.File, cf controlFunc) error {
	if fio == nil || cf == nil {
		return errors.New("invalid parameters")
	}
	rc, err := fio.SyscallConn()
	if err != nil {
		return err
	}
	rc.Control(func(fd uintptr) {
		err = cf(fd)
	})
	return err
}

func safeFallocate(fio *os.File, oldsz, newsz int64) (err error) {
	//attempt to use syscall.Fallocate
	err = rawFdCall(fio, func(fd uintptr) (lerr error) {
		if errno := syscall.Fallocate(int(fd), 0, oldsz, newsz); errno != nil {
			if errno == syscall.ENOTSUP || errno == syscall.EOPNOTSUPP {
				lerr = slowFileAllocate(fio, oldsz, newsz)
			} else {
				lerr = fmt.Errorf("Failed to perform fallocate: %v", errno)
			}
		}
		return
	})
	return
}

func slowFileAllocate(fio *os.File, oldsz, newsz int64) error {
	if newsz <= oldsz {
		return nil
	}
	buffsize := 64 * 1024
	toWrite := newsz - oldsz
	buff := make([]byte, buffsize)
	ptr := oldsz
	for toWrite > 0 {
		if toWrite < int64(len(buff)) {
			buff = buff[0:toWrite]
		}
		n, err := fio.WriteAt(buff, ptr)
		if err != nil {
			return err
		}
		ptr += int64(n)
		toWrite -= int64(n)
	}
	return nil
}

func setAttr(f *os.File, attr uint32) (err error) {
	err = rawFdCall(f, func(fd uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, setFlags, uintptr(unsafe.Pointer(&attr))); errno != 0 {
			return os.NewSyscallError("IOCTL", errno)
		}
		return nil
	})
	return
}

func getAttr(f *os.File) (attr uint32, err error) {
	err = rawFdCall(f, func(fd uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, getFlags, uintptr(unsafe.Pointer(&attr))); errno != 0 {
			return os.NewSyscallError("IOCTL", errno)
		}
		return nil
	})
	return
}
