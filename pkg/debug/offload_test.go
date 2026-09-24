package debug

import (
	"testing"
)

func TestSuppressMask(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  []uint32
	}{
		{
			name:  "checksum and segmentation bits are suppressed",
			names: []string{"tx-checksumming", "tx-checksum-ip-generic", "tx-tcp-segmentation", "generic-segmentation-offload", "rx-checksumming", "scatter-gather"},
			want:  []uint32{0b00011111},
		},
		{
			name:  "mangleid and udp segmentation are included",
			names: []string{"tx-tcp-mangleid-segmentation", "tx-udp-segmentation", "tx-checksum-ipv4"},
			want:  []uint32{0b111},
		},
		{
			name: "unrelated features untouched across words",
			names: func() []string {
				n := make([]string, 40)
				for i := range n {
					n[i] = "plain-feature"
				}
				n[32] = "tx-checksum-x"
				return n
			}(),
			want: []uint32{0, 1},
		},
		{
			name:  "empty names",
			names: nil,
			want:  nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suppressMask(tt.names)
			if len(got) != len(tt.want) {
				t.Fatalf("suppressMask() = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("word %d: got %#x want %#x", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func mkSnap(words []featureWord, names []string) *FeatureSnapshot {
	return &FeatureSnapshot{Words: append([]featureWord(nil), words...), Names: names}
}

func TestChangedBits(t *testing.T) {
	tests := []struct {
		name    string
		a, b    *FeatureSnapshot
		wantLen int
		wantHas []string // subset that must appear
	}{
		{
			name:    "identical snapshots have no diffs",
			a:       mkSnap([]featureWord{{Available: 0xf, Requested: 0xf, Active: 0xf}}, nil),
			b:       mkSnap([]featureWord{{Available: 0xf, Requested: 0xf, Active: 0xf}}, nil),
			wantLen: 0,
		},
		{
			name:    "active change within changeable mask is reported by name",
			a:       mkSnap([]featureWord{{Available: 0x3, Requested: 0x3, Active: 0x3}}, []string{"tx-checksumming", "scatter-gather"}),
			b:       mkSnap([]featureWord{{Available: 0x3, Requested: 0x3, Active: 0x2}}, []string{"tx-checksumming", "scatter-gather"}),
			wantLen: 1,
			wantHas: []string{"tx-checksumming"},
		},
		{
			name: "never-changed bits are ignored",
			a:    mkSnap([]featureWord{{Available: 0x1, NeverChange: 0x1, Requested: 0x1, Active: 0x1}}, nil),
			b:    mkSnap([]featureWord{{Available: 0x1, NeverChange: 0x1, Requested: 0x0, Active: 0x0}}, nil),
			// changeable mask is 0: no reported diff even though bits differ
			wantLen: 0,
		},
		{
			name: "requested-only drift is reported (the tx-udp wanted lesson)",
			a:    mkSnap([]featureWord{{Available: 0x1, Requested: 0x1, Active: 0x0}}, []string{"tx-udp-segmentation"}),
			b:    mkSnap([]featureWord{{Available: 0x1, Requested: 0x0, Active: 0x0}}, []string{"tx-udp-segmentation"}),
			// active equal, requested differs — changeable includes bit0
			wantLen: 1,
			wantHas: []string{"tx-udp-segmentation"},
		},
		{
			name: "shorter other snapshot compares the common prefix",
			a:    mkSnap([]featureWord{{Available: 0x1, Active: 0x1}, {Available: 0x1, Active: 0x1}}, nil),
			b:    mkSnap([]featureWord{{Available: 0x1, Active: 0x1}}, nil),
			// word count differs but common prefix equal → no diffs
			wantLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.changedBits(tt.b)
			if len(got) != tt.wantLen {
				t.Fatalf("changedBits() = %v (len %d), want len %d", got, len(got), tt.wantLen)
			}
			for _, want := range tt.wantHas {
				found := false
				for _, g := range got {
					if g == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("changedBits() = %v, want to contain %q", got, want)
				}
			}
		})
	}
}

func TestTrimSnapshotWords(t *testing.T) {
	tests := []struct {
		name  string
		in    []featureWord
		wantN int
	}{
		{"all zero", []featureWord{{}, {}}, 0},
		{"trailing zeros dropped", []featureWord{{Active: 1}, {}, {Requested: 2}}, 3},
		{"no trailing zeros", []featureWord{{Active: 1}}, 1},
		{"nil", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mkSnap(tt.in, nil)
			trimSnapshotWords(s)
			if len(s.Words) != tt.wantN {
				t.Fatalf("len = %d, want %d", len(s.Words), tt.wantN)
			}
		})
	}
}

// TestOffloadRoundtripRoot exercises the ioctl path against a real throwaway
// TAP: snapshot → suppress → restore must return the device bit-identical.
func TestOffloadRoundtripRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := tapIdleSelfCheck(); err != nil {
		t.Skipf("needs root: %v", err)
	}
	ns := createScratchNS(t, "dtut-off")
	tap := createScratchTap(t, ns, "off0")

	var before, during, after *FeatureSnapshot
	err := inSwitchNetns(ns, func() (err error) {
		before, err = GetFeatures(tap, ethtoolRetries)
		return err
	})
	if err != nil {
		t.Fatalf("GetFeatures: %v", err)
	}
	snap, err := SuppressOffloadsInNS(ns, tap)
	if err != nil {
		t.Fatalf("Suppress: %v", err)
	}
	err = inSwitchNetns(ns, func() (err error) { during, err = GetFeatures(tap, 1); return err })
	if err != nil {
		t.Fatal(err)
	}
	if len(before.changedBits(during)) == 0 {
		t.Fatalf("suppress did not change any feature on a fresh tap")
	}
	if err := RestoreFeaturesInNS(ns, tap, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	err = inSwitchNetns(ns, func() (err error) { after, err = GetFeatures(tap, 1); return err })
	if err != nil {
		t.Fatal(err)
	}
	if diff := before.changedBits(after); len(diff) != 0 {
		t.Fatalf("features not restored: %v", diff)
	}
}
