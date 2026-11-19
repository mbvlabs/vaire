package cli

import (
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/node"
)

func TestExposurePickerOnlyOffersEnrolledFactors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status node.ExposureFactorStatus
		want   []string
	}{
		{"none", node.ExposureFactorStatus{}, nil},
		{"password", node.ExposureFactorStatus{Password: true}, []string{"password"}},
		{"authenticator", node.ExposureFactorStatus{TOTP: true}, []string{"totp"}},
		{"both", node.ExposureFactorStatus{Password: true, TOTP: true}, []string{"password", "totp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string

			for _, choice := range exposureChoices(&tc.status) {
				got = append(got, choice[1])
			}

			if !slices.Equal(got, tc.want) {
				t.Fatalf("choices = %v, want %v", got, tc.want)
			}
		})
	}
}
