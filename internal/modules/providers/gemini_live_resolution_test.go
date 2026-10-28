package providers

import "testing"

// TestResolutionCheckRequiresBothAxesToGrow pins the live success rule offline.
// A larger image must be verified on both axes against the reported original.
func TestResolutionCheckRequiresBothAxesToGrow(t *testing.T) {
	cases := []struct {
		name                 string
		metaW, metaH, finalW int
		finalH               int
		decodeOK             bool
		want                 string
	}{
		{name: "both axes grew", metaW: 1024, metaH: 1024, finalW: 2048, finalH: 2048, decodeOK: true, want: resolutionIncreased},
		{name: "only width grew", metaW: 1024, metaH: 1024, finalW: 2048, finalH: 1024, decodeOK: true, want: resolutionNotIncreased},
		{name: "same size", metaW: 1024, metaH: 1024, finalW: 1024, finalH: 1024, decodeOK: true, want: resolutionNotIncreased},
		{name: "smaller", metaW: 1024, metaH: 1024, finalW: 512, finalH: 512, decodeOK: true, want: resolutionNotIncreased},
		{name: "original width unknown", metaW: 0, metaH: 1024, finalW: 2048, finalH: 2048, decodeOK: true, want: resolutionUnverified},
		{name: "original height unknown", metaW: 1024, metaH: 0, finalW: 2048, finalH: 2048, decodeOK: true, want: resolutionUnverified},
		{name: "final not decoded", metaW: 1024, metaH: 1024, finalW: 0, finalH: 0, decodeOK: false, want: resolutionUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolutionCheck(tc.metaW, tc.metaH, tc.finalW, tc.finalH, tc.decodeOK); got != tc.want {
				t.Fatalf("resolutionCheck = %s, want %s", got, tc.want)
			}
		})
	}
}
