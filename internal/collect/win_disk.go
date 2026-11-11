//go:build windows && (amd64 || arm64)

package collect

// Disk: space of the system volume every ~10 s, and once (from Info) the model and bus of
// the physical disk under it, through two IOCTLs on handles opened with no access rights.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"btophub/internal/proto"
)

const (
	winIoctlVolumeGetVolumeDiskExtents = 0x00560000 // CTL_CODE(IOCTL_VOLUME_BASE, 0, METHOD_BUFFERED, FILE_ANY_ACCESS)
	winIoctlStorageQueryProperty       = 0x002D1400 // CTL_CODE(IOCTL_STORAGE_BASE, 0x500, METHOD_BUFFERED, FILE_ANY_ACCESS)
)

// winSystemDrive returns the system drive as "C:".
func winSystemDrive() string {
	if d := winDriveLetter(os.Getenv("SystemDrive")); d != "" {
		return d
	}
	if winHave(winModKernel32, "GetSystemWindowsDirectoryW") != nil {
		return ""
	}
	if w, err := windows.GetSystemWindowsDirectory(); err == nil {
		return winDriveLetter(w)
	}
	return ""
}

func (c *winCollector) initDisk() {
	c.ioBuf = make([]byte, 4096)
	c.haveIoctl = winHave(winModKernel32, "CreateFileW", "DeviceIoControl", "CloseHandle") == nil
	if err := winHave(winModKernel32, "GetDiskFreeSpaceExW"); err != nil {
		c.setSource("disk_space", "none: "+err.Error())
	} else {
		c.haveDiskFree = true
	}
	d := winSystemDrive()
	if d == "" {
		c.setSource("disk_space", "none: cannot tell the system drive (%SystemDrive% and GetSystemWindowsDirectory)")
		return
	}
	root, err := windows.UTF16PtrFromString(d + `\`)
	if err != nil {
		c.setSource("disk_space", "none: "+err.Error())
		return
	}
	c.sysDrive, c.sysRoot = d, root
	c.info.Disk.Label = d
	if c.haveDiskFree {
		c.setSource("disk_space", "GetDiskFreeSpaceExW("+d+`\`+"), every 10 s")
	}
}

// readDiskSpace refreshes the cached system volume size (called every ~10 s from Collect).
func (c *winCollector) readDiskSpace(now time.Time) {
	c.diskSpaceAt = now
	if c.sysRoot == nil || !c.haveDiskFree {
		return
	}
	c.dsAvail, c.dsTotal, c.dsFree = 0, 0, 0
	if err := windows.GetDiskFreeSpaceEx(c.sysRoot, &c.dsAvail, &c.dsTotal, &c.dsFree); err != nil {
		c.diskOK = false
		c.setSource("disk_space", "none: GetDiskFreeSpaceExW("+c.sysDrive+`\`+"): "+err.Error())
		return
	}
	if c.dsTotal == 0 || c.dsFree > c.dsTotal {
		c.diskOK = false
		c.setSource("disk_space", fmt.Sprintf("none: GetDiskFreeSpaceExW returned total %d, free %d", c.dsTotal, c.dsFree))
		return
	}
	c.diskOK = true
	c.diskUsed, c.diskTotal = winI64(c.dsTotal-c.dsFree), winI64(c.dsTotal)
	c.setSource("disk_space", "GetDiskFreeSpaceExW("+c.sysDrive+`\`+"), every 10 s")
}

// refreshDiskModel reads model and bus once (up to three tries), from Info() only.
func (c *winCollector) refreshDiskModel() {
	if c.sysDrive == "" || !c.haveIoctl {
		c.diskModelDone = true
		return
	}
	c.diskModelTries++
	model, bus, raw, disk, err := c.diskModelBus()
	if err != nil {
		c.note("disk_model", "disk: model/bus unavailable: "+err.Error())
		return
	}
	c.diskModelDone = true
	c.info.Disk.Model = winTrunc(model, proto.MaxStringLen)
	c.info.Disk.Bus = bus
	c.note("disk_model", fmt.Sprintf("disk: %s is on PhysicalDrive%d: model %q, STORAGE_BUS_TYPE 0x%02X (%q)", c.sysDrive, disk, model, raw, bus))
}

// diskModelBus maps the system volume to its first physical disk
// (IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS) and reads that disk's STORAGE_DEVICE_DESCRIPTOR
// (IOCTL_STORAGE_QUERY_PROPERTY, StorageDeviceProperty, PropertyStandardQuery). Both
// handles are opened with desired access 0, which needs no privileges.
func (c *winCollector) diskModelBus() (model, bus string, raw, disk uint32, err error) {
	vol := `\\.\` + c.sysDrive
	if err = c.ioctl(vol, winIoctlVolumeGetVolumeDiskExtents, false); err != nil {
		return "", "", 0, 0, fmt.Errorf("%s: IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS: %w", vol, err)
	}
	// VOLUME_DISK_EXTENTS: NumberOfDiskExtents uint32 @0, then DISK_EXTENT[] @8 with
	// DiskNumber uint32 @0, StartingOffset @8, ExtentLength @16 (24 bytes each).
	if c.ioRet < 32 {
		return "", "", 0, 0, fmt.Errorf("%s: disk extents reply is %d bytes", vol, c.ioRet)
	}
	n := binary.LittleEndian.Uint32(c.ioBuf[0:])
	disk = binary.LittleEndian.Uint32(c.ioBuf[8:])
	if n == 0 {
		return "", "", 0, 0, fmt.Errorf("%s has no disk extents", vol)
	}
	if n > 1 {
		c.note("disk_extents", fmt.Sprintf("disk: %s spans %d disk extents; the model is the first extent's disk", c.sysDrive, n))
	}
	pd := fmt.Sprintf(`\\.\PhysicalDrive%d`, disk)
	if err = c.ioctl(pd, winIoctlStorageQueryProperty, true); err != nil {
		return "", "", 0, disk, fmt.Errorf("%s: IOCTL_STORAGE_QUERY_PROPERTY: %w", pd, err)
	}
	vendor, product, raw, err := winParseStorageDescriptor(c.ioBuf[:c.ioRet])
	if err != nil {
		return "", "", 0, disk, fmt.Errorf("%s: %w", pd, err)
	}
	return winDiskModel(vendor, product), winBusName(raw), raw, disk, nil
}

// ioctl opens path with no access rights and sends code; the reply lands in c.ioBuf with
// c.ioRet bytes. With query set, the input is a zeroed STORAGE_PROPERTY_QUERY (12 bytes:
// PropertyId StorageDeviceProperty = 0, QueryType PropertyStandardQuery = 0).
func (c *winCollector) ioctl(path string, code uint32, query bool) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("CreateFile: %w", err)
	}
	defer windows.CloseHandle(h)
	c.ioRet = 0
	var in *byte
	var inLen uint32
	if query {
		c.ioQuery = [12]byte{}
		in, inLen = &c.ioQuery[0], uint32(len(c.ioQuery))
	}
	if err := windows.DeviceIoControl(h, code, in, inLen, &c.ioBuf[0], uint32(len(c.ioBuf)), &c.ioRet, nil); err != nil {
		return err
	}
	if c.ioRet > uint32(len(c.ioBuf)) {
		return errors.New("reply larger than the buffer")
	}
	return nil
}
