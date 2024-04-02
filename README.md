## Package ASTM

This is a basic implentation of an ASTM host interface, simplifying the creation of of bi-directional
interfaces between ASTM-compliant analysers and a LIMS/LIS by abstracting the protocol behind
familiar interfaces.

Because `Listen()` wraps any `io.ReadWriteCloser`, the library works with serial or TCP interfaces.

Full example (using [go-serial](https://github.com/bugst/go-serial) package):
```go
func main() {
	baud := 9600
	parity := 0
	stopBit := 2
	dataBit := 8

	mode := &serial.Mode{
		BaudRate: baud,
		StopBits: serial.StopBits(stopBit),
		Parity:   serial.Parity(parity),
		DataBits: dataBit,
	}

	p, err := serial.Open("/dev/ttyS0", mode)
	if err != nil {
		log.Fatal(err)
	}

	conn := astm.Listen(p)
	defer conn.Close()

	for {
		tx, err := conn.Acknowledge()
		if err != nil {
			fmt.Println(err)
			return
		}

		for {
			buf := make([]byte, 248)
			n, err := tx.Read(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
			fmt.Println(string(buf[:n]))
		}
	}
}

// Example output:
// H|\^&||||||||||P||
// O|1|MySample|36^0044^2^^SAMPLE^NORMAL|ALL|R|20050705093416|||||X||||||||||||||O
// R|1|^^^245^^0|22.50|pmol/l|18.94^27.26|N||F|||20230602184928|20230602190748|
// R|3|^^^182^^0|8.71|uIU/ml|7.78^10.52|N||F|||20230602184804|20230602190624|
// L|1|
```

## Bi-directional communication

`RequestControl()` tries to avoid line contention. If called while in active transfer, it will block
until the end of the transfer. If the analyzer responds to an ENQ with ENQ, it will return with
`astm.ErrLineContention`. That said, how this is used will depend on the behaviour of each
analyzer and especially on how rapidly it sends succesive Inquiries. It may be necessary to implement
some sort of queue.

```go
// ...
tx, err := conn.RequestControl()
if err != nil {
    // ...
}

myFrames := [][]byte{/* ... */}

for _, f := range myFrames {
    _, err := tx.Write(f)
    if err != nil {
        switch {
        case errors.Is(err, astm.ErrNotAcknowledged):
        //...
        default:
        //...
        }
    }
    //...
}

tx.End()
// ...
```

