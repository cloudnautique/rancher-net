// +build darwin dragonfly freebsd netbsd openbsd

package raw

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/bpf"
)

const (
	// bpfDIn tells BPF to pass through only incoming packets, so we do not
	// receive the packets we send using BPF.
	bpfDIn = 0

	// osFreeBSD is the GOOS name for FreeBSD.
	osFreeBSD = "freebsd"
)

// bpfLen returns the length of the BPF header prepended to each incoming ethernet
// frame.  FreeBSD uses a slightly modified header from other BSD variants.
func bpfLen() int {
	// Majority of BSD family systems use the bpf_hdr struct, but FreeBSD
	// has replaced this with bpf_xhdr, which is longer.
	const (
		bpfHeaderLen  = 18
		bpfXHeaderLen = 26
	)

	if runtime.GOOS == osFreeBSD {
		return bpfXHeaderLen
	}

	return bpfHeaderLen
}

var (
	// Must implement net.PacketConn at compile-time.
	_ net.PacketConn = &packetConn{}
)

// packetConn is the Linux-specific implementation of net.PacketConn for this
// package.
type packetConn struct {
	proto  Protocol
	ifi    *net.Interface
	f      *os.File
	fd     int
	buflen int
}

// listenPacket creates a net.PacketConn which can be used to send and receive
// data at the device driver level.
//
// ifi specifies the network interface which will be used to send and receive
// data.  proto specifies the protocol which should be captured and
// transmitted.
func listenPacket(ifi *net.Interface, proto Protocol) (*packetConn, error) {
	var f *os.File
	var err error

	// Try to find an available BPF device
	for i := 0; i <= 10; i++ {
		bpfPath := fmt.Sprintf("/dev/bpf%d", i)
		f, err = os.OpenFile(bpfPath, os.O_RDWR, 0666)
		if err == nil {
			// Found a usable device
			break
		}

		// Device is busy, try the next one
		if err == syscall.EBUSY {
			continue
		}

		return nil, err
	}

	if f == nil {
		return nil, errors.New("unable to open BPF device")
	}

	fd := int(f.Fd())
	if fd == -1 {
		return nil, errors.New("unable to open BPF device")
	}

	// Configure BPF device to send and receive data
	buflen, err := configureBPF(fd, ifi, proto)
	if err != nil {
		return nil, err
	}

	return &packetConn{
		proto:  proto,
		ifi:    ifi,
		f:      f,
		fd:     fd,
		buflen: buflen,
	}, nil
}

// ReadFrom implements the net.PacketConn.ReadFrom method.
func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	// Attempt to receive on socket
	buf := make([]byte, p.buflen)
	n, err := syscall.Read(p.fd, buf)
	if err != nil {
		// Return other errors
		return n, nil, err
	}

	// TODO(mdlayher): consider parsing BPF header if it proves useful.
	// BPF header length depends on the platform this code is running on
	bpfl := bpfLen()

	// Retrieve source MAC address of ethernet header
	mac := make(net.HardwareAddr, 6)
	copy(mac, buf[bpfl+6:bpfl+12])

	// Skip past BPF header to retrieve ethernet frame
	out := copy(b, buf[bpfl:bpfl+n])

	return out, &Addr{
		HardwareAddr: mac,
	}, nil
}

// WriteTo implements the net.PacketConn.WriteTo method.
func (p *packetConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return syscall.Write(p.fd, b)
}

// Close closes the connection.
func (p *packetConn) Close() error {
	return p.f.Close()
}

// LocalAddr returns the local network address.
func (p *packetConn) LocalAddr() net.Addr {
	return &Addr{
		HardwareAddr: p.ifi.HardwareAddr,
	}
}

// SetDeadline implements the net.PacketConn.SetDeadline method.
func (p *packetConn) SetDeadline(t time.Time) error {
	return ErrNotImplemented
}

// SetReadDeadline implements the net.PacketConn.SetReadDeadline method.
func (p *packetConn) SetReadDeadline(t time.Time) error {
	return ErrNotImplemented
}

// SetWriteDeadline implements the net.PacketConn.SetWriteDeadline method.
func (p *packetConn) SetWriteDeadline(t time.Time) error {
	return ErrNotImplemented
}

