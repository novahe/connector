// Package debug implements the VM-less sandbox network debugging session
// used by `connector-ctl vswitch debug`: a disposable netns whose kernel
// network stack is bridged, frame by frame, to an allocated vswitch TAP port.
package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ethtool ioctl command numbers and constants from linux/ethtool.h (stable UAPI).
const (
	ethtoolGFeatures = 0x0000003a // ETHTOOL_GFEATURES
	ethtoolSFeatures = 0x0000003b // ETHTOOL_SFEATURES
	ethtoolGStrings  = 0x0000001b // ETHTOOL_GSTRINGS
	ethtoolGSsetInfo = 0x00000037 // ETHTOOL_GSSET_INFO
	ethSSFeatures    = 4          // ETH_SS_FEATURES
	gstringLen       = 32         // ETH_GSTRING_LEN
	siocEthtool      = 0x8946     // SIOCETHTOOL
	ifnamesz         = 16         // IFNAMSIZ
	ifreqSize        = 40         // sizeof(struct ifreq)
	maxFeatureWords  = 64         // 2048 feature bits, far above any kernel today
	ethtoolRetries   = 3
)

// featureWord is one 32-bit block of struct ethtool_get_features_block.
type featureWord struct {
	Available   uint32 `json:"available"`
	Requested   uint32 `json:"requested"`
	Active      uint32 `json:"active"`
	NeverChange uint32 `json:"never_changed"`
}

// FeatureSnapshot is a full `ethtool -k` equivalent state of one netdev,
// captured bit by bit. Names are stored for human-readable diagnostics;
// restore only relies on the bit words.
type FeatureSnapshot struct {
	Words []featureWord `json:"words"`
	Names []string      `json:"names,omitempty"`
}

// putU32/getU32 read/write native-endian 32-bit values, matching the kernel
// UAPI structs the ioctl payloads mirror.
func putU32(b []byte, v uint32) { *(*uint32)(unsafe.Pointer(&b[0])) = v }
func getU32(b []byte) uint32    { return *(*uint32)(unsafe.Pointer(&b[0])) }

// changeable returns the per-word mask of bits the kernel may modify.
func (s *FeatureSnapshot) changeable() []uint32 {
	out := make([]uint32, len(s.Words))
	for i, w := range s.Words {
		out[i] = w.Available &^ w.NeverChange
	}
	return out
}

// changedBits names every bit whose active or requested value differs between
// the two snapshots, within the changeable mask of the receiver.
func (s *FeatureSnapshot) changedBits(other *FeatureSnapshot) []string {
	mask := s.changeable()
	n := len(s.Words)
	if len(other.Words) < n {
		n = len(other.Words)
	}
	var out []string
	for w := 0; w < n; w++ {
		diff := ((s.Words[w].Active ^ other.Words[w].Active) |
			(s.Words[w].Requested ^ other.Words[w].Requested)) & mask[w]
		for b := 0; b < 32; b++ {
			if diff&(1<<uint(b)) == 0 {
				continue
			}
			idx := w*32 + b
			name := fmt.Sprintf("feature-bit-%d", idx)
			if idx < len(s.Names) && s.Names[idx] != "" {
				name = s.Names[idx]
			}
			out = append(out, name)
		}
	}
	return out
}

// suppressMask returns the bit mask of features that must be off while a
// no-vnet_hdr fd is attached to the TAP: without virtio metadata the fd
// cannot carry partial checksums or GSO segmentation, so all checksum and
// segmentation offloads must produce complete, MTU-sized frames. This is a
// superset of the shell tool's `ethtool -K tx off tso off gso off`.
func suppressMask(names []string) []uint32 {
	out := make([]uint32, (len(names)+31)/32)
	for i, n := range names {
		if containsFeatureKeyword(n) {
			out[i/32] |= 1 << uint(i%32)
		}
	}
	return out
}

