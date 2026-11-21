// Command ledctl is a hardware triage tool: it sends one LED frame straight
// to the CH340 port through the exact same internal/led code path the daemon
// uses (paced byte-by-byte writes), bypassing config, HTTP and the screen
// loop. A strip that stays lit then pins the fault to one of two places:
// ledctl works but the daemon does not -> the daemon opened the wrong port
// (check its log for "continuing without LED control"); ledctl itself fails
// -> the port path, permissions or the CH340 itself are wrong.
//
// Stop the daemon first (docker compose stop / systemctl stop s1panel) so
// nothing else holds the serial port, then run on the Linux host:
//
//	ledctl list                              # serial ports + VID/PID (CH340 = 1a86:7523)
//	ledctl off                               # FA 04 05 05 08 - strip must go dark
//	ledctl -port /dev/ttyUSB1 rainbow -i 3 -s 3
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"go.bug.st/serial/enumerator"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/led"
)

func main() {
	port := flag.String("port", "/dev/ttyUSB0", "CH340 serial port")
	intensity := flag.Int("i", 3, "intensity, config scale (1 = dim .. 5 = bright)")
	speed := flag.Int("s", 3, "speed, config scale (1 = slow .. 5 = fast)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: ledctl [-port DEV] [-i N] [-s N] list|off|rainbow|breathing|colors|auto|ignore\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	mode := flag.Arg(0)
	if mode == "" {
		flag.Usage()
		os.Exit(2)
	}

	if mode == "list" {
		listPorts()
		return
	}
	if err := send(mode, *port, *intensity, *speed); err != nil {
		fmt.Fprintf(os.Stderr, "ledctl: %v\n", err)
		os.Exit(1)
	}
}

// listPorts prints every serial port with its USB identity and marks the
// CH340 (1a86:7523) the strip hangs off - if it shows up as /dev/ttyUSB1,
// that is the value the daemon config needs too.
func listPorts() {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ledctl: enumerate: %v\n", err)
		os.Exit(1)
	}
	if len(ports) == 0 {
		fmt.Println("no serial ports found (is the panel's USB plug in?)")
		return
	}
	for _, p := range ports {
		line := fmt.Sprintf("%-16s VID=%s PID=%s", p.Name, p.VID, p.PID)
		if strings.EqualFold(p.VID, "1a86") {
			line += "   <- CH340 (LED strip)"
		}
		fmt.Println(line)
	}
}

// send reproduces one daemon action end to end: open at 10000 baud 8N1,
// push the frame paced (writeFrame), close. The printed frame lets you
// cross-check the bytes against doc/revrite.md §4 with an independent
// tool (e.g. a second terminal running `cat /dev/ttyUSB0 | xxd`).
func send(mode, port string, intensity, speed int) error {
	theme, ok := modes[mode]
	if !ok {
		return fmt.Errorf("unknown mode %q (want list|off|rainbow|breathing|colors|auto|ignore)", mode)
	}
	if theme == led.ThemeIgnore {
		fmt.Println("theme 6 (ignore): the protocol sends nothing - the strip keeps whatever mode it has")
		return nil
	}

	d, err := led.Open(port)
	if err != nil {
		return fmt.Errorf("%w (stop the daemon/container first, and check the path against `ledctl list`)", err)
	}
	defer d.Close()

	if theme == led.ThemeOff {
		fmt.Printf("sending % X to %s ...\n", led.OffPacket(), port)
		if err := d.Off(); err != nil {
			return err
		}
	} else {
		fmt.Printf("sending % X to %s ...\n", led.Packet(theme, intensity, speed), port)
		if err := d.Set(theme, intensity, speed); err != nil {
			return err
		}
	}
	fmt.Println("frame sent and drained - did the strip change? if not: wrong port or the CH340/firmware is not answering")
	return nil
}

var modes = map[string]int{
	"off":       led.ThemeOff,
	"rainbow":   led.ThemeRainbow,
	"breathing": led.ThemeBreathing,
	"colors":    led.ThemeColors,
	"auto":      led.ThemeAuto,
	"ignore":    led.ThemeIgnore,
}
