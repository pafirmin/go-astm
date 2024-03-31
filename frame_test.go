package astm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatFrame(t *testing.T) {
	type args struct {
		num     int
		b       []byte
		partial bool
	}
	tests := []struct {
		name string
		args args
		want []byte
	}{
		{
			name: "complete",
			args: args{
				b:       []byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F|||20050619094203|20050619101521"),
				partial: false,
			},
			want: []byte("\x020R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F|||20050619094203|20050619101521\x0354\x0D\x0A"),
		},
		{
			name: "partial",
			args: args{
				num:     1,
				b:       []byte("R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F|||20050619094203"),
				partial: true,
			},
			want: []byte("\x021R|1|^^^10^^0|0.310|ulU/ml|0.270^4.20|N||F|||20050619094203\x172C\x0D\x0A"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatFrame(tt.args.num, tt.args.b, tt.args.partial)
			assert.Equal(t, got, tt.want)
		})
	}
}
