package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenHiveProvenance_ParseSSEUsageIntString(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		want   int
		wantOK bool
	}{
		{name: "integer", value: "42", want: 42, wantOK: true},
		{name: "surrounding whitespace", value: " 17\t", want: 17, wantOK: true},
		{name: "negative integer", value: "-3", want: -3, wantOK: true},
		{name: "fraction is rejected", value: "1.5"},
		{name: "non numeric is rejected", value: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSSEUsageInt(tt.value)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}
