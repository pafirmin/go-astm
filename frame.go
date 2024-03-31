package astm

import (
	"fmt"
)

const (
	ACK = '\x06' // request acknowledged
	CR  = '\x0D' // carriage return
	ENQ = '\x05' // session initialisation request
	EOT = '\x04' // end of transfer
	ETB = '\x17' // end of partial frame
	ETX = '\x03' // end of frame
	LF  = '\x0A' // linefeed
	NAK = '\x15' // request rejected
	STX = '\x02' // start of text
)

// FormatFrame returns a new ASTM frame with frame number n and record r. Partial
// frames are terminated with ETB and full frames with ETX. NewFrame calculates
// the new frame's checksum and appends this to the frame along with a new line.
func FormatFrame(n int, r []byte, partial bool) []byte {
	var term byte
	if partial {
		term = ETB
	} else {
		term = ETX
	}
	b := make([]byte, 0, len(r)+7)
	b = fmt.Appendf(b, "%c%d%s%c", STX, n, r, term)

	cs := calcChecksum(b)
	b = append(b, cs[0], cs[1], CR, LF)

	return b
}

// calcChecksum calculates the frame's checksum
func calcChecksum(b []byte) []byte {
	var sum uint8

	//take each byte in the string and add the values
	for _, b := range b {
		if b == STX {
			continue
		}

		sum += b

		if b == ETX || b == ETB {
			break
		}
	}

	return []byte(fmt.Sprintf("%02X", sum))
}
