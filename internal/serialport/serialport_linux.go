//go:build linux

package serialport

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// baudRates maps a numeric line rate to its termios constant. GQ-RFC1201
// "Serial Port configuration" lists these as the rates a GMC-320 accepts.
var baudRates = map[int]uint32{
	1200:   unix.B1200,
	2400:   unix.B2400,
	4800:   unix.B4800,
	9600:   unix.B9600,
	19200:  unix.B19200,
	38400:  unix.B38400,
	57600:  unix.B57600,
	115200: unix.B115200,
}

// devicePort is a serial port backed by a real character device.
type devicePort struct {
	mu          sync.Mutex
	fd          int
	closed      bool
	readTimeout time.Duration
}

// Open configures the named device for raw 8N1 communication at cfg.Baud and
// returns a Port.
//
// The file descriptor is left in non-blocking mode and reads are gated with
// poll(2). That gives an exact read deadline without the VMIN/VTIME decisecond
// granularity that termios alone would impose.
func Open(cfg Config) (Port, error) {
	speed, ok := baudRates[cfg.Baud]
	if !ok {
		return nil, fmt.Errorf("serialport: unsupported baud rate %d", cfg.Baud)
	}
	if cfg.ReadTimeout <= 0 {
		return nil, fmt.Errorf("serialport: read timeout must be positive, got %s", cfg.ReadTimeout)
	}

	fd, err := unix.Open(cfg.Path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("serialport: open %s: %w", cfg.Path, err)
	}

	// Raw 8N1, no flow control, ignore modem control lines, enable receiver.
	t := unix.Termios{
		Cflag:  unix.CS8 | unix.CREAD | unix.CLOCAL | speed,
		Ispeed: speed,
		Ospeed: speed,
	}
	// VMIN=0/VTIME=0 makes read(2) return immediately with whatever is
	// available; poll(2) supplies the actual deadline.
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &t); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("serialport: configure %s: %w", cfg.Path, err)
	}

	p := &devicePort{fd: fd, readTimeout: cfg.ReadTimeout}
	if err := p.Drain(); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// Read blocks until at least one byte is available, the read timeout elapses,
// or an error occurs. A timeout is reported as ErrTimeout, never as (0, nil),
// so callers cannot spin on it.
//
// Like any serial read, a successful call may return fewer bytes than
// requested. That is expected behaviour on CH340/CH341 bridges and is the
// caller's responsibility to accumulate.
func (p *devicePort) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	deadline := time.Now().Add(p.readTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, ErrTimeout
		}

		fd, err := p.descriptor()
		if err != nil {
			return 0, err
		}

		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining.Milliseconds())+1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return 0, fmt.Errorf("serialport: poll: %w", err)
		}
		if n == 0 {
			return 0, ErrTimeout
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return 0, fmt.Errorf("serialport: device error (revents=0x%x), it may have been unplugged", fds[0].Revents)
		}
		if fds[0].Revents&unix.POLLHUP != 0 && fds[0].Revents&unix.POLLIN == 0 {
			return 0, fmt.Errorf("serialport: device hung up, it may have been unplugged")
		}

		got, err := unix.Read(fd, b)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return 0, fmt.Errorf("serialport: read: %w", err)
		}
		if got == 0 {
			// End of file on a tty means the far side disappeared.
			return 0, fmt.Errorf("serialport: unexpected EOF, device may have been unplugged")
		}
		return got, nil
	}
}

// Write sends the whole buffer, retrying on partial writes.
func (p *devicePort) Write(b []byte) (int, error) {
	written := 0
	deadline := time.Now().Add(p.readTimeout)

	for written < len(b) {
		if time.Now().After(deadline) {
			return written, fmt.Errorf("serialport: write timed out after %d of %d bytes", written, len(b))
		}

		fd, err := p.descriptor()
		if err != nil {
			return written, err
		}

		n, err := unix.Write(fd, b[written:])
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if err == unix.EAGAIN {
				fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
				if _, perr := unix.Poll(fds, 100); perr != nil && perr != unix.EINTR {
					return written, fmt.Errorf("serialport: poll for write: %w", perr)
				}
				continue
			}
			return written, fmt.Errorf("serialport: write: %w", err)
		}
		written += n
	}
	return written, nil
}

// Drain discards anything sitting in the kernel's input queue.
func (p *devicePort) Drain() error {
	fd, err := p.descriptor()
	if err != nil {
		return err
	}
	if err := unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH); err != nil {
		return fmt.Errorf("serialport: flush input: %w", err)
	}
	return nil
}

// Close releases the file descriptor. It is safe to call more than once.
func (p *devicePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	if err := unix.Close(p.fd); err != nil {
		return fmt.Errorf("serialport: close: %w", err)
	}
	return nil
}

// descriptor returns the file descriptor, or an error if the port was closed.
func (p *devicePort) descriptor() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, fmt.Errorf("serialport: port is closed")
	}
	return p.fd, nil
}
