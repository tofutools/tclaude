package web

import (
	"fmt"
	"net/url"
	"os"
	"syscall"

	"github.com/skip2/go-qrcode"
	"golang.org/x/term"
)

// renderQR prints a compact QR code to stdout using Unicode half-block characters.
// Each terminal character represents 2 vertical modules, making the output ~4x smaller.
// Uses \r\n for compatibility with raw terminal mode.
func renderQR(text string) {
	qr, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(os.Stderr, "QR error: %v\r\n", err)
		return
	}

	matrix := qr.Bitmap()
	rows := len(matrix)

	for y := 0; y < rows; y += 2 {
		for x := range matrix[y] {
			top := matrix[y][x]
			bottom := false
			if y+1 < rows {
				bottom = matrix[y+1][x]
			}

			// Use half-block characters: ▀ (upper half) with fg=top color, bg=bottom color
			switch {
			case top && bottom: // both black
				fmt.Print("\033[40m \033[0m")
			case top && !bottom: // top black, bottom white
				fmt.Print("\033[30;47m▀\033[0m")
			case !top && bottom: // top white, bottom black
				fmt.Print("\033[37;40m▀\033[0m")
			default: // both white
				fmt.Print("\033[47m \033[0m")
			}
		}
		fmt.Print("\033[0m\r\n")
	}
}

// connectionURL builds a URL with embedded basic auth credentials,
// choosing the best host address for remote access.
func connectionURL(scheme, bind string, port int, user, pass string) string {
	host := bind
	if host == "0.0.0.0" || host == "" {
		// Prefer a resolvable hostname for human-friendly addresses
		if hostnames := resolvableHostnames(); len(hostnames) > 0 {
			host = hostnames[0]
		} else {
			// Fall back to first non-loopback IP
			for _, ip := range localIPStrings() {
				if ip != "127.0.0.1" {
					host = ip
					break
				}
			}
		}
	}

	u := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(user, pass),
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   "/",
	}
	return u.String()
}

// printQR prints a QR code with the connection URL below it.
func printQR(connURL string) {
	fmt.Print("\r\n")
	renderQR(connURL)
	fmt.Printf("  %s\r\n\r\n", connURL)
}

// startKeypressListener enters raw terminal mode and listens for keypresses.
// Space shows the QR code, q/Ctrl+C triggers shutdown.
// Returns a cleanup function that restores the terminal.
func startKeypressListener(showQR func()) func() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}

	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			switch buf[0] {
			case ' ':
				showQR()
			case 'q', 0x03: // q or Ctrl+C
				syscall.Kill(syscall.Getpid(), syscall.SIGINT)
				return
			}
		}
	}()

	return func() {
		term.Restore(fd, oldState)
	}
}