// SetBPF attaches an assembled BPF program to a raw net.PacketConn.
func (p *packetConn) SetBPF(filter []bpf.RawInstruction) error {
	// Base filter filters traffic based on EtherType
	base, err := bpf.Assemble(baseFilter(p.proto))
	if err != nil {
		return err
	}

	// Append user filter to base filter, translate to raw format,
	// and apply to BPF device
	return syscall.SetBpf(p.fd, assembleBpfInsn(append(base, filter...)))
}

// configureBPF configures a BPF device with the specified file descriptor to
// use the specified network and interface and protocol.
func configureBPF(fd int, ifi *net.Interface, proto Protocol) (int, error) {
	// Use specified interface with BPF device
	if err := syscall.SetBpfInterface(fd, ifi.Name); err != nil {
		return 0, err
	}

	// Inform BPF to send us its data immediately
	if err := syscall.SetBpfImmediate(fd, 1); err != nil {
		return 0, err
	}

	// Check buffer size of BPF device
	buflen, err := syscall.BpfBuflen(fd)
	if err != nil {
		return 0, err
	}

	// Do not automatically complete source address in ethernet headers
	if err := syscall.SetBpfHeadercmpl(fd, 0); err != nil {
		return 0, err
	}

	// Only retrieve incoming traffic using BPF device
	if err := setBPFDirection(fd, bpfDIn); err != nil {
		return 0, err
	}

	// Build and apply base BPF filter which checks for correct EtherType
	// on incoming packets
	prog, err := bpf.Assemble(baseInterfaceFilter(proto, ifi.MTU))
	if err != nil {
		return 0, err
	}
	if err := syscall.SetBpf(fd, assembleBpfInsn(prog)); err != nil {
		return 0, err
	}

	// Flush any packets currently in the BPF device's buffer
	if err := syscall.FlushBpf(fd); err != nil {
		return 0, err
	}

	return buflen, nil
}

// setBPFDirection enables filtering traffic traveling in a specific direction
// using BPF, so that traffic sent by this package is not captured when reading
// using this package.
func setBPFDirection(fd int, direction int) error {
	_, _, err := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		// Even though BIOCSDIRECTION is preferred on FreeBSD, BIOCSSEESENT continues
		// to work, and is required for other BSD platforms
		syscall.BIOCSSEESENT,
		uintptr(unsafe.Pointer(&direction)),
	)
	if err != 0 {
		return syscall.Errno(err)
	}

	return nil
}

// assembleBpfInsn assembles a slice of bpf.RawInstructions to the format required by
// package syscall.
func assembleBpfInsn(filter []bpf.RawInstruction) []syscall.BpfInsn {
	// Copy each bpf.RawInstruction into syscall.BpfInsn.  If needed,
	// the structures have the same memory layout and could probably be
	// unsafely cast to each other for speed.
	insns := make([]syscall.BpfInsn, 0, len(filter))
	for _, ins := range filter {
		insns = append(insns, syscall.BpfInsn{
			Code: ins.Op,
			Jt:   ins.Jt,
			Jf:   ins.Jf,
			K:    ins.K,
		})
	}

	return insns
}

// baseInterfaceFilter creates a base BPF filter which filters traffic based
// on its EtherType and returns up to "mtu" bytes of data for processing.
func baseInterfaceFilter(proto Protocol, mtu int) []bpf.Instruction {
	return append(
		// Filter traffic based on EtherType
		baseFilter(proto),
		// Accept the packet bytes up to the interface's MTU
		bpf.RetConstant{
			Val: uint32(mtu),
		},
	)
}

// baseFilter creates a base BPF filter which filters traffic based on its
// EtherType.  baseFilter can be prepended to other filters to handle common
// filtering tasks.
func baseFilter(proto Protocol) []bpf.Instruction {
	// Offset | Length | Comment
	// -------------------------
	//   00   |   06   | Ethernet destination MAC address
	//   06   |   06   | Ethernet source MAC address
	//   12   |   02   | Ethernet EtherType
	const (
		etherTypeOffset = 12
		etherTypeLength = 2
	)

	return []bpf.Instruction{
		// Load EtherType value from Ethernet header
		bpf.LoadAbsolute{
			Off:  etherTypeOffset,
			Size: etherTypeLength,
		},
		// If EtherType is equal to the protocol we are using, jump to instructions
		// added outside of this function.
		bpf.JumpIf{
			Cond:     bpf.JumpEqual,
			Val:      uint32(proto),
			SkipTrue: 1,
		},
		// EtherType does not match our protocol
		bpf.RetConstant{
			Val: 0,
		},
	}
}