func containsFeatureKeyword(name string) bool {
	return strContains(name, "checksum") || strContains(name, "segmentation")
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ioctlEthtool issues one SIOCETHTOOL ioctl with the given payload buffer.
// The payload buffer must remain referenced by the caller across this call.
func ioctlEthtool(ifname string, payload []byte) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("ethtool control socket: %w", err)
	}
	defer unix.Close(sock)

	req := make([]byte, ifreqSize)
	copy(req, ifname)
	*(*unsafe.Pointer)(unsafe.Pointer(&req[ifnamesz])) = unsafe.Pointer(&payload[0])
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(sock), siocEthtool, uintptr(unsafe.Pointer(&req[0])))
	runtime.KeepAlive(payload)
	runtime.KeepAlive(req)
	if errno != 0 {
		return errno
	}
	return nil
}

// featureNames asks the kernel for the ETH_SS_FEATURES name table.
func featureNames(ifname string, retries int) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		names, err := featureNamesOnce(ifname)
		if err == nil {
			return names, nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, lastErr
}

func featureNamesOnce(ifname string) ([]string, error) {
	// ETHTOOL_GSSET_INFO first: learn the string count for ETH_SS_FEATURES.
	info := make([]byte, 24) // cmd, reserved, sset_mask(u64), data[1]u32
	putU32(info[0:4], ethtoolGSsetInfo)
	*(*uint64)(unsafe.Pointer(&info[8])) = 1 << ethSSFeatures
	if err := ioctlEthtool(ifname, info); err != nil {
		return nil, fmt.Errorf("GSSET_INFO: %w", err)
	}
	count := int(getU32(info[16:20]))
	if count <= 0 || count > maxFeatureWords*32 {
		return nil, fmt.Errorf("implausible feature string count %d", count)
	}
	// ETHTOOL_GSTRINGS next: fetch the names themselves.
	buf := make([]byte, 12+count*gstringLen)
	putU32(buf[0:4], ethtoolGStrings)
	putU32(buf[4:8], ethSSFeatures)
	putU32(buf[8:12], uint32(count))
	if err := ioctlEthtool(ifname, buf); err != nil {
		return nil, fmt.Errorf("GSTRINGS: %w", err)
	}
	names := make([]string, count)
	for i := 0; i < count; i++ {
		b := buf[12+i*gstringLen : 12+(i+1)*gstringLen]
		end := 0
		for end < len(b) && b[end] != 0 {
			end++
		}
		names[i] = string(b[:end])
	}
	return names, nil
}

