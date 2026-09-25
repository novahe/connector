package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupRetainsNamespaceWithoutRecordedInode(t *testing.T) {
	rootGate(t)
	root, err := OpenStateRoot(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := root.NewSessionDir(&SessionState{Phase: "preparing"})
	if err != nil {
		t.Fatal(err)
	}
	name := "sandbox_debug_ns_" + strings.TrimPrefix(filepath.Base(sess.Dir), "session.")
	if _, err := CreateDebugNS(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteDebugNS(name) })
	st, err := sess.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.DebugNS = name // simulate exit before DebugNSIno was saved
	if err := sess.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := CleanupSession(root, sess.Dir, CleanupOptions{}); err == nil {
		t.Fatal("cleanup accepted namespace with no recorded identity")
	}
	if _, err := NetnsIno(name); err != nil {
		t.Fatalf("namespace removed: %v", err)
	}
	if _, err := os.Stat(sess.Dir); err != nil {
		t.Fatalf("session lost: %v", err)
	}
}

func TestPristineNamespaceRejectsLiveTapHolder(t *testing.T) {
	rootGate(t)
	ns := createScratchNS(t, "not-pristine")
	ino, err := NetnsIno(ns)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := PristineDebugNS(ns, ino); err != nil || !ok {
		t.Fatalf("fresh namespace: pristine=%v err=%v", ok, err)
	}
	if ok, err := PristineDebugNS(ns, "0:0"); err != nil || ok {
		t.Fatalf("wrong namespace identity: pristine=%v err=%v", ok, err)
	}
	createScratchTap(t, ns, "busy0")
	if ok, err := PristineDebugNS(ns, ino); err != nil || ok {
		t.Fatalf("occupied namespace: pristine=%v err=%v", ok, err)
	}
}
