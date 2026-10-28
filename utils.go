package tilemap

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	// FS_IOC_SETFLAGS / FS_IOC_GETFLAGS on 64 bit linux
	setFlags = uintptr(0x40086602)
	getFlags = uintptr(0x80086601)

	NO_COMP uint32 = 0x00000400
	COMP    uint32 = 0x00000004
	NO_COW  uint32 = 0x00800000
)

var (
	sipkey1 uint64 = 0xDEADBEEFFEEDFEBE
	sipkey2 uint64 = 0x0123456789ABCDEF
)

type controlFunc func(fd uintptr) error

func rawFdCall(fio *os.File, cf controlFunc) error {
	if fio == nil || cf == nil {
		return errors.New("invalid parameters")
	}
	rc, err := fio.SyscallConn()
	if err != nil {
		return err
	}
	if cerr := rc.Control(func(fd uintptr) {
		err = cf(fd)
	}); cerr != nil {
		return cerr
	}
	return err
}

// safeFallocate grows the file from oldsz to newsz, preferring fallocate and
// falling back to writing zeros on filesystems that do not support it.
func safeFallocate(fio *os.File, oldsz, newsz int64) error {
	if newsz <= oldsz {
		return nil
	}
	return rawFdCall(fio, func(fd uintptr) error {
		err := syscall.Fallocate(int(fd), 0, oldsz, newsz-oldsz)
		if err == nil {
			return nil
		}
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
			return slowFileAllocate(fio, oldsz, newsz)
		}
		return fmt.Errorf("failed to perform fallocate: %w", err)
	})
}

func slowFileAllocate(fio *os.File, oldsz, newsz int64) error {
	if newsz <= oldsz {
		return nil
	}
	buff := make([]byte, 64*1024)
	toWrite := newsz - oldsz
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

// setNoCOW marks the file as no-copy-on-write (btrfs and friends) so that the
// memory mapped index does not fragment.  It is best effort: unsupported
// filesystems are ignored and existing attributes are preserved.
func setNoCOW(f *os.File) error {
	attr, err := getAttr(f)
	if err != nil {
		return err
	} else if attr&NO_COW != 0 {
		return nil
	}
	return setAttr(f, attr|NO_COW)
}

func setAttr(f *os.File, attr uint32) error {
	return rawFdCall(f, func(fd uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, setFlags, uintptr(unsafe.Pointer(&attr))); errno != 0 {
			return os.NewSyscallError("IOCTL", errno)
		}
		return nil
	})
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