// GetFeatures reads the full feature state of one netdev.
func GetFeatures(ifname string, retries int) (*FeatureSnapshot, error) {
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		snap, err := getFeaturesOnce(ifname)
		if err == nil {
			return snap, nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, lastErr
}

func getFeaturesOnce(ifname string) (*FeatureSnapshot, error) {
	for words := 8; words <= maxFeatureWords; words *= 2 {
		buf := make([]byte, 8+words*16)
		putU32(buf[0:4], ethtoolGFeatures)
		putU32(buf[4:8], uint32(words))
		if err := ioctlEthtool(ifname, buf); err != nil {
			if err == unix.EINVAL || err == unix.EOVERFLOW {
				continue // buffer too small for this kernel; grow
			}
			return nil, err
		}
		if got := int(getU32(buf[4:8])); got > words {
			continue
		}
		snap := &FeatureSnapshot{Words: make([]featureWord, 0, words)}
		for w := 0; w < words; w++ {
			off := 8 + w*16
			snap.Words = append(snap.Words, featureWord{
				Available:   getU32(buf[off : off+4]),
				Requested:   getU32(buf[off+4 : off+8]),
				Active:      getU32(buf[off+8 : off+12]),
				NeverChange: getU32(buf[off+12 : off+16]),
			})
		}
		trimSnapshotWords(snap)
		return snap, nil
	}
	return nil, fmt.Errorf("feature word buffer too small even at %d words", maxFeatureWords)
}

// trimSnapshotWords drops all-zero trailing words so snapshots stay
// comparable across kernels with different feature counts.
func trimSnapshotWords(s *FeatureSnapshot) {
	last := -1
	for i, w := range s.Words {
		if w.Available|w.Requested|w.Active|w.NeverChange != 0 {
			last = i
		}
	}
	s.Words = s.Words[:last+1]
}

// setFeaturesRequested drives every changeable bit to the snapshot's
// requested value. valid is derived from the CURRENT state so only bits the
// kernel will actually honor are written.
func setFeaturesRequested(ifname string, snap *FeatureSnapshot) error {
	cur, err := getFeaturesOnce(ifname)
	if err != nil {
		return err
	}
	words := len(snap.Words)
	if len(cur.Words) < words {
		words = len(cur.Words)
	}
	if words == 0 {
		return fmt.Errorf("empty feature snapshot")
	}
	buf := make([]byte, 8+words*8)
	putU32(buf[0:4], ethtoolSFeatures)
	putU32(buf[4:8], uint32(words))
	for w := 0; w < words; w++ {
		off := 8 + w*8
		valid := cur.Words[w].Available &^ cur.Words[w].NeverChange
		putU32(buf[off:off+4], valid)
		putU32(buf[off+4:off+8], snap.Words[w].Requested&valid)
	}
	return ioctlEthtool(ifname, buf)
}

// RestoreFeatures drives the device back to the snapshot state and verifies
// it, mirroring the shell tool's per-bit restore + diff check (dependent bits
// such as tx-tcp-mangleid-segmentation included).
func RestoreFeatures(ifname string, snap *FeatureSnapshot, retries int) error {
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		switch err := func() error {
			if err := setFeaturesRequested(ifname, snap); err != nil {
				return err
			}
			now, err := getFeaturesOnce(ifname)
			if err != nil {
				return err
			}
			if diff := snap.changedBits(now); len(diff) != 0 {
				return fmt.Errorf("features still differ: %v", diff)
			}
			return nil
		}(); {
		case err == nil:
			return nil
		default:
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("offload restore incomplete for %s: %w", ifname, lastErr)
}

// SuppressOffloadsForBridge snapshots the device state and turns off every
// checksum/segmentation offload, so frames crossing a no-vnet_hdr TAP fd
// always carry complete checksums at MTU size. The snapshot (with names) is
// the exact state RestoreFeatures will drive back to.
func SuppressOffloadsForBridge(ifname string) (*FeatureSnapshot, error) {
	snap, err := SnapshotOffloads(ifname)
	if err != nil {
		return nil, err
	}
	return snap, ApplySuppressedOffloads(ifname, snap)
}

// SnapshotOffloads reads everything needed for a later full restoration.
// Callers must persist this value and mark their session dirty before applying it.
func SnapshotOffloads(ifname string) (*FeatureSnapshot, error) {
	snap, err := GetFeatures(ifname, ethtoolRetries)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", ifname, err)
	}
	if len(snap.Names) == 0 {
		names, err := featureNames(ifname, ethtoolRetries)
		if err != nil {
			return nil, fmt.Errorf("feature names %s: %w", ifname, err)
		}
		snap.Names = names
	}
	return snap, nil
}

// ApplySuppressedOffloads changes the TAP only after its snapshot is durable.
func ApplySuppressedOffloads(ifname string, snap *FeatureSnapshot) error {
	if snap == nil || len(snap.Words) == 0 || len(snap.Names) == 0 {
		return fmt.Errorf("missing offload snapshot")
	}
	mask := suppressMask(snap.Names)
	words := len(snap.Words)
	buf := make([]byte, 8+words*8)
	putU32(buf[0:4], ethtoolSFeatures)
	putU32(buf[4:8], uint32(words))
	for w := 0; w < words; w++ {
		off := 8 + w*8
		valid := snap.Words[w].Available &^ snap.Words[w].NeverChange
		var offMask uint32
		if w < len(mask) {
			offMask = mask[w]
		}
		putU32(buf[off:off+4], valid)
		putU32(buf[off+4:off+8], snap.Words[w].Requested&valid&^offMask)
	}
	if err := ioctlEthtool(ifname, buf); err != nil {
		return fmt.Errorf("disable offloads %s: %w", ifname, err)
	}
	return nil
}

// SaveSnapshotFile / LoadSnapshotFile persist the snapshot beside the session.
func SaveSnapshotFile(path string, snap *FeatureSnapshot) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

func LoadSnapshotFile(path string) (*FeatureSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap FeatureSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
