package astm

import (
	"bytes"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConnListen_receive(t *testing.T) {
	type args struct {
		in [][]byte
	}
	tests := []struct {
		name     string
		args     args
		wantMsg  []byte
		wantSent []byte
	}{
		{
			name: "normal operation",
			args: args{
				in: [][]byte{
					{ENQ},
					FormatFrame(0, []byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"), false),
					FormatFrame(1, []byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192914||||N||||1||||||||||F\r"), false),
					FormatFrame(2, []byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||20050619101521\r"), false),
					FormatFrame(3, []byte("L|1|N\r"), false),
					{EOT},
				},
			},
			wantMsg: slices.Concat(
				[]byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"),
				[]byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192914||||N||||1||||||||||F\r"),
				[]byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||20050619101521\r"),
				[]byte("L|1|N\r"),
			),
			wantSent: []byte{ACK, ACK, ACK, ACK, ACK},
		},
		{
			name: "split records",
			args: args{
				in: [][]byte{
					{ENQ},
					FormatFrame(0, []byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"), false),
					FormatFrame(1, []byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192"), true),
					FormatFrame(2, []byte("914||||N||||1||||||||||F\r"), false),
					FormatFrame(3, []byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||200"), true),
					FormatFrame(4, []byte("50619101521\r"), false),
					FormatFrame(5, []byte("L|1|N\r"), false),
					{EOT},
				},
			},
			wantMsg: slices.Concat(
				[]byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"),
				[]byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192914||||N||||1||||||||||F\r"),
				[]byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||20050619101521\r"),
				[]byte("L|1|N\r"),
			),
			wantSent: []byte{ACK, ACK, ACK, ACK, ACK, ACK, ACK},
		},
		{
			name: "checksum failed",
			args: args{
				in: [][]byte{
					{ENQ},
					FormatFrame(0, []byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"), false),
					[]byte("\x021R|INVALID\x0355\x0D\x0A"),
					FormatFrame(1, []byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192914||||N||||1||||||||||F\r"), false),
					FormatFrame(2, []byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||20050619101521\r"), false),
					FormatFrame(3, []byte("L|1|N\r"), false),
					{EOT},
				},
			},
			wantMsg: slices.Concat(
				[]byte("H|\\^&|||cobas-e411^1|||||host|RSUPL^BATCH|P|1\r"),
				[]byte("O|1|123|193^2^1^^S1^SC|^^^111^1^1|R||20230602192914||||N||||1||||||||||F\r"),
				[]byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F||||20050619101521\r"),
				[]byte("L|1|N\r"),
			),
			wantSent: []byte{ACK, ACK, NAK, ACK, ACK, ACK},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrument, host := net.Pipe()
			conn := Listen(instrument)
			got := bytes.Buffer{}
			sent := bytes.Buffer{}
			var mu sync.Mutex

			go func() {
				for _, b := range tt.args.in {
					_, _ = host.Write(b)
					res := make([]byte, 256)
					n, _ := host.Read(res)
					mu.Lock()
					sent.Write(res[:n])
					mu.Unlock()
				}

				_, _ = got.ReadFrom(host)
			}()

			tx, err := conn.Acknowledge()
			assert.NoError(t, err)
			_, err = got.ReadFrom(tx)
			assert.NoError(t, err)

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tt.wantSent, sent.Bytes())
			assert.Equal(t, tt.wantMsg, got.Bytes())
		})
	}
}

func TestConnListen_send(t *testing.T) {
	type args struct {
		in  []byte
		out [][]byte
	}
	type fields struct {
		timeout time.Duration
	}
	tests := []struct {
		name       string
		fields     fields
		args       args
		wantSent   []byte
		wantEnqErr error
		wantStxErr error
	}{
		{
			name: "normal operation",
			fields: fields{
				timeout: 10 * time.Second,
			},
			args: args{
				in: []byte{ACK, ACK, ACK, ACK},
				out: [][]byte{
					FormatFrame(1, []byte("H|\\^&|||NORM|||||||P\r"), false),
					FormatFrame(2, []byte("O|1|sample1|3^@95^2^^SAMPLE^NORMAL|^^^141^\\^^^137^|R|20230602192914|||||N||||||||||||||Q\r"), false),
					FormatFrame(3, []byte("L|1|N\x0D"), false),
				},
			},
			wantSent: slices.Concat(
				[]byte{ENQ},
				FormatFrame(1, []byte("H|\\^&|||NORM|||||||P\r"), false),
				FormatFrame(2, []byte("O|1|sample1|3^@95^2^^SAMPLE^NORMAL|^^^141^\\^^^137^|R|20230602192914|||||N||||||||||||||Q\r"), false),
				FormatFrame(3, []byte("L|1|N\x0D"), false),
				[]byte{EOT},
			),
		},
		{
			name: "receive NAK",
			fields: fields{
				timeout: 10 * time.Second,
			},
			args: args{
				in: []byte{ACK, NAK},
				out: [][]byte{
					FormatFrame(0, []byte("H|\\^&|||NAK|||||||P\r"), false),
					FormatFrame(1, []byte("O|1|sample1|3^@95^2^^SAMPLE^NORMAL|^^^141^\\^^^137^|R|20230602192914|||||N||||||||||||||Q\r"), false),
					FormatFrame(2, []byte("L|1|N\x0D"), false),
				},
			},
			wantSent: slices.Concat(
				[]byte{ENQ},
				FormatFrame(0, []byte("H|\\^&|||NAK|||||||P\r"), false),
				[]byte{EOT},
			),
			wantStxErr: ErrNotAcknowledged,
		},
		{
			name: "line contention",
			fields: fields{
				timeout: 10 * time.Second,
			},
			args: args{
				in: []byte{ENQ},
				out: [][]byte{
					FormatFrame(0, []byte("H|\\^&|||Host|||||||P\r"), false),
				},
			},
			wantSent:   nil,
			wantEnqErr: ErrLineContention,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instrument, host := net.Pipe()
			conn := Listen(instrument)
			got := bytes.Buffer{}
			var mu sync.Mutex

			go func() {
				mu.Lock()
				defer mu.Unlock()
				i := 0
				for {
					sent := make([]byte, 256)
					n, _ := host.Read(sent)
					_, _ = got.Write(sent[:n])

					if i >= len(tt.args.in) {
						break
					}

					_, _ = host.Write([]byte{tt.args.in[i]})
					i++
				}
			}()

			tx, err := conn.RequestControl()
			assert.ErrorIs(t, err, tt.wantEnqErr)
			if tt.wantEnqErr != nil {
				return
			}
			assert.NotNil(t, tx)
			assert.NoError(t, err)

			for _, frame := range tt.args.out {
				_, err := tx.Write(frame)
				assert.ErrorIs(t, err, tt.wantStxErr)
				if tt.wantStxErr != nil {
					break
				}
			}
			tx.Close()

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tt.wantSent, got.Bytes())
		})
	}
}
